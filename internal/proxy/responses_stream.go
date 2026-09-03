package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// responsesNativeMaxSample bounds the debug-body sample captured from a
// relayed native stream: at most this many bytes are ever retained while the
// full body keeps flowing straight through to the client.
const responsesNativeMaxSample = 32 << 10

// sseSampleCap is the sampling budget shared by both Responses streaming
// paths (debug-body log; mirrors pumpStream's ≤32KB clamp).
func (h *Handler) sseSampleCap() int {
	if h.BodyLogMaxBytes <= 0 {
		return 0
	}
	c := h.BodyLogMaxBytes
	if c > responsesNativeMaxSample {
		c = responsesNativeMaxSample
	}
	return c
}

// isResponsesKeepaliveEvent reports whether an SSE event is a transport-level
// keepalive / sentinel rather than real Responses protocol:
//
//   - event: ping (or data {"type":"ping"}): some OpenAI-compatible upstreams
//     (xAI and others) emit these to keep idle connections alive;
//   - data: [DONE]: the Chat Completions sentinel, NOT part of the Responses
//     protocol (which terminates with response.completed / response.failed).
//
// Strict Responses clients (e.g. Grok Build CLI's Rust serde enum expecting
// only response.created / response.in_progress / response.completed /
// response.failed / response.incomplete / response.*) fail HARD on these with
// "serialization error: unknown variant `ping`" instead of ignoring them.
// The gateway must therefore never forward them verbatim on the native
// Responses path.
func isResponsesKeepaliveEvent(ev sseEvent) bool {
	if ev.name == "ping" {
		return true
	}
	data := bytes.TrimSpace(ev.data)
	if len(data) == 0 {
		return false
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return true
	}
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	var m map[string]interface{}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	if t, _ := m["type"].(string); t == "ping" {
		return true
	}
	return false
}

// cutCompleteSSEBlock extracts the first complete SSE block (terminated by a
// blank line) from pending. It handles both LF and CRLF framing and preserves
// the original bytes verbatim for forwarding. ok=false means no complete
// block yet — the caller must wait for more upstream bytes.
func cutCompleteSSEBlock(pending []byte) (block, rest []byte, ok bool) {
	pos := 0
	for pos < len(pending) {
		nl := bytes.IndexByte(pending[pos:], '\n')
		if nl < 0 {
			return nil, pending, false
		}
		lineEnd := pos + nl + 1
		if len(bytes.TrimSpace(pending[pos:lineEnd])) == 0 {
			block = pending[:lineEnd]
			rest = append([]byte(nil), pending[lineEnd:]...)
			return block, rest, true
		}
		pos = lineEnd
	}
	return nil, pending, false
}

// nativeStreamChunk is pumpStream's pump-channel payload shape: a fixed-size
// value copy avoids aliasing between the producer goroutine and the relay loop.
type nativeStreamChunk struct {
	data [8192]byte
	n    int
	err  error
}

// nativeStreamResult reports how a native pass-through stream ended.
type nativeStreamResult struct {
	// committed is true once ANY byte reached the client. When false no
	// headers were written and the caller may safely fall through elsewhere.
	committed bool
}

// streamNativeResponses relays a native OpenAI Responses SSE body to the
// client chunk-by-chunk as bytes arrive — a true os.Copy-style pass-through
// with no intermediate buffering of the whole body:
//   - flush per read; honors r.Context() cancellation;
//   - idle watchdog on h.Timeouts.StreamIdle, reset per received chunk
//     (pattern copied from pumpStream);
//   - NO more than a bounded sample (≤32KB / BodyLogMaxBytes) is retained,
//     used for the debug-body log;
//   - usage is harvested by running extractUsage (plus the recursive
//     harvestAnthropicTokens scan, which also reaches the nested
//     response.completed → response.usage object) over every completed data:
//     frame parsed out of the relayed byte stream.
//
// Termination conventions mirror pumpStream exactly:
//   - clean upstream EOF → recordUsage + logRequestExtendedBodies(isStream)
//   - abnormal death → honest failure logs (502 / 499-client-gone), nothing
//     further written to the wire (the upstream already controls the protocol);
//   - first-read EOF/error before ANY byte was committed → committed=false so
//     the caller can fall through to the translated path instead of emitting
//     garbage.
func (h *Handler) streamNativeResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, model, keyPrefix, providerID string, start time.Time, ttfb *ttfbController) nativeStreamResult {
	res := nativeStreamResult{}
	ctx := r.Context()
	upstreamCtx := resp.Request.Context()

	flusher, _ := w.(http.Flusher)

	idle := h.Timeouts.StreamIdle
	wd := newIdleWatchdog(idle)
	defer wd.stop()

	chunks := make(chan nativeStreamChunk) // sized copies avoid aliasing
	go func() {
		for {
			var cm nativeStreamChunk
			cm.n, cm.err = resp.Body.Read(cm.data[:])
			select {
			case chunks <- cm:
			case <-upstreamCtx.Done():
				return
			}
			if cm.err != nil {
				return
			}
		}
	}()

	var pending []byte // partial SSE event carry-over between reads
	var promptTok, completionTok int
	sampleCap := h.sseSampleCap()
	sample := &bytes.Buffer{}
	// Assembled-text capture for the usage log (nil when body logging is off).
	var capture *streamCapture
	if h.LogBodies {
		capture = newStreamCapture(h.BodyLogMaxBytes)
	}

	commit := func() {
		if ttfb.headersCommitted() {
			// Keepalive umbra already on the wire; real frames take over.
			ttfb.stop()
			return
		}
		copyHeader(w.Header(), resp.Header)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
	}

	harvestFrame := func(frame []byte) {
		events := parseSSEEvents(frame)
		for _, ev := range events {
			// Bounded raw sample for the debug-body log.
			if sampleCap > 0 && sample.Len() < sampleCap {
				snapshot := make([]byte, 0, len(ev.data)+len(ev.name)+16)
				if ev.name != "" {
					snapshot = append(snapshot, []byte(ev.name+": ")...)
				}
				snapshot = append(snapshot, ev.data...)
				snapshot = append(snapshot, '\n')
				if sample.Len()+len(snapshot) > sampleCap {
					snapshot = snapshot[:sampleCap-sample.Len()]
				}
				sample.Write(snapshot)
			}
			if bytes.Equal(bytes.TrimSpace(ev.data), []byte("[DONE]")) {
				continue
			}
			var evMap map[string]interface{}
			if len(ev.data) > 0 && ev.data[0] == '{' {
				if err := json.Unmarshal(ev.data, &evMap); err != nil {
					evMap = nil
				}
			}
			if capture != nil {
				capture.observe(ev, evMap, false)
			}
			// Usage lives on response.completed (nested under "response")
			// for OpenAI and inside message_start/message_delta for exotic
			// compat upstreams — extractUsage covers flat shapes,
			// harvestAnthropicTokens walks nested ones.
			pt, ct := extractUsage(ev.data)
			if pt > 0 {
				promptTok = pt
			}
			if ct > 0 {
				completionTok = ct
			}
			aPt, aCt := harvestAnthropicTokens(ev.data)
			if aPt > 0 {
				promptTok = aPt
			}
			if aCt > 0 {
				completionTok = aCt
			}
		}
	}

	fail := func(reason string, clientGone bool) nativeStreamResult {
		resp.Body.Close()
		drainChan(chunks)
		logStatus := http.StatusBadGateway
		if clientGone {
			logStatus = 499 // nginx convention: client closed request
		}
		cost := h.costForModel(model, promptTok, completionTok)
		log.Error().Str("model", model).Str("provider", providerID).Str("reason", reason).Bool("client_gone", clientGone).Msg("native responses stream terminated abnormally")
		h.recordUsage(keyPrefix, r, promptTok+completionTok, cost)
		h.logRequestStreamed(keyPrefix, providerID, model, "responses", logStatus, time.Since(start).Milliseconds(), promptTok, completionTok, cost, sample.Bytes(), capture)
		res.committed = true
		return res
	}

	first := true

	finishClean := func() nativeStreamResult {
		resp.Body.Close()
		if len(pending) > 0 {
			// Flush a trailing fragment that never got its blank-line
			// terminator (upstream closed without final \n\n). Probe it as
			// a complete event: ping/[DONE] fragments are still dropped so
			// a ping-only stream correctly falls through to translated.
			trimmed := bytes.TrimSpace(pending)
			if len(trimmed) > 0 {
				probe := append(append([]byte(nil), pending...), '\n', '\n')
				evs := parseSSEEvents(probe)
				if len(evs) == 0 {
					// Pure comment fragment (e.g. ": keepalive" without
					// terminator): safe, forward verbatim.
					if first {
						first = false
						commit()
					}
					w.Write(pending)
					if flusher != nil {
						flusher.Flush()
					}
				} else {
					suppressed := false
					for _, ev := range evs {
						if isResponsesKeepaliveEvent(ev) {
							suppressed = true
							break
						}
					}
					if !suppressed {
						if first {
							first = false
							commit()
						}
						w.Write(pending)
						if flusher != nil {
							flusher.Flush()
						}
						harvestFrame(probe)
					}
				}
			}
			pending = nil
		}
		if first {
			// Nothing but ping/[DONE]/comments (or nothing at all) arrived:
			// fall through to the translated path which synthesizes a
			// strict-client-safe response.* transcript instead of emitting
			// an empty protocol violation.
			resp.Body.Close()
			drainChan(chunks)
			log.Info().Str("model", model).Str("provider", providerID).Msg("native responses stream produced no usable events before commit; falling through to translated path")
			res.committed = false
			return res
		}
		cost := h.costForModel(model, promptTok, completionTok)
		h.recordUsage(keyPrefix, r, promptTok+completionTok, cost)
		h.logRequestStreamed(keyPrefix, providerID, model, "responses", resp.StatusCode, time.Since(start).Milliseconds(), promptTok, completionTok, cost, sample.Bytes(), capture)
		res.committed = true
		return res
	}

	// relayBlock forwards one complete SSE block after strict-client
	// filtering. Returns true if the block committed real protocol bytes
	// (and therefore flipped `first`).
	relayBlock := func(block []byte) bool {
		evs := parseSSEEvents(block)
		if len(evs) == 0 {
			// Pure SSE comment (": keepalive" from upstream or edge):
			// invisible to event parsers but a real byte for idle timers.
			// Safe for strict clients — forward verbatim.
			if first {
				first = false
				commit()
			}
			w.Write(block)
			if flusher != nil {
				flusher.Flush()
			}
			if sampleCap > 0 && sample.Len() < sampleCap {
				take := len(block)
				if room := sampleCap - sample.Len(); take > room {
					take = room
				}
				sample.Write(block[:take])
			}
			return true
		}
		for _, ev := range evs {
			if isResponsesKeepaliveEvent(ev) {
				// Drop: strict Responses clients fail hard on unknown
				// `ping` / [DONE] variants. The gateway's own TTFB
				// heartbeat (": keepalive" comments) already keeps edge
				// idle timers fed, so dropping loses no liveness.
				return false
			}
		}
		if first {
			first = false
			commit()
		}
		w.Write(block)
		if flusher != nil {
			flusher.Flush()
		}
		if sampleCap > 0 && sample.Len() < sampleCap {
			take := len(block)
			if room := sampleCap - sample.Len(); take > room {
				take = room
			}
			sample.Write(block[:take])
		}
		harvestFrame(block)
		return true
	}

	for {
		select {
		case <-ctx.Done():
			if first {
				// Client vanished before anything flowed: nobody left to
				// serve; do NOT fall through to a translated retry.
				resp.Body.Close()
				drainChan(chunks)
				log.Warn().Str("model", model).Str("provider", providerID).Msg("client disconnected during native responses stream before first byte")
				h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", 499, time.Since(start).Milliseconds(), 0, 0, 0, true, nil, nil)
				res.committed = true
				return res
			}
			return fail("gateway client disconnected", true)

		case <-wd.c:
			return fail("upstream idle timeout: no data received within "+idle.String(), false)

		case cm := <-chunks:
			wd.reset(idle) // watchdog resets per delivered chunk

			// Process payload FIRST: Read may deliver n>0 together with err==io.EOF.
			if cm.n > 0 {
				buf := cm.data[:cm.n]
				pending = append(pending, buf...)
				for {
					block, rest, ok := cutCompleteSSEBlock(pending)
					if !ok {
						break
					}
					pending = rest
					relayBlock(block)
				}
				if len(pending) > 1<<20 { // runaway non-SSE payload guard
					// No blank-line terminator in 1MB: not SSE. Treat as
					// opaque — harvest for usage, forward once so the
					// client is not hung, and reset.
					harvestFrame(pending)
					if first {
						first = false
						commit()
					}
					w.Write(pending)
					if flusher != nil {
						flusher.Flush()
					}
					pending = pending[:0]
				}
			}

			if cm.err != nil {
				// True io.EOF = graceful upstream end-of-stream (the decoder
				// emits io.EOF only for a properly terminated body). Anything
				// else — including io.ErrUnexpectedEOF from a decapitated
				// chunked response — is abnormal.
				if cm.err == io.EOF {
					if first {
						// Zero bytes AND clean EOF before any commit: the
						// upstream accepted the connection then produced
						// nothing usable — fall through translated instead
						// of emitting an empty protocol violation.
						resp.Body.Close()
						drainChan(chunks)
						log.Info().Str("model", model).Str("provider", providerID).Msg("native responses stream produced no data before commit; falling through to translated path")
						res.committed = false
						return res
					}
					return finishClean()
				}
				return fail("upstream stream error: "+cm.err.Error(), false)
			}
		}
	}
}

// forceStreamTrue injects "stream":true into a JSON request body, overriding
// whatever the translator carried (both dialects honor the boolean field).
func forceStreamTrue(body []byte) ([]byte, bool) {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	m["stream"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// upstreamErrorMessage pulls {"error":{"message":...}} out of an error
// response body for surfacing inside response.failed frames.
func upstreamErrorMessage(body []byte, fallback string) string {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) == nil {
		if e, ok := m["error"].(map[string]interface{}); ok {
			if msg, ok := e["message"].(string); ok && msg != "" {
				return msg
			}
		}
	}
	return fallback
}

// itoaCacheFree avoids pulling fmt in just for tiny integer formatting.
func itoaCacheFree(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// sseEventWriter emits framed OpenAI-Responses-style server-sent events:
//
//	event: <name>\ndata: <one-line JSON>\n\n
//
// Every data payload carries "type" (== event name) and a strictly increasing
// "sequence_number". A bounded sample of emitted frames feeds the debug log.
type sseEventWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	seq     int
	buf     bytes.Buffer
	cap     int
}

// idleWatchdog bounds the gap between upstream chunks. A zero/negative idle
// means "disabled": time.NewTimer(0) fires immediately and Stop() cannot
// retract the already-queued value, so a disabled watchdog built from
// NewTimer(0) still spuriously fires once. A nil channel blocks forever when
// received from in a select — exactly "disabled".
type idleWatchdog struct {
	timer *time.Timer
	c     <-chan time.Time
}

func newIdleWatchdog(idle time.Duration) *idleWatchdog {
	if idle <= 0 {
		return &idleWatchdog{}
	}
	t := time.NewTimer(idle)
	return &idleWatchdog{timer: t, c: t.C}
}

// reset re-arms the watchdog after each delivered chunk (no-op when disabled).
func (w *idleWatchdog) reset(idle time.Duration) {
	if w.timer != nil {
		w.timer.Reset(idle)
	}
}

func (w *idleWatchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

func newSSEEventWriter(w http.ResponseWriter, h *Handler) *sseEventWriter {
	f, _ := w.(http.Flusher)
	return &sseEventWriter{w: w, flusher: f, cap: h.sseSampleCap()}
}

// emit writes one event; payload is mutated to include type/sequence_number.
func (s *sseEventWriter) emit(event string, payload map[string]interface{}) {
	s.seq++
	payload["type"] = event
	payload["sequence_number"] = s.seq
	data, err := json.Marshal(payload)
	if err != nil {
		return // payloads are built exclusively from marshallable primitives
	}
	frame := "event: " + event + "\ndata: " + string(data) + "\n\n"
	s.w.Write([]byte(frame))
	if s.flusher != nil {
		s.flusher.Flush()
	}
	if s.cap > 0 && s.buf.Len() < s.cap {
		sn := frame
		if over := s.buf.Len() + len(sn) - s.cap; over > 0 {
			sn = sn[:len(sn)-over]
		}
		s.buf.WriteString(sn)
	}
}

func (s *sseEventWriter) sample() []byte { return s.buf.Bytes() }

// respToolCall accumulates one streaming tool call (chat delta.tool_calls or
// an anthropic tool_use block) for re-emission as Responses function_call
// output items. Without this accumulation a model tool call on
// /v1/responses vanished mid-stream and agents could never loop.
type respToolCall struct {
	itemID  string
	callID  string
	name    string
	args    strings.Builder
	outIdx  int
	started bool
}

// emitToolCallAdded announces a function_call output item the first time a
// fragment for it arrives.
func emitToolCallAdded(em *sseEventWriter, acc *respToolCall) {
	if acc.started {
		return
	}
	acc.started = true
	em.emit("response.output_item.added", map[string]interface{}{
		"output_index": acc.outIdx,
		"item": map[string]interface{}{
			"id":        acc.itemID,
			"type":      "function_call",
			"call_id":   acc.callID,
			"name":      acc.name,
			"arguments": "",
			"status":    "in_progress",
		},
	})
}

// responsesPumpResult carries what pumpResponsesFromStream harvested.
type responsesPumpResult struct {
	midStreamFailure bool
	clientGone       bool
}

// streamTranslatedResponses serves streaming /v1/responses when the selected
// provider has no native Responses API: it opens ONE effective attempt window
// against the translated chat (or anthropic messages) endpoint with stream:true
// forced, then re-emits inbound deltas as the OpenAI Responses streaming
// protocol via pumpResponsesFromStream.
//
// Retry semantics equal proxyWithMetrics pre-commit behavior:
//   - transport errors or status>=500/429 → sleepCtx/backoff + ShouldRetry up
//     to MaxRetries, all BEFORE any byte reaches the client;
//   - every terminal pre-commit failure writes one clean
//     `event: response.failed` frame instead of an HTTP-shaped JSON blob;
//   - after commit there are no retries, ever.
func (h *Handler) streamTranslatedResponses(w http.ResponseWriter, r *http.Request, targetURL, apiKey string, upstreamBody []byte, model, keyPrefix, providerID string, start time.Time, isAnthropicUpstream bool, ttfb *ttfbController) {
	retry := h.retryOrDefault()

	ctx := r.Context()
	var cancelFn context.CancelFunc
	if h.Timeouts.RequestTotal > 0 {
		ctx, cancelFn = context.WithTimeout(ctx, h.Timeouts.RequestTotal)
		defer cancelFn()
	}

	if forced, changed := forceStreamTrue(upstreamBody); changed {
		upstreamBody = forced
	}

	for attempt := 0; ; attempt++ {
		// Budget spent by the native probe and still nothing committed: this
		// stream commits keepalive headers, then either retries under the
		// umbra or terminates with the protocol's response.failed event.
		if ttfb.expired() && !ttfb.headersCommitted() {
			ttfb.commitKeepalive(w, "")
		}
		req, err := h.newUpstreamRequest(ctx, r, targetURL, apiKey, upstreamBody, true, isAnthropicUpstream)
		if err != nil {
			if ttfb.headersCommitted() {
				ttfb.stop()
				h.emitTranslatedResponsesFailure(w, model, keyPrefix, providerID, "upstream_error", "failed to create upstream request: "+err.Error(), start)
				return
			}
			h.emitTranslatedResponsesFailure(w, model, keyPrefix, providerID, "upstream_error", "failed to create upstream request: "+err.Error(), start)
			return
		}
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		defer attemptCancel()
		req = req.WithContext(attemptCtx)
		ttfbTimer := armTTFBWatchdog(ttfb, attemptCancel)
		resp, err := h.Client.Do(req)
		if ttfbTimer != nil {
			ttfbTimer.Stop()
		}
		if err != nil {
			if ctx.Err() != nil {
				// Client cancelled during the retry window: stop silently.
				log.Warn().Str("model", model).Str("provider", providerID).Msg("client disconnected during translated responses retry window")
				h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", 499, time.Since(start).Milliseconds(), 0, 0, 0, true, nil, nil)
				return
			}
			if ttfb.expired() && !ttfb.headersCommitted() {
				// Out of first-byte budget: commit keepalive so the client
				// (and any edge) stays connected through remaining retries.
				ttfb.commitKeepalive(w, "")
			}
			if retry.ShouldRetry(attempt, retryableCode(0)) {
				sleepCtx(ctx, retryAfterDelay(nil, retry.Backoff(attempt)))
				continue
			}
			h.emitTranslatedResponsesFailure(w, model, keyPrefix, providerID, "upstream_error", "upstream unavailable: "+err.Error(), start)
			return
		}

		status := resp.StatusCode
		ct := resp.Header.Get("Content-Type")
		if status < 200 || status >= 400 || !strings.HasPrefix(ct, "text/event-stream") {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			retryable := status == 429 || status >= 500
			if retryable && retry.ShouldRetry(attempt, retryableCode(status)) {
				sleepCtx(r.Context(), retryAfterDelay(resp.Header, retry.Backoff(attempt)))
				continue
			}
			h.emitTranslatedResponsesFailure(w, model, keyPrefix, providerID, "upstream_error",
				"upstream returned status "+itoaCacheFree(status)+": "+upstreamErrorMessage(bodyBytes, "non-streamable upstream response"), start)
			return
		}

		// Usable 200 + text/event-stream: hand over to the translation pump.
		h.pumpResponsesFromStream(w, r, resp, model, keyPrefix, providerID, start, isAnthropicUpstream, ttfb)
		return
	}
}

// emitTranslatedResponsesFailure commits a minimal but protocol-clean SSE
// exchange whose single event is response.failed. Used for failures that
// occur inside the pre-commit retry window once retries are exhausted.
func (h *Handler) emitTranslatedResponsesFailure(w http.ResponseWriter, model, keyPrefix, providerID, code, reason string, start time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)

	em := newSSEEventWriter(w, h)
	respObj := map[string]interface{}{
		"id":         "resp_" + uuid.NewString(),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "failed",
		"model":      model,
		"error":      map[string]interface{}{"code": code, "message": reason},
	}
	em.emit("response.failed", map[string]interface{}{"response": respObj})
	log.Error().Str("model", model).Str("provider", providerID).Str("code", code).Str("reason", reason).Msg("translated responses stream failed before commit")
	h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", http.StatusBadGateway, time.Since(start).Milliseconds(), 0, 0, 0, true, nil, em.sample())
}

// pumpResponsesFromStream reads the committed upstream SSE body and re-emits
// the OpenAI Responses streaming protocol:
//
//	response.created → response.in_progress → response.output_item.added →
//	response.content_part.added → response.output_text.delta×k →
//	response.output_text.done → response.content_part.done →
//	response.output_item.done → response.completed
//
// Deltas come from chat choices[].delta.content frames, or — for anthropic
// upstreams — content_block_delta text_delta frames (thinking /
// redacted_thinking blocks are skipped). Usage is harvested strictly from
// upstream usage frames (chat final chunk / anthropic message_start +
// message_delta); tokens are never fabricated. Mid-stream death or idle
// timeouts terminate with a single response.failed frame
// (error.code "upstream_error"/"timeout"); client disconnects stop silently.
// Idle watchdog discipline matches pumpStream. There is no [DONE] sentinel:
// the protocol terminates with response.completed (or response.failed).
func (h *Handler) pumpResponsesFromStream(w http.ResponseWriter, r *http.Request, resp *http.Response, model, keyPrefix, providerID string, start time.Time, isAnthropicUpstream bool, ttfb *ttfbController) responsesPumpResult {
	res := responsesPumpResult{}
	ctx := r.Context()
	upstreamCtx := resp.Request.Context()

	idle := h.Timeouts.StreamIdle
	wd := newIdleWatchdog(idle)
	defer wd.stop()

	chunks := make(chan nativeStreamChunk)
	go func() {
		for {
			var cm nativeStreamChunk
			cm.n, cm.err = resp.Body.Read(cm.data[:])
			select {
			case chunks <- cm:
			case <-upstreamCtx.Done():
				return
			}
			if cm.err != nil {
				return
			}
		}
	}()

	em := newSSEEventWriter(w, h)
	respID := "resp_" + uuid.NewString()
	itemID := "msg_" + uuid.NewString()[:8]
	createdAt := time.Now().Unix()

	var (
		text        strings.Builder
		promptTok   int
		completeTok int
		started     bool // headers committed + created/in_progress emitted
		blockTypes  = map[int]string{}
		// Tool-call accumulation (chat delta.tool_calls keyed by their
		// index; anthropic tool_use blocks keyed by block index).
		toolCalls = map[int]*respToolCall{}
		fcOrder   []int
		// streamErrMsg/Code capture an in-band upstream error frame
		// (chat `{"error":{...}}` / anthropic `event: error`). The stream
		// must terminate with response.failed carrying the upstream's
		// message — never launder it into response.completed.
		streamErrMsg  string
		streamErrCode string
		// finishReason captures the upstream's terminal finish_reason
		// ("length"/"content_filter") so truncation is reported honestly
		// as response.incomplete instead of a certified-complete response.
		finishReason string
		// streamDetail accumulates the usage-log metadata (cache/reasoning
		// token split + normalized finish reason) harvested from frames.
		streamDetail usageDetail
	)

	toolSlot := func(key int) *respToolCall {
		if acc, ok := toolCalls[key]; ok {
			return acc
		}
		acc := &respToolCall{
			itemID: "fc_" + uuid.NewString()[:8],
			outIdx: len(fcOrder) + 1, // 0 is the message item
		}
		toolCalls[key] = acc
		fcOrder = append(fcOrder, key)
		return acc
	}

	responseBase := func(status string) map[string]interface{} {
		return map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"model":      model,
		}
	}

	emitDelta := func(piece string) {
		em.emit("response.output_text.delta", map[string]interface{}{
			"delta":         piece,
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
		})
	}

	commitAndStart := func() {
		if ttfb.headersCommitted() {
			// Keepalive umbra already on the wire: the status line is sent,
			// but the protocol preamble below must still flow (the client
			// needs response.created before any delta). Just end the
			// heartbeat — real frames take over from here.
			ttfb.stop()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(http.StatusOK)
		em.emit("response.created", map[string]interface{}{"response": responseBase("in_progress")})
		em.emit("response.in_progress", map[string]interface{}{"response": responseBase("in_progress")})
		em.emit("response.output_item.added", map[string]interface{}{
			"output_index": 0,
			"item": map[string]interface{}{
				"id":      itemID,
				"type":    "message",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []interface{}{},
			},
		})
		em.emit("response.content_part.added", map[string]interface{}{
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": ""},
		})
		started = true
	}

	finalizeSuccess := func() {
		full := text.String()
		// Truncation-aware statuses: a length/content_filter-capped response
		// is "incomplete" at both the response and message-item level.
		msgStatus := "completed"
		switch finishReason {
		case "length", "content_filter":
			msgStatus = "incomplete"
		}
		em.emit("response.output_text.done", map[string]interface{}{
			"text":          full,
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
		})
		em.emit("response.content_part.done", map[string]interface{}{
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": full},
		})
		em.emit("response.output_item.done", map[string]interface{}{
			"output_index": 0,
			"item": map[string]interface{}{
				"id":     itemID,
				"type":   "message",
				"role":   "assistant",
				"status": msgStatus,
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": full, "annotations": []interface{}{}},
				},
			},
		})
		// Close out accumulated tool calls and assemble the final output
		// array: message item first, then each function_call in arrival
		// order. The completed frame carries the whole output so clients
		// can consume tool calls from either the events or the terminal
		// response object.
		outputArr := []interface{}{map[string]interface{}{
			"id":     itemID,
			"type":   "message",
			"role":   "assistant",
			"status": msgStatus,
			"content": []interface{}{
				map[string]interface{}{"type": "output_text", "text": full, "annotations": []interface{}{}},
			},
		}}
		for _, key := range fcOrder {
			acc := toolCalls[key]
			args := acc.args.String()
			em.emit("response.function_call_arguments.done", map[string]interface{}{
				"item_id":      acc.itemID,
				"output_index": acc.outIdx,
				"arguments":    args,
			})
			item := map[string]interface{}{
				"id":        acc.itemID,
				"type":      "function_call",
				"status":    "completed",
				"arguments": args,
			}
			if acc.callID != "" {
				item["call_id"] = acc.callID
			}
			if acc.name != "" {
				item["name"] = acc.name
			}
			em.emit("response.output_item.done", map[string]interface{}{
				"output_index": acc.outIdx,
				"item":         item,
			})
			outputArr = append(outputArr, item)
		}
		usage := map[string]interface{}{
			"input_tokens":  promptTok,
			"output_tokens": completeTok,
			"total_tokens":  promptTok + completeTok,
		}
		// Truncation honesty: finish_reason "length"/"content_filter" means
		// the response is NOT complete — certify it as response.incomplete
		// with incomplete_details, never as a clean response.completed.
		// (A truncated json_schema payload marked "completed" is the worst
		// case: clients parse and trust it.)
		incompleteReason := ""
		switch finishReason {
		case "length":
			incompleteReason = "max_output_tokens"
		case "content_filter":
			incompleteReason = "content_filter"
		}
		terminalEvent := "response.completed"
		robj := responseBase("completed")
		if incompleteReason != "" {
			terminalEvent = "response.incomplete"
			robj = responseBase("incomplete")
			robj["incomplete_details"] = map[string]interface{}{"reason": incompleteReason}
		}
		robj["usage"] = usage
		robj["output"] = outputArr
		em.emit(terminalEvent, map[string]interface{}{"response": robj})

		cost := h.costForModel(model, promptTok, completeTok)
		h.recordUsage(keyPrefix, r, promptTok+completeTok, cost)
		if streamDetail.FinishReason == "" && finishReason != "" {
			streamDetail.FinishReason = normalizeFinishReason(finishReason)
		}
		// Assembled responses payload (output items + usage) is far more
		// useful in the usage log than the raw SSE sample; keep the sample
		// as fallback when assembly produced nothing.
		if assembled, err := json.Marshal(robj); err == nil && len(assembled) > 0 {
			h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", resp.StatusCode, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, assembled, &logMeta{
				FinishReason: streamDetail.FinishReason,
				CacheRead:    streamDetail.CacheRead,
				CacheWrite:   streamDetail.CacheWrite,
				Reasoning:    streamDetail.Reasoning,
			})
		} else {
			h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", resp.StatusCode, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, em.sample())
		}
	}

	failTerminated := func(reason, code string, clientGone bool) {
		robj := responseBase("failed")
		robj["error"] = map[string]interface{}{"code": code, "message": reason}
		em.emit("response.failed", map[string]interface{}{"response": robj})

		logStatus := http.StatusBadGateway
		if clientGone {
			logStatus = 499 // nginx convention: client closed request
		}
		log.Error().Str("model", model).Str("provider", providerID).Str("code", code).Bool("client_gone", clientGone).Str("reason", reason).Msg("translated responses stream terminated abnormally")
		cost := h.costForModel(model, promptTok, completeTok)
		h.recordUsage(keyPrefix, r, promptTok+completeTok, cost)
		h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", logStatus, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, em.sample())
	}

	stopSilently := func() {
		// Client disconnect: no more writes — just honest bookkeeping.
		log.Error().Str("model", model).Str("provider", providerID).Str("reason", "gateway client disconnected").Bool("client_gone", true).Msg("mid-stream failure terminated")
		cost := h.costForModel(model, promptTok, completeTok)
		h.recordUsage(keyPrefix, r, promptTok+completeTok, cost)
		h.logRequestExtendedBodies(keyPrefix, providerID, model, "responses", 499, time.Since(start).Milliseconds(), promptTok, completeTok, cost, true, nil, em.sample())
	}

	handleEvent := func(ev sseEvent) {
		data := bytes.TrimSpace(ev.data)
		if len(data) == 0 {
			return
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) != nil {
			return
		}
		typ, _ := m["type"].(string)

		if isAnthropicUpstream {
			switch typ {
			case "message_start", "message_delta":
				pt, ct := harvestAnthropicTokens(data)
				if pt > 0 {
					promptTok = pt
				}
				if ct > 0 {
					completeTok = ct
				}
				if typ == "message_delta" {
					if delta, ok := m["delta"].(map[string]interface{}); ok {
						if s := normalizeFinishReason(delta["stop_reason"]); s != "" {
							streamDetail.FinishReason = s
						}
					}
				}
			case "content_block_start":
				idx := int(toInt(m["index"]))
				cb, _ := m["content_block"].(map[string]interface{})
				bt, _ := cb["type"].(string)
				blockTypes[idx] = bt
				if bt == "tool_use" {
					// Anthropic tool invocation: capture id/name now, args
					// arrive as input_json_delta fragments.
					acc := toolSlot(idx)
					if id, ok := cb["id"].(string); ok && id != "" {
						acc.callID = id
					}
					if n, ok := cb["name"].(string); ok && n != "" {
						acc.name = n
					}
					emitToolCallAdded(em, acc)
				}
			case "content_block_delta":
				idx := int(toInt(m["index"]))
				if bt := blockTypes[idx]; bt == "thinking" || bt == "redacted_thinking" {
					return
				}
				d, _ := m["delta"].(map[string]interface{})
				dt, _ := d["type"].(string)
				if bt := blockTypes[idx]; bt == "tool_use" {
					// Tool arguments stream as partial JSON fragments.
					if dt == "input_json_delta" {
						acc := toolCalls[idx]
						if acc == nil {
							acc = toolSlot(idx)
							emitToolCallAdded(em, acc)
						}
						if frag, ok := d["partial_json"].(string); ok && frag != "" {
							acc.args.WriteString(frag)
							em.emit("response.function_call_arguments.delta", map[string]interface{}{
								"delta":        frag,
								"item_id":      acc.itemID,
								"output_index": acc.outIdx,
							})
						}
					}
					return
				}
				if dt != "text_delta" {
					return
				}
				piece, _ := d["text"].(string)
				if piece == "" {
					return
				}
				text.WriteString(piece)
				emitDelta(piece)
			case "message_stop":
				// Terminal marker; completion frames follow below on EOF.
			case "error":
				// In-band upstream failure (e.g. overloaded_error). Surface
				// it as response.failed rather than a clean completion.
				if e, ok := m["error"].(map[string]interface{}); ok {
					if msg, ok := e["message"].(string); ok && msg != "" {
						streamErrMsg = msg
					}
					if t, ok := e["type"].(string); ok && t != "" {
						streamErrCode = t
					}
				}
			}
			return
		}

		// Chat dialect: bare data frames carrying choices[]/usage.
		switch typ {
		case "", "chat.completion.chunk":
			// In-band error frame: classic OpenAI-style
			// `data: {"error":{"message":...,"type":...}}` carries no choices.
			if e, ok := m["error"]; ok && e != nil {
				switch ev := e.(type) {
				case map[string]interface{}:
					if msg, ok := ev["message"].(string); ok && msg != "" {
						streamErrMsg = msg
					}
					if t, ok := ev["type"].(string); ok && t != "" {
						streamErrCode = t
					}
				case string:
					streamErrMsg = ev
				}
				return
			}
			if chArr, ok := m["choices"].([]interface{}); ok && len(chArr) > 0 {
				if c0, ok := chArr[0].(map[string]interface{}); ok {
					if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
						finishReason = fr
					}
					if d, ok := c0["delta"].(map[string]interface{}); ok {
						if piece, ok := d["content"].(string); ok && piece != "" {
							text.WriteString(piece)
							emitDelta(piece)
						}
						// Streaming tool calls: delta.tool_calls entries
						// carry {index,id,function{name,arguments-frag}}.
						if tcs, ok := d["tool_calls"].([]interface{}); ok {
							for _, tcRaw := range tcs {
								tc, ok := tcRaw.(map[string]interface{})
								if !ok {
									continue
								}
								idx := int(toInt(tc["index"]))
								acc := toolSlot(idx)
								if id, ok := tc["id"].(string); ok && id != "" {
									acc.callID = id
								}
								if fn, ok := tc["function"].(map[string]interface{}); ok {
									if n, ok := fn["name"].(string); ok && n != "" {
										acc.name += n
									}
									if frag, ok := fn["arguments"].(string); ok && frag != "" {
										emitToolCallAdded(em, acc)
										acc.args.WriteString(frag)
										em.emit("response.function_call_arguments.delta", map[string]interface{}{
											"delta":        frag,
											"item_id":      acc.itemID,
											"output_index": acc.outIdx,
										})
									}
								}
							}
						}
					}
				}
			}
		}
		// Usage rides the final chunk when include_usage was honored;
		// extractUsage understands prompt/completion_tokens AND
		// input/output_tokens spellings. Zero means "not present here" —
		// never fabricated.
		pt, ct, ud := extractUsageDetailFromMap(m)
		if ud.CacheRead+ud.CacheWrite > 0 {
			// Billing fold: ONLY anthropic cache fields are additive — OpenAI's
			// prompt_tokens_details.cached_tokens is a breakdown of
			// prompt_tokens and must not be counted twice.
			anthropicCache := ud.CacheWrite
			if usageMap, ok := m["usage"].(map[string]interface{}); ok {
				if _, hasFlatCache := usageMap["cache_read_input_tokens"]; hasFlatCache {
					anthropicCache += ud.CacheRead
				}
			}
			pt += anthropicCache
		}
		if pt > 0 {
			promptTok = pt
		}
		if ct > 0 {
			completeTok = ct
		}
		if ud.CacheRead > 0 {
			streamDetail.CacheRead = ud.CacheRead
		}
		if ud.CacheWrite > 0 {
			streamDetail.CacheWrite = ud.CacheWrite
		}
		if ud.Reasoning > 0 {
			streamDetail.Reasoning = ud.Reasoning
		}
		if ud.FinishReason != "" {
			streamDetail.FinishReason = ud.FinishReason
		}
	}

	var pending []byte // partial SSE event carry-over between reads

	processFramed := func(frame []byte) {
		events := parseSSEEvents(frame)
		for _, ev := range events {
			handleEvent(ev)
		}
	}

	for {
		select {
		case <-ctx.Done():
			resp.Body.Close()
			drainChan(chunks)
			res.clientGone = true
			res.midStreamFailure = true // pumpStream convention: aborts count
			stopSilently()
			return res

		case <-wd.c:
			resp.Body.Close()
			drainChan(chunks)
			res.midStreamFailure = true
			if !started {
				commitAndStart() // guarantee a parseable transcript
			}
			failTerminated("upstream idle timeout: no data received within "+idle.String(), "timeout", false)
			return res

		case cm := <-chunks:
			wd.reset(idle) // watchdog resets per delivered chunk

			// Process payload FIRST: Read may deliver n>0 together with err==io.EOF.
			if cm.n > 0 {
				if !started {
					commitAndStart()
				}
				buf := cm.data[:cm.n]
				pending = append(pending, buf...)
				if idx := bytes.LastIndexByte(pending, '\n'); idx >= 0 {
					frame := pending[:idx+1]
					processFramed(frame)
					rest := append([]byte(nil), pending[idx+1:]...)
					pending = rest
				}
				if len(pending) > 1<<20 { // runaway non-SSE payload guard
					processFramed(pending)
					pending = pending[:0]
				}
			}

			if cm.err != nil {
				resp.Body.Close()
				drainChan(chunks)
				// Success requires a TRUE io.EOF (properly terminated body)
				// AND no in-band upstream error frame; io.ErrUnexpectedEOF
				// etc. mean the upstream died mid-stream.
				if started && cm.err == io.EOF && streamErrMsg == "" {
					finalizeSuccess()
					return res
				}
				res.midStreamFailure = true
				if streamErrMsg != "" {
					// The upstream told us it failed; relay its verdict.
					if streamErrCode == "" {
						streamErrCode = "upstream_error"
					}
					failTerminated(streamErrMsg, streamErrCode, false)
					return res
				}
				if !started {
					commitAndStart() // guarantee a parseable transcript
				}
				if cm.err == io.EOF {
					failTerminated("upstream produced no stream data", "upstream_error", false)
				} else {
					failTerminated("upstream stream error: "+cm.err.Error(), "upstream_error", false)
				}
				return res
			}
		}
	}
}
