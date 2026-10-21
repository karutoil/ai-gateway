package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/translate"
	"ai-gateway/internal/videogen"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// DefaultVideoTTFB is the client-facing first-byte budget for the video
// *create* call. The create itself is fast (it only enqueues work upstream),
// but provider queues can be slow to acknowledge, so it gets a more generous
// budget than chat while staying well inside an edge proxy's window.
//
// Polls and content downloads are NOT governed by this budget: a poll is a
// read of the normal handler budget (Timeouts.TTFB) and a content download gets
// a first-byte budget plus a per-chunk idle watchdog (see
// streamVideoContent). Pre-fix this constant was dead code — no video route was
// bounded at all, so a silent provider could hold a client connection (and a
// goroutine) for the whole upstream-header timeout on every route.
const DefaultVideoTTFB = 120 * time.Second

// MaxVideoCreateErrorBytes bounds the provider error body relayed to the client
// on a failed create. The full upstream body used to be copied verbatim.
const MaxVideoCreateErrorBytes = 4 << 10

// errVideoContentForeignOrigin marks a content URL that does not belong to the
// provider's own origin. Such a URL is never fetched server-side: the client is
// redirected to it instead (see streamVideoContent).
var errVideoContentForeignOrigin = errors.New("video content URL is not on the provider origin")

// videoJob is the persisted row backing an async generation job.
type videoJob struct {
	ID         string
	ProviderID string
	KeyPrefix  string
	Dialect    videogen.Dialect
	Model      string
	UpstreamID string
	Prompt     string
	Seconds    string
	Size       string
	Status     string
	Progress   int
	ErrCode    string
	ErrMessage string
	VideoURL   string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Completed  *time.Time
	Expires    *time.Time
}

// toVideo renders the OpenAI Video resource for a job row.
func (j *videoJob) toVideo() *videogen.Video {
	v := &videogen.Video{
		ID:        j.ID,
		Object:    "video",
		Model:     j.Model,
		Status:    j.Status,
		Progress:  clampProgress(j.Progress),
		CreatedAt: j.CreatedAt.Unix(),
		Size:      j.Size,
		Seconds:   j.Seconds,
		Prompt:    j.Prompt,
	}
	if j.Completed != nil {
		ts := j.Completed.Unix()
		v.CompletedAt = &ts
	}
	if j.Expires != nil {
		ts := j.Expires.Unix()
		v.ExpiresAt = &ts
	}
	if j.ErrMessage != "" || j.ErrCode != "" {
		v.Error = &videogen.VideoError{Code: j.ErrCode, Message: j.ErrMessage}
	}
	return v
}

// expired reports whether the job's artefact is gone: either the retention
// window the provider advertised has passed, or the job was already marked
// expired (a provider 404/410 on poll or content).
func (j *videoJob) expired(now time.Time) bool {
	if j.ErrCode == "expired" {
		return true
	}
	// Only a finished job has artefacts to lose, so only a completed job can
	// expire on time.
	return j.Status == videogen.StatusCompleted && j.Expires != nil && now.After(*j.Expires)
}

// VideoCreate handles POST /v1/videos — the standardized OpenAI-compatible
// video-generation endpoint.
//
// Video generation is asynchronous: this call enqueues work upstream and
// returns a job resource in state "queued" immediately. Clients poll
// GET /v1/videos/{id} until the job reaches a terminal state and then fetch
// the bytes from GET /v1/videos/{id}/content. A synchronous request is not an
// option — generation runs for minutes, far beyond any HTTP budget.
func (h *Handler) VideoCreate(w http.ResponseWriter, r *http.Request) {
	body, bodyOK := h.readProxyBody(w, r)
	if !bodyOK {
		return
	}
	r = r.WithContext(WithAttribution(r.Context(), attributionFromRequest(r, body)))
	rawModel := translate.ExtractModel(body)
	model := h.resolveAlias(rawModel)
	if !h.enforceModelAllowlist(w, r, rawModel, model) {
		return
	}
	if !h.checkTPM(w, r, body) {
		return
	}
	norm, err := videogen.ParseRequest(body)
	if err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	if h.rejectNonVideoModelOnVideos(w, model) {
		return
	}
	candidates, rule := h.videoCandidates(r, rawModel, model)
	if len(candidates) == 0 {
		h.videoNoRoute(w, model, rule)
		return
	}
	cand := candidates[0]
	p := cand.Provider
	// P1-5: the lb_rules member override is the model the upstream will run, so
	// it is also the model that must be billed and logged. Pre-fix the outer
	// (requested) model was captured here, so a rule with a model_override ran
	// one model and billed another.
	sentModel := model
	if override := cand.ModelOverrideForRequest(); strings.TrimSpace(override) != "" {
		sentModel = override
	}
	apiKey, derr := h.ProviderStore.DecryptKey(p)
	if derr != nil {
		httperr.Write(w, http.StatusBadGateway, "provider credential unavailable", httperr.TypeProxy)
		return
	}
	upstream := stripProviderPrefix(sentModel, p)
	dialect := videoDialectFor(p, upstream)
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")

	target, reqBody, cerr := videoCreateRequest(p, dialect, norm, upstream)
	if cerr != nil {
		httperr.Invalid(w, cerr.Error())
		return
	}
	// P1-8: bound the create call by the video first-byte budget. The watchdog
	// stays armed past the header read so the (buffered, small) body read is
	// covered too.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ttfb := newTTFBController(h.videoTTFB(), start)
	defer ttfb.stop()
	watchdog := armTTFBWatchdog(ttfb, cancel)
	respBody, status, cerr := h.doVideoUpstream(ctx, target, apiKey, reqBody, dialect)
	if watchdog != nil {
		watchdog.Stop()
	}
	if cerr != nil {
		log.Error().Err(cerr).Str("provider", p.Name).Str("model", sentModel).Msg("video create failed")
		h.observeVideoRoute("videos", http.StatusBadGateway, start, p.ID, sentModel)
		if ttfb.expired() {
			httperr.Proxy(w, http.StatusGatewayTimeout, "upstream is not responding (first-byte timeout)")
			return
		}
		httperr.Proxy(w, http.StatusBadGateway, "upstream video request failed")
		return
	}
	if status < 200 || status >= 300 {
		// Relay the upstream's verdict: a bad model name or an unsupported size
		// is the caller's error, and the provider's message is far more useful
		// than a generic envelope — but bounded and sanitized (P2-3), because
		// the raw body could be an HTML page, an unbounded dump, or carry
		// credential-shaped material from the provider.
		code, relay := videoUpstreamErrorBody(respBody)
		h.observeVideoRoute("videos", outboundStatus(status), start, p.ID, sentModel)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outboundStatus(status))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": relay, "type": "upstream_error", "code": code},
		})
		return
	}
	upstreamID := videoUpstreamID(respBody, dialect)
	if upstreamID == "" {
		log.Error().Str("provider", p.Name).Str("body", errSnippet(respBody)).Msg("video create returned no job handle")
		h.observeVideoRoute("videos", http.StatusBadGateway, start, p.ID, sentModel)
		httperr.Proxy(w, http.StatusBadGateway, "upstream did not return a video job handle")
		return
	}

	job := &videoJob{
		ID:         "video_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		ProviderID: p.ID,
		KeyPrefix:  keyPrefix,
		Dialect:    dialect,
		Model:      sentModel,
		UpstreamID: upstreamID,
		Prompt:     norm.Prompt,
		Seconds:    norm.Seconds,
		Size:       norm.Size,
		Status:     videogen.StatusQueued,
		CreatedAt:  time.Now().UTC(),
	}
	if err := h.insertVideoJob(job); err != nil {
		log.Error().Err(err).Msg("failed to persist video job")
		h.observeVideoRoute("videos", http.StatusInternalServerError, start, p.ID, sentModel)
		httperr.Proxy(w, http.StatusInternalServerError, "failed to record the video job")
		return
	}
	// Meter the enqueue immediately: video bills by generated duration, and the
	// cost is known from the requested seconds regardless of how polling ends.
	cost := h.videoCost(sentModel, norm.Seconds)
	h.recordUsage(keyPrefix, r, 0, cost)
	h.logRequestExtendedBodies(r.Context(), keyPrefix, p.ID, sentModel, "videos", http.StatusOK,
		time.Since(start).Milliseconds(), 0, 0, cost, false, body, respBody, &logMeta{})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(videogen.VideoToJSON(job.toVideo()))
}

// videoTTFB returns the effective first-byte budget for the video create call.
func (h *Handler) videoTTFB() time.Duration {
	if h.Timeouts.UpstreamHeader > 0 && h.Timeouts.UpstreamHeader < DefaultVideoTTFB {
		// Never exceed the configured upstream header budget: a watchdog that
		// fires after the transport already gave up is pointless.
		return h.Timeouts.UpstreamHeader
	}
	return DefaultVideoTTFB
}

// VideoGet handles GET /v1/videos/{id} — poll a job.
//
// A terminal job is served from the stored row (no upstream call). A live job
// is polled upstream, normalized, and the row updated, so repeated polls do
// not keep hitting the provider once it finishes.
func (h *Handler) VideoGet(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := chi.URLParam(r, "id")
	job, err := h.getVideoJob(id)
	if err != nil || job == nil {
		h.observeVideoRoute("videos.poll", http.StatusNotFound, start, "", "")
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	if !h.videoJobVisible(r, job) {
		h.observeVideoRoute("videos.poll", http.StatusNotFound, start, job.ProviderID, job.Model)
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	// P1-6: an expired job is GONE, not a healthy completed job. The row is
	// marked locally first so the next poll does not re-hit the provider.
	if job.expired(time.Now().UTC()) {
		h.expireVideoJob(job)
		h.observeVideoRoute("videos.poll", http.StatusGone, start, job.ProviderID, job.Model)
		writeVideoGone(w, job)
		return
	}
	if videogen.IsTerminal(job.Status) {
		h.observeVideoRoute("videos.poll", http.StatusOK, start, job.ProviderID, job.Model)
		writeVideoJSON(w, job.toVideo())
		return
	}
	p, perr := h.ProviderStore.GetByID(job.ProviderID)
	if perr != nil || p == nil {
		h.observeVideoRoute("videos.poll", http.StatusBadGateway, start, "", job.Model)
		httperr.Proxy(w, http.StatusBadGateway, "the provider that owns this video job no longer exists")
		return
	}
	// P2-1: never decrypt (or use) a provider the requesting key's org may not
	// touch. A provider reassigned to another org keeps serving stale jobs
	// otherwise.
	if !orgAllows(h.requestKeyOrg(r), p) {
		h.observeVideoRoute("videos.poll", http.StatusNotFound, start, job.ProviderID, job.Model)
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	apiKey, derr := h.ProviderStore.DecryptKey(p)
	if derr != nil {
		h.observeVideoRoute("videos.poll", http.StatusBadGateway, start, job.ProviderID, job.Model)
		httperr.Write(w, http.StatusBadGateway, "provider credential unavailable", httperr.TypeProxy)
		return
	}
	updated, uerr := h.pollVideoJobBounded(r, job, p, apiKey)
	if uerr != nil {
		// A transient upstream poll failure must not fail the client's poll:
		// report the last known state instead of an error.
		log.Warn().Err(uerr).Str("job", job.ID).Msg("video poll failed; serving last known state")
		h.observeVideoRoute("videos.poll", http.StatusOK, start, job.ProviderID, job.Model)
		writeVideoJSON(w, job.toVideo())
		return
	}
	h.observeVideoRoute("videos.poll", http.StatusOK, start, job.ProviderID, job.Model)
	writeVideoJSON(w, updated.toVideo())
}

// VideoContent handles GET /v1/videos/{id}/content — stream the finished bytes.
func (h *Handler) VideoContent(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := chi.URLParam(r, "id")
	job, err := h.getVideoJob(id)
	if err != nil || job == nil || !h.videoJobVisible(r, job) {
		h.observeVideoRoute("videos.content", http.StatusNotFound, start, "", "")
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	// P1-6: expiry is a local, terminal fact. Answer 410 and stop hitting the
	// provider for an artefact it no longer retains.
	if job.expired(time.Now().UTC()) {
		h.expireVideoJob(job)
		h.observeVideoRoute("videos.content", http.StatusGone, start, job.ProviderID, job.Model)
		writeVideoGone(w, job)
		return
	}
	if !videogen.IsTerminal(job.Status) {
		h.observeVideoRoute("videos.content", http.StatusConflict, start, job.ProviderID, job.Model)
		httperr.Write(w, http.StatusConflict, "the video is not ready yet (status: "+job.Status+"); poll GET /v1/videos/"+id, httperr.TypeInvalid)
		return
	}
	if job.Status == videogen.StatusFailed {
		msg := job.ErrMessage
		if msg == "" {
			msg = "the video generation failed"
		}
		h.observeVideoRoute("videos.content", http.StatusConflict, start, job.ProviderID, job.Model)
		httperr.Write(w, http.StatusConflict, msg, httperr.TypeInvalid)
		return
	}
	p, perr := h.ProviderStore.GetByID(job.ProviderID)
	if perr != nil || p == nil {
		h.observeVideoRoute("videos.content", http.StatusBadGateway, start, "", job.Model)
		httperr.Proxy(w, http.StatusBadGateway, "the provider that owns this video job no longer exists")
		return
	}
	if !orgAllows(h.requestKeyOrg(r), p) {
		h.observeVideoRoute("videos.content", http.StatusNotFound, start, job.ProviderID, job.Model)
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	apiKey, derr := h.ProviderStore.DecryptKey(p)
	if derr != nil {
		h.observeVideoRoute("videos.content", http.StatusBadGateway, start, job.ProviderID, job.Model)
		httperr.Write(w, http.StatusBadGateway, "provider credential unavailable", httperr.TypeProxy)
		return
	}
	contentURL, cerr := h.videoContentURL(r, job, p, apiKey)
	if cerr != nil || contentURL == "" {
		log.Error().Err(cerr).Str("job", job.ID).Msg("could not resolve video content URL")
		h.observeVideoRoute("videos.content", http.StatusBadGateway, start, job.ProviderID, job.Model)
		httperr.Proxy(w, http.StatusBadGateway, "could not resolve the generated video")
		return
	}
	status := h.streamVideoContent(w, r, contentURL, apiKey, p, job)
	h.observeVideoRoute("videos.content", status, start, job.ProviderID, job.Model)
}

// VideoList handles GET /v1/videos.
//
// Pagination (P3-7): the list used to be a hard `LIMIT 100` with no way to page
// past it, so a key with more than 100 jobs silently lost access to the rest.
// It now honours the OpenAI list convention — `limit` (1..100) and `after` (a
// job id cursor) — and reports `has_more` plus the trailing `last_id` so a
// client can walk the whole set. Ordering is created_at DESC, so `after` means
// "older than that job".
func (h *Handler) VideoList(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	keyPrefix := r.Header.Get("X-Gateway-Key-Prefix")

	const (
		defaultLimit = 20
		maxLimit     = 100
	)
	limit := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			h.observeVideoRoute("videos.list", http.StatusBadRequest, start, "", "")
			httperr.Invalid(w, "limit must be a positive integer (max 100)")
			return
		}
		if n > maxLimit {
			n = maxLimit
		}
		limit = n
	}

	// Fetch one extra row to learn whether another page exists without a
	// separate COUNT (which would scan the key's whole history).
	fetch := limit + 1
	var (
		rows  *sql.Rows
		err   error
		after = strings.TrimSpace(r.URL.Query().Get("after"))
	)
	base := `SELECT id, provider_id, COALESCE(key_prefix,''), dialect, model, upstream_id, COALESCE(prompt,''),
		COALESCE(seconds,''), COALESCE(size,''), status, COALESCE(progress,0), COALESCE(error_code,''),
		COALESCE(error_message,''), COALESCE(video_url,''), created_at, updated_at, completed_at, expires_at
		FROM video_jobs WHERE key_prefix=?`
	if after != "" {
		// The cursor is the anchor job's own (created_at, id), compared as a
		// ROW VALUE inside SQL and never round-tripped through Go.
		//
		// Round-tripping is the trap: created_at is a TEXT column holding
		// RFC3339Nano ('2026-10-17T22:51:58.7Z'), while rows written by SQL
		// datetime('now') hold the same instant with a SPACE
		// ('2026-10-17 22:51:58'). Binding a Go time.Time re-serializes it with
		// 'T', so `created_at = ?` never matches those rows and the anchor row
		// is re-emitted as the first item of the next page. Comparing the
		// stored text to the stored text avoids the mismatch entirely and works
		// on both dialects.
		var anchorExists int
		if aerr := h.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM video_jobs WHERE id=? AND key_prefix=?`), after, keyPrefix).Scan(&anchorExists); aerr != nil || anchorExists == 0 {
			h.observeVideoRoute("videos.list", http.StatusBadRequest, start, "", "")
			httperr.Invalid(w, "after must be the id of one of this key's video jobs")
			return
		}
		rows, err = h.DB.Query(db.Q(base+` AND (created_at, id) < ((SELECT created_at FROM video_jobs WHERE id=?), ?) ORDER BY created_at DESC, id DESC LIMIT ?`),
			keyPrefix, after, after, fetch)
	} else {
		rows, err = h.DB.Query(db.Q(base+` ORDER BY created_at DESC, id DESC LIMIT ?`), keyPrefix, fetch)
	}
	if err != nil {
		h.observeVideoRoute("videos.list", http.StatusInternalServerError, start, "", "")
		httperr.Write(w, http.StatusInternalServerError, "could not list video jobs", httperr.TypeProxy)
		return
	}
	defer rows.Close()
	// Read the look-ahead row too, then decide: collecting first (rather than
	// breaking mid-iteration) is what makes has_more correct — breaking out of
	// the loop leaves the cursor ON the look-ahead row, and the next rows.Next()
	// would step past it and report "no more" even when more exist.
	all := []*videogen.Video{}
	for rows.Next() {
		j, serr := scanVideoJob(rows)
		if serr != nil {
			continue
		}
		all = append(all, j.toVideo())
	}
	if err := rows.Err(); err != nil {
		h.observeVideoRoute("videos.list", http.StatusInternalServerError, start, "", "")
		httperr.Write(w, http.StatusInternalServerError, "could not list video jobs", httperr.TypeProxy)
		return
	}
	hasMore := len(all) > limit
	if hasMore {
		all = all[:limit]
	}
	lastID := ""
	if n := len(all); n > 0 {
		lastID = all[n-1].ID
	}
	h.observeVideoRoute("videos.list", http.StatusOK, start, "", "")
	w.Header().Set("Content-Type", "application/json")
	out := all
	if out == nil {
		out = []*videogen.Video{}
	}
	resp := map[string]any{"object": "list", "data": out, "has_more": hasMore}
	if lastID != "" {
		resp["last_id"] = lastID
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// VideoDelete handles DELETE /v1/videos/{id}. It forgets the job locally; the
// provider's own copy (and its expiry) is left alone, since not every dialect
// exposes a cancel.
func (h *Handler) VideoDelete(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := chi.URLParam(r, "id")
	job, err := h.getVideoJob(id)
	if err != nil || job == nil || !h.videoJobVisible(r, job) {
		h.observeVideoRoute("videos.delete", http.StatusNotFound, start, "", "")
		httperr.NotFound(w, "no such video job: "+id)
		return
	}
	if _, derr := h.DB.Exec(db.Q(`DELETE FROM video_jobs WHERE id=?`), id); derr != nil {
		h.observeVideoRoute("videos.delete", http.StatusInternalServerError, start, job.ProviderID, job.Model)
		httperr.Write(w, http.StatusInternalServerError, "could not delete the video job", httperr.TypeProxy)
		return
	}
	h.observeVideoRoute("videos.delete", http.StatusOK, start, job.ProviderID, job.Model)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "video.deleted", "deleted": true})
}

/* ------------------------------------------------------------------ */
/* helpers                                                             */
/* ------------------------------------------------------------------ */

// observeVideoRoute emits the standard request/latency instruments for a video
// route (P2-6). Polls, list, content and delete previously emitted nothing at
// all: the read half of the video API was invisible in Prometheus. It does NOT
// write a request_logs row — a poll loop would otherwise fill the table with
// rows that carry no tokens and no cost; the metrics are the operability signal
// here, and create still logs a full row.
func (h *Handler) observeVideoRoute(endpoint string, status int, start time.Time, providerID, model string) {
	if h == nil || h.Metrics == nil {
		return
	}
	h.Metrics.IncRequests(providerID, model, endpoint, status)
	h.Metrics.ObserveLatency(providerID, endpoint, time.Since(start))
}

// videoCandidates resolves routing for a video request: any provider except a
// decision one (a System One endpoint cannot generate video).
func (h *Handler) videoCandidates(r *http.Request, rawModel, model string) ([]lb.Candidate, *lb.Rule) {
	providerHint := r.Header.Get("X-Provider")
	return h.candidateProvidersWithRuleContext(r.Context(), rawModel, model, providerHint, h.requestKeyOrg(r), func(p *models.Provider) bool {
		return !isDecisionProvider(p)
	})
}

// videoNoRoute writes the unroutable error for a video request.
func (h *Handler) videoNoRoute(w http.ResponseWriter, model string, rule *lb.Rule) {
	if rule != nil {
		httperr.Proxy(w, http.StatusServiceUnavailable, "no routing member is below its OAuth quota threshold")
		return
	}
	if h.unroutedModel() {
		noRouteFor(w, model)
		return
	}
	httperr.Proxy(w, http.StatusServiceUnavailable, "no video-capable provider configured for this model")
}

// rejectNonVideoModelOnVideos refuses models positively known NOT to be video
// models, while forwarding unknown ids (resellers expose video models the
// gateway cannot enumerate).
func (h *Handler) rejectNonVideoModelOnVideos(w http.ResponseWriter, model string) bool {
	if videogen.IsVideoModel(model) || h.videoModelKnown(model) {
		return false
	}
	if h.modelIsImageOnly(model) || h.imageModelKnown(model) {
		httperr.Invalid(w, "model '"+model+"' is an image model, not a video model; POST /v1/videos requires a video model (e.g. sora-2, veo-3.1-generate-preview, grok-imagine-video)")
		return true
	}
	// A decision model on the video endpoint gets the actionable redirect to
	// /v1/systemone rather than an opaque "no video-capable provider".
	if h.modelIsDecisionModel(model) {
		httperr.Invalid(w, "model '"+model+"' is a System One / decision model, not a video model; POST /v1/systemone is the decision endpoint")
		return true
	}
	if cm := h.catalogRow(model); cm != nil && cm.Modalities != "" {
		if !videogen.VideoModality(cm.Modalities) {
			httperr.Invalid(w, "model '"+model+"' is not a video model; POST /v1/videos requires a video model")
			return true
		}
	}
	return false
}

// videoDialectFor picks the upstream dialect for a provider + model.
//
// The PROVIDER decides the wire protocol, not the model id. A reseller typed
// openai/openai_compatible/azure exposes the standardized OpenAI Videos API
// (POST {base}/videos) for every video model it carries — including vendor ids
// it proxies (veo-3.1-fast, grok-imagine-video-1.5). Consulting the curated
// registry first sent Google's :predictLongRunning to an OpenAI-shaped reseller,
// which answered 400 "video model must be called through the video endpoint",
// and xAI's /videos/generations, which answered 404. Every veo and grok model on
// such a provider was therefore unusable.
//
// The registry's Dialect is only the right answer when nothing classifies the
// provider (an untyped legacy row): it records which VENDOR's native API a model
// belongs to, which says nothing about who is serving it here.
func videoDialectFor(p *models.Provider, model string) videogen.Dialect {
	if p == nil {
		return videogen.DialectOpenAI
	}
	switch p.Type {
	case models.ProviderGoogle:
		return videogen.DialectVeo
	case models.ProviderXAI:
		return videogen.DialectXAI
	case models.ProviderOpenAI, models.ProviderAzure, models.ProviderOpenAICompatible:
		// OpenAI-shaped: the standardized Videos API, whatever vendor the model
		// id names. Never fall through to the registry here.
		return videogen.DialectOpenAI
	}
	// Unclassified provider type: the curated registry is the only evidence.
	if m, ok := videogen.Lookup(model); ok {
		return m.Dialect
	}
	return videogen.DialectOpenAI
}

// videoCreateRequest builds the upstream URL and body for a create call.
func videoCreateRequest(p *models.Provider, dialect videogen.Dialect, n *videogen.Normalized, model string) (string, []byte, error) {
	// The body must carry the model id actually being sent upstream, never the
	// alias or the requested id: P2-4 was exactly that gap for the xAI dialect
	// (the stripped/override id was computed and then dropped).
	sent := *n
	sent.Model = model
	switch dialect {
	case videogen.DialectXAI:
		body, err := videogen.XAICreateBody(&sent)
		if err != nil {
			return "", nil, err
		}
		return strings.TrimRight(p.BaseURL, "/") + "/videos/generations", body, nil
	case videogen.DialectVeo:
		if verr := validateGoogleModelID(model); verr != nil {
			return "", nil, &videogen.ValidateError{Field: "model", Msg: verr.Error()}
		}
		body, err := videogen.VeoCreateBody(&sent)
		if err != nil {
			return "", nil, err
		}
		return googleVeoTarget(p.BaseURL, model), body, nil
	default:
		out, err := json.Marshal(map[string]any{
			"model": model, "prompt": n.Prompt,
			"seconds": n.Seconds, "size": n.Size,
		})
		if err != nil {
			return "", nil, err
		}
		if len(n.InputReference) > 0 {
			var m map[string]any
			if json.Unmarshal(out, &m) == nil {
				m["input_reference"] = json.RawMessage(n.InputReference)
				if b, e := json.Marshal(m); e == nil {
					out = b
				}
			}
		}
		return upstreamTarget(p.BaseURL, "/videos", model, p.Type == models.ProviderAzure), out, nil
	}
}

// googleVeoTarget builds the Veo predictLongRunning URL:
//
//	{base}/v1beta/models/{model}:predictLongRunning
//
// The model id is path-escaped (P3-2): it is client-influenced and was
// concatenated raw into the upstream URL.
func googleVeoTarget(baseURL, model string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	b = strings.TrimSuffix(b, "/models")
	for _, suffix := range []string{"/v1beta", "/v1"} {
		b = strings.TrimSuffix(b, suffix)
	}
	return b + "/v1beta/models/" + url.PathEscape(model) + ":predictLongRunning"
}

// googleVeoOperationURL builds the operation poll URL. Google returns an
// operation name like "models/veo-3.1/models/.../operations/xyz" (already
// version-relative), so it is appended to the versioned base. Every path
// segment is escaped: the name comes from the provider, but it is persisted and
// replayed, so it must not be able to introduce structure.
func googleVeoOperationURL(baseURL, operationName string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	b = strings.TrimSuffix(b, "/models")
	for _, suffix := range []string{"/v1beta", "/v1"} {
		b = strings.TrimSuffix(b, suffix)
	}
	segs := strings.Split(strings.TrimPrefix(strings.TrimSpace(operationName), "/"), "/")
	escaped := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" {
			continue
		}
		escaped = append(escaped, url.PathEscape(s))
	}
	return b + "/v1beta/" + strings.Join(escaped, "/")
}

// videoUpstreamID extracts the provider's job handle from a create response.
func videoUpstreamID(body []byte, dialect videogen.Dialect) string {
	switch dialect {
	case videogen.DialectXAI:
		return videogen.XAIRequestID(body)
	case videogen.DialectVeo:
		return videogen.VeoOperationName(body)
	default:
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &v) != nil {
			return ""
		}
		return v.ID
	}
}

// doVideoUpstream performs one upstream JSON request, returning the body and
// status. The API key placement follows the provider convention (Google uses
// x-goog-api-key, everything else Bearer).
func (h *Handler) doVideoUpstream(ctx context.Context, target, apiKey string, body []byte, dialect videogen.Dialect) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if isGoogleNativeAPI(target) {
		req.Header.Set("x-goog-api-key", apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, nil
}

// videoUpstreamErrorBody bounds and sanitizes a provider error body before it is
// relayed to the client (P2-3). The raw body used to be copied verbatim: an
// unbounded provider dump, an HTML error page, or credential-shaped material
// from the reseller's own envelope then landed in the client's response.
//
// A recognizable JSON error keeps its message/type/code (the provider's wording
// is the most useful part of the failure); anything else is replaced with a
// bounded, control-character-free snippet.
func videoUpstreamErrorBody(body []byte) (code, message string) {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &env) == nil && len(env.Error) > 0 {
		var eo struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &eo) == nil {
			if msg := sanitizeVideoErrorText(eo.Message); msg != "" {
				c := sanitizeVideoErrorText(eo.Type)
				if c == "" {
					c = sanitizeVideoErrorText(eo.Status)
				}
				if c == "" {
					c = fmt.Sprintf("%v", eo.Code)
					c = sanitizeVideoErrorText(c)
				}
				return c, msg
			}
		}
		var es string
		if json.Unmarshal(env.Error, &es) == nil {
			if msg := sanitizeVideoErrorText(es); msg != "" {
				return "", msg
			}
		}
	}
	snippet := sanitizeVideoErrorText(errSnippet(body))
	if snippet == "" {
		snippet = "the provider rejected the video request"
	}
	return "", snippet
}

// sanitizeVideoErrorText strips control characters, scrubs credential-shaped
// material, and bounds the length of provider-supplied error text.
func sanitizeVideoErrorText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	s = ScrubSecrets(s)
	if len(s) > MaxVideoCreateErrorBytes {
		s = s[:MaxVideoCreateErrorBytes] + "…"
	}
	return s
}

// pollVideoJobBounded polls the provider once under the normal handler
// first-byte budget, so a silent provider cannot hold the poll connection open
// for the whole upstream-header timeout.
func (h *Handler) pollVideoJobBounded(r *http.Request, job *videoJob, p *models.Provider, apiKey string) (*videoJob, error) {
	start := time.Now()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ttfb := newTTFBController(h.Timeouts.TTFB, start)
	defer ttfb.stop()
	watchdog := armTTFBWatchdog(ttfb, cancel)
	updated, err := h.pollVideoJob(ctx, job, p, apiKey)
	if watchdog != nil {
		watchdog.Stop()
	}
	if err != nil && ttfb.expired() {
		return nil, fmt.Errorf("poll timed out after %s", h.Timeouts.TTFB)
	}
	return updated, err
}

// pollVideoJob queries the provider once and persists the normalized state.
func (h *Handler) pollVideoJob(ctx context.Context, job *videoJob, p *models.Provider, apiKey string) (*videoJob, error) {
	var target string
	switch job.Dialect {
	case videogen.DialectXAI:
		target = strings.TrimRight(p.BaseURL, "/") + "/videos/" + url.PathEscape(job.UpstreamID)
	case videogen.DialectVeo:
		target = googleVeoOperationURL(p.BaseURL, job.UpstreamID)
	default:
		// Azure addresses jobs through its deployment shape
		// (/openai/deployments/<deployment>/videos/<id>?api-version=...) just
		// like every other Azure action. Building a bare {base}/videos/{id}
		// here meant an Azure video job could be CREATED (videoCreateRequest
		// uses upstreamTarget) and then never polled, so it stayed queued
		// forever from the client's point of view. upstreamTarget applies both
		// the deployment path and the api-version.
		target = upstreamTarget(p.BaseURL, "/videos/"+url.PathEscape(job.UpstreamID), job.Model, p.Type == models.ProviderAzure)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if isGoogleNativeAPI(target) {
		req.Header.Set("x-goog-api-key", apiKey)
	} else if isAzureTarget(target) {
		req.Header.Set("api-key", apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// P1-6: a provider that no longer knows the job is a TERMINAL fact. Looping
	// on a 404/410 kept re-hitting the provider forever while the client saw a
	// live job.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		h.failVideoJob(job, "expired", "the provider no longer retains this video (its retention window has passed)")
		return job, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poll returned status %d: %s", resp.StatusCode, errSnippet(body))
	}

	updated := h.normalizeVideoPoll(body, job)
	if updated == nil {
		return nil, fmt.Errorf("unrecognized poll response")
	}
	if h.applyVideoPollState(job, updated) {
		if err := h.updateVideoJob(job); err != nil {
			log.Warn().Err(err).Str("job", job.ID).Msg("failed to persist video poll state")
		}
	}
	return job, nil
}

// normalizedVideoPoll is the dialect-neutral result of one poll.
type normalizedVideoPoll struct {
	Status    string
	Progress  int
	VideoURL  string
	Completed *time.Time
	Expires   *time.Time
	Error     *videogen.VideoError
}

// applyVideoPollState merges one normalized poll into the stored job with the
// monotonic rules the API contract needs (P2-5):
//
//   - progress is clamped to [0,100] and never regresses;
//   - the status never regresses (queued < in_progress < terminal) and a
//     terminal job is never left;
//   - completed_at is stamped exactly ONCE, on the transition into a terminal
//     state — pre-fix it was synthesized from time.Now() on every poll, so it
//     drifted and rewrote the row each time.
//
// It reports whether anything changed (so a steady poll does not write the row).
func (h *Handler) applyVideoPollState(job *videoJob, upd *normalizedVideoPoll) bool {
	if job == nil || upd == nil {
		return false
	}
	changed := false
	now := time.Now().UTC()

	if !videogen.IsTerminal(job.Status) {
		status := upd.Status
		if status == "" {
			status = job.Status
		}
		if videoStatusRank(status) < videoStatusRank(job.Status) {
			status = job.Status // never regress (a flapping provider)
		}
		if status != job.Status {
			job.Status = status
			changed = true
		}
	}

	progress := clampProgress(upd.Progress)
	if progress < job.Progress {
		progress = job.Progress // never regress
	}
	if job.Status == videogen.StatusCompleted {
		progress = 100
	}
	if progress != job.Progress {
		job.Progress = progress
		changed = true
	}

	wasTerminal := job.Completed != nil
	if videogen.IsTerminal(job.Status) && !wasTerminal {
		stamp := now
		if upd.Completed != nil {
			stamp = upd.Completed.UTC()
		}
		job.Completed = &stamp
		changed = true
	}

	if upd.Expires != nil {
		if job.Expires == nil || !job.Expires.Equal(*upd.Expires) {
			exp := upd.Expires.UTC()
			job.Expires = &exp
			changed = true
		}
	}
	if upd.VideoURL != "" && upd.VideoURL != job.VideoURL {
		job.VideoURL = upd.VideoURL
		changed = true
	}
	if upd.Error != nil {
		if upd.Error.Code != job.ErrCode || upd.Error.Message != job.ErrMessage {
			job.ErrCode, job.ErrMessage = upd.Error.Code, upd.Error.Message
			changed = true
		}
	}
	if changed {
		job.UpdatedAt = now
	}
	return changed
}

// videoStatusRank orders the lifecycle states for the monotonicity rule.
func videoStatusRank(status string) int {
	switch status {
	case videogen.StatusQueued:
		return 0
	case videogen.StatusInProgress:
		return 1
	case videogen.StatusCompleted, videogen.StatusFailed:
		return 2
	}
	return 0
}

// clampProgress bounds a provider-reported percentage.
func clampProgress(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// failVideoJob moves a job to a terminal failed state locally and persists it,
// so it stops re-hitting the provider.
func (h *Handler) failVideoJob(job *videoJob, code, message string) {
	if job == nil {
		return
	}
	if videogen.IsTerminal(job.Status) && job.ErrCode == code && job.ErrMessage == message {
		return // already recorded; do not rewrite the row on every request
	}
	now := time.Now().UTC()
	job.Status = videogen.StatusFailed
	job.ErrCode, job.ErrMessage = code, message
	job.VideoURL = ""
	if job.Completed == nil {
		job.Completed = &now
	}
	job.UpdatedAt = now
	if err := h.updateVideoJob(job); err != nil {
		log.Warn().Err(err).Str("job", job.ID).Msg("failed to persist terminal video state")
	}
}

// expireVideoJob records the expiry locally: the job becomes failed/expired so
// nothing re-polls or re-downloads it, and the caller answers 410 Gone.
func (h *Handler) expireVideoJob(job *videoJob) {
	if job == nil || job.ErrCode == "expired" {
		return
	}
	when := "unknown"
	if job.Expires != nil {
		when = job.Expires.UTC().Format(time.RFC3339)
	}
	h.failVideoJob(job, "expired", "the generated video expired at "+when+"; provider artefacts are retained per the provider's policy")
}

// writeVideoGone answers 410 for an artefact the provider no longer retains.
func writeVideoGone(w http.ResponseWriter, job *videoJob) {
	msg := "the generated video has expired"
	if job != nil && job.ErrMessage != "" {
		msg = job.ErrMessage
	}
	httperr.Write(w, http.StatusGone, msg, httperr.TypeInvalid)
}

// normalizeVideoPoll converts a provider poll response into the neutral shape.
func (h *Handler) normalizeVideoPoll(body []byte, job *videoJob) *normalizedVideoPoll {
	switch job.Dialect {
	case videogen.DialectXAI:
		v := videogen.XAIPollToVideo(body, job.ID, job.Model, job.Prompt, job.Seconds, job.Size)
		if v == nil {
			return nil
		}
		out := &normalizedVideoPoll{Status: v.Status, Progress: v.Progress, VideoURL: videogen.XAIVideoURL(body), Error: v.Error}
		out.Completed = unixPtr(v.CompletedAt)
		return out
	case videogen.DialectVeo:
		v := videogen.VeoPollToVideo(body, job.ID, job.Model, job.Prompt, job.Seconds, job.Size)
		if v == nil {
			return nil
		}
		return &normalizedVideoPoll{
			Status:    v.Status,
			Progress:  v.Progress,
			VideoURL:  videogen.VeoVideoURI(body),
			Completed: unixPtr(v.CompletedAt),
			Error:     v.Error,
		}
	default:
		v, err := videogen.ParseVideo(body)
		if err != nil {
			return nil
		}
		out := &normalizedVideoPoll{
			Status:   videogen.NormalizeStatus(v.Status),
			Progress: v.Progress,
			Error:    v.Error,
		}
		if v.CompletedAt != nil {
			t := time.Unix(*v.CompletedAt, 0).UTC()
			out.Completed = &t
		}
		if v.ExpiresAt != nil {
			t := time.Unix(*v.ExpiresAt, 0).UTC()
			out.Expires = &t
		}
		if v.Status == videogen.StatusCompleted {
			// The OpenAI shape exposes the bytes through the content endpoint,
			// not a URL field, so content is fetched from the provider's own
			// content route.
			out.VideoURL = ""
		}
		return out
	}
}

func unixPtr(secs *int64) *time.Time {
	if secs == nil {
		return nil
	}
	t := time.Unix(*secs, 0).UTC()
	return &t
}

// videoContentURL resolves where to fetch the finished bytes from.
func (h *Handler) videoContentURL(r *http.Request, job *videoJob, p *models.Provider, apiKey string) (string, error) {
	if job.VideoURL != "" {
		return job.VideoURL, nil
	}
	// OpenAI-style: the provider exposes the bytes at its own content route.
	if job.Dialect == videogen.DialectOpenAI {
		return upstreamTarget(p.BaseURL, "/videos/"+url.PathEscape(job.UpstreamID)+"/content", job.Model, p.Type == models.ProviderAzure), nil
	}
	// xAI/Veo: the URL arrives with the poll response. If we do not have it,
	// poll once more to capture it.
	updated, err := h.pollVideoJobBounded(r, job, p, apiKey)
	if err != nil {
		return "", err
	}
	if updated.Status == videogen.StatusFailed {
		return "", fmt.Errorf("%s: %s", updated.ErrCode, updated.ErrMessage)
	}
	return updated.VideoURL, nil
}

// streamVideoContent relays the video bytes to the client and returns the
// client-facing status.
//
// P1-1 (security): the provider credential is attached ONLY when the content
// URL is the provider's own origin — same scheme AND same host. Pre-fix only
// the host was compared, so a provider-supplied "http://api.provider.com/…" or
// an empty-scheme "//api.provider.com/…" received the decrypted key (in
// cleartext, over http). A URL on any other origin is NOT fetched server-side at
// all: the client is redirected to it instead, which removes the
// authenticated-network SSRF read path entirely.
//
// P1-8: the download is bounded by a first-byte budget and then by a per-chunk
// idle watchdog, so a trickling provider cannot pin the connection and its
// goroutine indefinitely.
func (h *Handler) streamVideoContent(w http.ResponseWriter, r *http.Request, contentURL, apiKey string, p *models.Provider, job *videoJob) int {
	if !sameOrigin(contentURL, p.BaseURL) {
		// P1-1: never fetch a third-party URL server-side, and never hand it the
		// provider credential. Redirect the client instead.
		w.Header().Set("Location", contentURL)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
		return http.StatusFound
	}
	// P1-1 defense in depth: the one remaining server-side fetch is the
	// provider's own origin, so a private/loopback/link-local/metadata target is
	// legitimate only when the operator pointed the provider there too (homelab
	// vLLM and friends). Anything else is a resolved-to-internal-host escape.
	if !privateFetchPermitted(contentURL, p.BaseURL) {
		log.Warn().Str("job", job.ID).Str("url_host", urlHost(contentURL)).Msg("refusing a video content fetch that resolves into a private range")
		httperr.Proxy(w, http.StatusBadGateway, "refusing to fetch the video content from a private network address")
		return http.StatusBadGateway
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		httperr.Proxy(w, http.StatusBadGateway, "bad content URL")
		return http.StatusBadGateway
	}
	if isGoogleNativeAPI(contentURL) {
		req.Header.Set("x-goog-api-key", apiKey)
	} else if isAzureTarget(contentURL) {
		// Azure content URLs carry the deployment shape + api-version and
		// authenticate with api-key, not Bearer.
		req.Header.Set("api-key", apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// First-byte budget: the gateway stays silent until the provider's headers
	// arrive, so the same edge-proxy reasoning as the chat path applies.
	start := time.Now()
	ttfb := newTTFBController(h.contentTTFB(), start)
	defer ttfb.stop()
	watchdog := armTTFBWatchdog(ttfb, cancel)
	client := h.videoContentClient(p)
	resp, err := client.Do(req)
	if watchdog != nil {
		watchdog.Stop()
	}
	if err != nil {
		if ttfb.expired() {
			httperr.Proxy(w, http.StatusGatewayTimeout, "upstream is not responding (first-byte timeout)")
			return http.StatusGatewayTimeout
		}
		httperr.Proxy(w, http.StatusBadGateway, "could not download the generated video")
		return http.StatusBadGateway
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		// P1-6: the provider lost the artefact — terminate the job locally so
		// every subsequent request is answered from the row, not by re-hitting
		// the provider.
		h.failVideoJob(job, "expired", "the provider no longer retains this video (its retention window has passed)")
		writeVideoGone(w, job)
		return http.StatusGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httperr.Proxy(w, outboundStatus(resp.StatusCode), "the provider could not serve the video content")
		return outboundStatus(resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "video/mp4"
	}
	w.Header().Set("Content-Type", ct)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", job.ID+".mp4"))
	w.WriteHeader(http.StatusOK)
	if _, cerr := h.copyVideoBody(w, resp.Body); cerr != nil {
		log.Warn().Err(cerr).Str("job", job.ID).Bool("client_gone", r.Context().Err() != nil).Msg("video content relay ended early")
	}
	return http.StatusOK
}

// contentTTFB is the first-byte budget for a content download: the normal
// handler budget (a download is a read, not a generation).
func (h *Handler) contentTTFB() time.Duration {
	if h.Timeouts.TTFB > 0 {
		return h.Timeouts.TTFB
	}
	return DefaultTimeouts().TTFB
}

// videoContentClient returns the shared upstream client with a redirect policy
// that keeps the download on the provider's origin. Without it a same-origin
// provider URL could 302 the gateway into the private network — the redirect
// check runs per hop, before the credential-bearing request is re-issued.
func (h *Handler) videoContentClient(p *models.Provider) *http.Client {
	base := ""
	if p != nil {
		base = p.BaseURL
	}
	if h.Client == nil {
		return GatewayHTTPClient(NewGatewayTransport(h.Timeouts.UpstreamHeader, true))
	}
	c := *h.Client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("stopped after 3 redirects")
		}
		if req.URL == nil || base == "" {
			return errors.New("video content redirect without a provider origin")
		}
		if !sameOrigin(req.URL.String(), base) {
			return errVideoContentForeignOrigin
		}
		return nil
	}
	return &c
}

// copyVideoBody relays an upstream video body with a per-chunk idle watchdog.
// A provider that trickles bytes (or stops sending them without closing the
// connection) used to pin the handler goroutine until the client gave up; the
// watchdog bounds the gap between chunks, mirroring the streaming machinery in
// responses_stream.go (idleWatchdog).
func (h *Handler) copyVideoBody(w http.ResponseWriter, body io.Reader) (int64, error) {
	idle := h.Timeouts.StreamIdle
	wd := newIdleWatchdog(idle)
	defer wd.stop()

	flusher, _ := w.(http.Flusher)
	type chunkMsg struct {
		data []byte
		err  error
	}
	chunks := make(chan chunkMsg)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			buf := make([]byte, 32<<10)
			n, err := body.Read(buf)
			cm := chunkMsg{data: buf[:n], err: err}
			select {
			case chunks <- cm:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var total int64
	for {
		select {
		case <-wd.c:
			return total, fmt.Errorf("upstream video download stalled: no data within %s", idle)
		case cm := <-chunks:
			if len(cm.data) > 0 {
				n, werr := w.Write(cm.data)
				total += int64(n)
				if flusher != nil {
					flusher.Flush()
				}
				if werr != nil {
					return total, werr
				}
				wd.reset(idle)
			}
			if cm.err != nil {
				if cm.err == io.EOF {
					return total, nil
				}
				return total, cm.err
			}
			if total >= videogen.MaxContentBytes {
				return total, nil
			}
		}
	}
}

// urlHost returns the host of a URL for logging ("" when unparseable).
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// privateFetchPermitted reports whether the gateway may server-side fetch a
// content URL that is on the provider's own origin but whose host is (or
// resolves to) a private/loopback/link-local/metadata address.
//
// It is permitted when the provider's OWN base URL is private too: the operator
// deliberately pointed the gateway at an internal upstream. Otherwise the
// content host resolved somewhere the operator never configured, and the fetch
// is refused.
func privateFetchPermitted(contentURL, baseURL string) bool {
	contentHost := urlHost(contentURL)
	if contentHost == "" {
		return false
	}
	ch := hostOnly(contentHost)
	bh := hostOnly(urlHost(baseURL))
	if ch == "" {
		return false
	}
	// Fast path: an IP literal needs no DNS.
	if ip := net.ParseIP(ch); ip != nil {
		if !isPrivateFetchIP(ip) {
			return true
		}
		return bh == ch
	}
	contentPrivate := hostResolvesPrivate(ch)
	if !contentPrivate {
		return true
	}
	if bh == ch {
		return true // same host as the operator-configured provider
	}
	return hostResolvesPrivate(bh)
}

// hostOnly strips a port from a host:port pair and brackets from an IPv6
// literal.
func hostOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// hostResolvesPrivate reports whether EVERY address a host resolves to is in a
// private range (a host that does not resolve is not treated as private — the
// dial itself will fail).
func hostResolvesPrivate(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateFetchIP(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !isPrivateFetchIP(a.IP) {
			return false
		}
	}
	return true
}

// sameOrigin reports whether two URLs share BOTH scheme and host. Scheme
// equality is what keeps the provider credential off an http:// (cleartext) or
// scheme-less URL; pre-fix only the host was compared.
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(strings.TrimSpace(a))
	ub, err2 := url.Parse(strings.TrimSpace(b))
	if err1 != nil || err2 != nil {
		return false
	}
	if ua.Host == "" || ub.Host == "" {
		return false
	}
	if !strings.EqualFold(ua.Scheme, ub.Scheme) {
		return false
	}
	return strings.EqualFold(ua.Host, ub.Host)
}

// isPrivateFetchIP reports whether an address belongs to a range the gateway
// must never fetch content from on a provider's behalf: loopback, RFC1918,
// link-local (cloud metadata lives there), unique-local IPv6 and the well-known
// metadata endpoints.
func isPrivateFetchIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if isMetadataIP(ip) {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() {
		return true
	}
	// IPv6 unique-local fc00::/7 (net.IP.IsPrivate covers it on modern Go, but
	// keep the explicit check so the intent survives a stdlib change).
	if len(ip) == net.IPv6len {
		if ip[0]&0xfe == 0xfc {
			return true
		}
	}
	return false
}

// videoJobVisible scopes a job to the gateway key that created it.
//
// P2-1: this is a tenancy check, so it fails CLOSED. An empty
// X-Gateway-Key-Prefix means "no gateway key in front of this route" (the
// middleware always sets it, for dashboard sessions too), and a row whose
// key_prefix is empty predates key recording — neither is a reason to let the
// caller see every job. The one explicit exception is a dashboard session
// (an operator in the UI), which mirrors the request-log model.
func (h *Handler) videoJobVisible(r *http.Request, job *videoJob) bool {
	if job == nil {
		return false
	}
	if isDashboardSession(r) {
		return true
	}
	prefix := r.Header.Get("X-Gateway-Key-Prefix")
	if prefix == "" || job.KeyPrefix == "" {
		return false
	}
	return prefix == job.KeyPrefix
}

// isDashboardSession reports whether the request was authenticated by a
// dashboard session (JWT) rather than a gateway API key. Those keys carry the
// synthetic "session:<subject>" id (see the auth middleware).
func isDashboardSession(r *http.Request) bool {
	if r == nil {
		return false
	}
	k, ok := middleware.GatewayKeyFromContext(r.Context())
	if !ok || k == nil {
		return false
	}
	return strings.HasPrefix(k.ID, "session:")
}

func writeVideoJSON(w http.ResponseWriter, v *videogen.Video) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(videogen.VideoToJSON(v))
}

// videoCost prices a job from the curated per-second registry.
func (h *Handler) videoCost(model, seconds string) float64 {
	if cost, ok := videogen.CostForModelSeconds(model, seconds); ok {
		return cost
	}
	return 0
}

/* ------------------------------------------------------------------ */
/* persistence                                                         */
/* ------------------------------------------------------------------ */

func (h *Handler) insertVideoJob(j *videoJob) error {
	_, err := h.DB.Exec(db.Q(`INSERT INTO video_jobs
		(id, provider_id, key_prefix, dialect, model, upstream_id, prompt, seconds, size, status, progress, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		j.ID, j.ProviderID, j.KeyPrefix, string(j.Dialect), j.Model, j.UpstreamID,
		j.Prompt, j.Seconds, j.Size, j.Status, j.Progress, j.CreatedAt, j.UpdatedAt)
	return err
}

func (h *Handler) updateVideoJob(j *videoJob) error {
	var completed, expires any
	if j.Completed != nil {
		completed = *j.Completed
	}
	if j.Expires != nil {
		expires = *j.Expires
	}
	_, err := h.DB.Exec(db.Q(`UPDATE video_jobs SET status=?, progress=?, error_code=?, error_message=?,
		video_url=?, completed_at=?, expires_at=?, updated_at=? WHERE id=?`),
		j.Status, j.Progress, j.ErrCode, j.ErrMessage, j.VideoURL, completed, expires, j.UpdatedAt, j.ID)
	return err
}

func (h *Handler) getVideoJob(id string) (*videoJob, error) {
	row := h.DB.QueryRow(db.Q(`SELECT id, provider_id, COALESCE(key_prefix,''), dialect, model, upstream_id,
		COALESCE(prompt,''), COALESCE(seconds,''), COALESCE(size,''), status, COALESCE(progress,0),
		COALESCE(error_code,''), COALESCE(error_message,''), COALESCE(video_url,''), created_at, updated_at,
		completed_at, expires_at FROM video_jobs WHERE id=?`), id)
	j, err := scanVideoJob(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return j, err
}

// videoJobScanner is satisfied by both *sql.Row and *sql.Rows.
type videoJobScanner interface{ Scan(dest ...any) error }

func scanVideoJob(sc videoJobScanner) (*videoJob, error) {
	var j videoJob
	var dialect string
	var completed, expires sql.NullTime
	if err := sc.Scan(&j.ID, &j.ProviderID, &j.KeyPrefix, &dialect, &j.Model, &j.UpstreamID,
		&j.Prompt, &j.Seconds, &j.Size, &j.Status, &j.Progress,
		&j.ErrCode, &j.ErrMessage, &j.VideoURL, &j.CreatedAt, &j.UpdatedAt,
		&completed, &expires); err != nil {
		return nil, err
	}
	j.Dialect = videogen.Dialect(dialect)
	if completed.Valid {
		t := completed.Time
		j.Completed = &t
	}
	if expires.Valid {
		t := expires.Time
		j.Expires = &t
	}
	return &j, nil
}
