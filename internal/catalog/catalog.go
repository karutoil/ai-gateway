package catalog

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/decision"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/models"
	"ai-gateway/internal/videogen"
)

const ModelsDevURL = "https://models.dev/api.json"

type RawModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Family           string `json:"family"`
	Attachment       bool   `json:"attachment"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"` // effort, toggle
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	Temperature      bool   `json:"temperature"`
	Knowledge        string `json:"knowledge"`
	ReleaseDate      string `json:"release_date"`
	LastUpdated      string `json:"last_updated"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	OpenWeights bool `json:"open_weights"`
	// Decision is the models.dev decision-model marker, when present.
	Decision bool `json:"decision"`
	Limit    struct {
		Context int `json:"context"`
		Output  int `json:"output"`
		Input   int `json:"input"`
	} `json:"limit"`
	Cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

type RawProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	API    string              `json:"api"`
	Models map[string]RawModel `json:"models"`
}

type Store struct {
	db     *sql.DB
	client *http.Client

	// cache is a tiny in-process modelID -> catalog-row memo, invalidated on
	// every write that can change the catalog (sync, seeds, settings writes
	// through the store). It exists because the capability guards ask the same
	// handful of questions about the same model several times per request
	// (images.go calls Get/GetByShortID up to 3x, costForModel once) and each
	// miss used to be a 6 ms full table scan at 4k rows.
	cache *modelCache
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, client: &http.Client{Timeout: 30 * time.Second}, cache: newModelCache()}
}

// catalogColumns is the canonical projection for one catalog row. All four
// read paths share it so a new column cannot be added to some scans and not
// others.
const catalogColumns = `id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits, decision, image, video`

// catalogRowScanner is the subset of *sql.Row / *sql.Rows that Scan needs.
type catalogRowScanner interface{ Scan(dest ...any) error }

// scanCatalogModel reads one catalog row tolerating NULLs. The schema makes
// only id/provider/name NOT NULL — everything else is nullable — so each
// column is scanned into a sql.Null* and folded to its zero value.
//
// This is not defensive style for its own sake: scanning straight into
// string/int/float64/time.Time made ANY row with a NULL in those columns fail
// the scan, and callers treat that failure as "no such model". The model then
// became silently unpriceable, recording $0 cost and bypassing every per-key
// budget. The models.dev sync always writes explicit values so it did not bite
// in the normal path, but a row inserted by any other means is one NULL away
// from disappearing from pricing.
func scanCatalogModel(sc catalogRowScanner) (*models.CatalogModel, error) {
	var (
		m                                                    models.CatalogModel
		description, family, modalities, knowledgeCutoff     sql.NullString
		contextWindow, maxOutput                             sql.NullInt64
		inputCost, outputCost, cacheReadCost, cacheWriteCost sql.NullFloat64
		reasoning, toolCall, structuredOutput                sql.NullBool
		attachment, openWeights                              sql.NullBool
		updatedAt                                            sql.NullTime
		rt, rl, rol                                          sql.NullString
		decision                                             sql.NullInt64
		image                                                sql.NullInt64
		video                                                sql.NullInt64
	)
	if err := sc.Scan(&m.ID, &m.Provider, &m.Name, &description, &family, &contextWindow, &maxOutput,
		&inputCost, &outputCost, &cacheReadCost, &cacheWriteCost,
		&reasoning, &toolCall, &structuredOutput, &attachment, &modalities,
		&openWeights, &knowledgeCutoff, &updatedAt, &rt, &rl, &rol, &decision, &image, &video); err != nil {
		return nil, err
	}
	m.Description = description.String
	m.Family = family.String
	m.Modalities = modalities.String
	m.KnowledgeCutoff = knowledgeCutoff.String
	m.ContextWindow = int(contextWindow.Int64)
	m.MaxOutput = int(maxOutput.Int64)
	m.InputCost = inputCost.Float64
	m.OutputCost = outputCost.Float64
	m.CacheReadCost = cacheReadCost.Float64
	m.CacheWriteCost = cacheWriteCost.Float64
	m.Reasoning = reasoning.Bool
	m.ToolCall = toolCall.Bool
	m.StructuredOutput = structuredOutput.Bool
	m.Attachment = attachment.Bool
	m.OpenWeights = openWeights.Bool
	if updatedAt.Valid {
		m.UpdatedAt = updatedAt.Time
	}
	m.ReasoningType = rt.String
	m.ReasoningLevels = rl.String
	m.ReasoningOutputLimits = rol.String
	m.Decision = decision.Int64 != 0 || decisionModality(modalities.String)
	m.Image = image.Int64 != 0 || imagegen.ImageModality(modalities.String)
	m.Video = video.Int64 != 0 || videogen.VideoModality(modalities.String)
	return &m, nil
}

// decisionModality reports whether a modalities JSON blob declares the
// "decision" output modality. It is the fallback marker for rows synced from
// an upstream that carries the modality but not an explicit decision flag.
func decisionModality(modalities string) bool {
	if modalities == "" {
		return false
	}
	var mm struct {
		Output []string `json:"output"`
	}
	if json.Unmarshal([]byte(modalities), &mm) != nil {
		return false
	}
	for _, o := range mm.Output {
		if o == "decision" {
			return true
		}
	}
	return false
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM models_catalog`).Scan(&n)
	return n, err
}

func (s *Store) List(filter string, providerFilter string, reasoningOnly bool, limit int, offset int) ([]models.CatalogModel, error) {
	where := "1=1"
	args := []interface{}{}
	if filter != "" {
		where += " AND (id LIKE ? OR name LIKE ? OR family LIKE ?)"
		like := "%" + filter + "%"
		args = append(args, like, like, like)
	}
	if providerFilter != "" {
		where += " AND provider = ?"
		args = append(args, providerFilter)
	}
	if reasoningOnly {
		where += " AND reasoning = " + db.BoolLit(true)
	}
	query := fmt.Sprintf(`SELECT `+catalogColumns+` FROM models_catalog WHERE %s ORDER BY provider, id LIMIT ? OFFSET ?`, where)
	args = append(args, limit, offset)
	rows, err := s.db.Query(db.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.CatalogModel
	for rows.Next() {
		m, err := scanCatalogModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, nil
}

// cacheKey scopes a cache entry to the resolution tier that produced it. Get and
// GetByShortID must not share a key: a negative entry written by an exact-id miss
// would otherwise short-circuit GetByShortID's short_id/LIKE tiers for the same
// spelling, which is exactly the miss those tiers exist to resolve.
func cacheKey(tier, id string) string { return tier + ":" + id }

func (s *Store) Get(id string) (*models.CatalogModel, error) {
	key := cacheKey("id", id)
	if m, found, ok := s.cache.get(key); ok {
		if !found {
			// Negative hit: a very recent miss on this exact id.
			return nil, sql.ErrNoRows
		}
		return m, nil
	}
	m, err := scanCatalogModel(s.db.QueryRow(db.Q(`SELECT `+catalogColumns+` FROM models_catalog WHERE id=?`), id))
	if err != nil {
		if err == sql.ErrNoRows {
			s.cache.putMiss(key)
		}
		return nil, err
	}
	s.cache.put(key, m)
	return m, nil
}

// GetByShortID resolves a bare or gateway-qualified model id to its catalog row.
//
// Tiers, cheapest first (this is on the hot path of every chat request and is
// called up to 3x per image request):
//
//  1. exact match on models_catalog.id (primary key);
//  2. exact match on models_catalog.short_id, the stored suffix after the last
//     '/' (migration 030, indexed) — a gateway-qualified id like
//     "oc1/muse-spark-1.3-contributor" strips to the bare slug and lands here;
//  3. lower(id) = lower(slug) — an index-only case-insensitive exact match
//     (idx_models_catalog_id_lower);
//  4. only now the containment scan (`LOWER(id) LIKE '%slug%'`), which cannot
//     use an index and is what every lookup used to do. It stays as the last
//     resort for dated/suffixed variants nobody enumerated.
//
// The historical ORDER BY on the containment query (priced rows first, then id)
// is kept so a zero-price reseller mirror still loses to a priced row.
func (s *Store) GetByShortID(shortID string) (*models.CatalogModel, error) {
	// Keyed by the ORIGINAL argument, whatever tier resolves it: the same
	// request id is what the guards ask about repeatedly.
	key := cacheKey("short", shortID)
	if m, found, ok := s.cache.get(key); ok {
		if !found {
			return nil, sql.ErrNoRows
		}
		return m, nil
	}
	if m, err := s.Get(shortID); err == nil {
		return m, nil
	}
	trimmed := strings.TrimSpace(shortID)
	if trimmed == "" {
		s.cache.putMiss(key)
		return nil, sql.ErrNoRows
	}
	// Exact short_id hit — the case this column exists for.
	if m, err := s.getByShortIDExact(trimmed); err == nil {
		s.cache.put(key, m)
		return m, nil
	}
	// Gateway-qualified ids carry the *gateway provider* prefix
	// ("oc1/muse-spark-1.3-contributor"), which never matches catalog ids (keyed
	// by models.dev provider namespace). Strip to the suffix before the suffix
	// match, otherwise every cost lookup misses and bills $0.
	if i := strings.LastIndex(trimmed, "/"); i >= 0 && i+1 < len(trimmed) {
		trimmed = trimmed[i+1:]
		if m, err := s.getByShortIDExact(trimmed); err == nil {
			s.cache.put(key, m)
			return m, nil
		}
	}
	// Case-insensitive exact match: indexable via idx_models_catalog_id_lower.
	if m, err := scanCatalogModel(s.db.QueryRow(db.Q(`SELECT `+catalogColumns+` FROM models_catalog WHERE LOWER(id)=LOWER(?) LIMIT 1`), trimmed)); err == nil {
		s.cache.put(key, m)
		return m, nil
	}
	// Last resort: dated/suffixed containment (full scan, no index can help a
	// leading-wildcard LIKE).
	m, err := scanCatalogModel(s.db.QueryRow(db.Q(`SELECT `+catalogColumns+` FROM models_catalog WHERE LOWER(id) LIKE LOWER(?) ESCAPE '\' ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`), "%/"+escapeLike(trimmed)))
	if err != nil {
		if err == sql.ErrNoRows {
			// Cache the full-chain miss so the second and third guard calls on
			// the same request do not each re-run the containment scan.
			s.cache.putMiss(key)
		}
		return nil, err
	}
	s.cache.put(key, m)
	return m, nil
}

// getByShortIDExact is the indexed tier of GetByShortID: an exact match on the
// stored short_id column. Rows written before migration 030's backfill (or by an
// out-of-band INSERT) have short_id NULL and simply miss here.
func (s *Store) getByShortIDExact(slug string) (*models.CatalogModel, error) {
	m, err := scanCatalogModel(s.db.QueryRow(db.Q(`SELECT `+catalogColumns+` FROM models_catalog WHERE short_id=? ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`), slug))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// NormalizeModelID strips reseller channel tags and routing prefixes,
// returning the bare upstream slug ("[aws][量] grok-4.6" -> "grok-4.6").
// Reseller upstreams decorate every model ID with billing-channel markers
// ("[Kiro3][正价] claude-opus-4-6 [不补]"); without stripping, no catalog
// lookup can match and discovery records zero context, pricing and
// capability flags. When several whitespace-separated tokens remain, the
// last one wins: decorations lead ("[tag] slug"), never trail, unbracketed.
func NormalizeModelID(id string) string {
	s := strings.TrimSpace(id)
	for {
		changed := false
		for strings.HasPrefix(s, "[") {
			end := strings.Index(s, "]")
			if end < 0 {
				break
			}
			s = strings.TrimSpace(s[end+1:])
			changed = true
		}
		for strings.HasSuffix(s, "]") {
			start := strings.LastIndex(s, "[")
			if start < 0 {
				break
			}
			s = strings.TrimSpace(s[:start])
			changed = true
		}
		// Gateway routing prefix ("oc1/slug", "ck-default/[aws] slug").
		if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
			if tail := strings.TrimSpace(s[i+1:]); tail != "" && tail != s {
				s = tail
				changed = true
			}
		}
		if !changed || s == "" {
			break
		}
	}
	if f := strings.Fields(s); len(f) > 1 {
		s = f[len(f)-1]
	}
	return s
}

// signalsThinking reports whether a model slug names a reasoning variant.
func signalsThinking(slug string) bool {
	return strings.Contains(strings.ToLower(slug), "think")
}

// ensureReasoning forces the reasoning flag on a catalog row matched for a
// thinking-named slug whose base row is not flagged (e.g. only the
// non-thinking base exists in the snapshot).
func ensureReasoning(m *models.CatalogModel) *models.CatalogModel {
	if m == nil {
		return nil
	}
	m.Reasoning = true
	if m.ReasoningType == "" || m.ReasoningType == "none" {
		m.ReasoningType = "toggle"
		m.ReasoningLevels = `["off","on"]`
		if m.ReasoningOutputLimits == "" {
			m.ReasoningOutputLimits = "{}"
		}
	}
	return m
}

// thinkAlternates proposes the alternate "-think"/"-thinking" spelling:
// the catalog snapshot mixes both ("x-think" vs upstream "x-thinking").
func thinkAlternates(slug string) []string {
	low := strings.ToLower(slug)
	if strings.HasSuffix(low, "-thinking") {
		return []string{slug[:len(slug)-3]}
	}
	if strings.HasSuffix(low, "-think") {
		return []string{slug + "ing"}
	}
	return nil
}

// cutThinkingSuffix strips a trailing "-thinking"/"-think", reporting the base.
func cutThinkingSuffix(slug string) (string, bool) {
	low := strings.ToLower(slug)
	if strings.HasSuffix(low, "-thinking") {
		return slug[:len(slug)-len("-thinking")], true
	}
	if strings.HasSuffix(low, "-think") {
		return slug[:len(slug)-len("-think")], true
	}
	return "", false
}

// vAlternates proposes "-vN" <-> "-N" spelling variants
// ("deepseek-3.2" vs catalog "deepseek-v3.2").
func vAlternates(slug string) []string {
	var out []string
	if stripped := stripVPrefix(slug); stripped != slug {
		out = append(out, stripped)
	}
	if inserted := insertVPrefix(slug); inserted != slug {
		out = append(out, inserted)
	}
	return out
}

func stripVPrefix(slug string) string {
	var b strings.Builder
	for i := 0; i < len(slug); {
		if slug[i] == '-' && i+2 < len(slug) && slug[i+1] == 'v' && slug[i+2] >= '0' && slug[i+2] <= '9' {
			b.WriteByte('-')
			i += 2
			continue
		}
		b.WriteByte(slug[i])
		i++
	}
	return b.String()
}

func insertVPrefix(slug string) string {
	for i := 0; i+1 < len(slug); i++ {
		if slug[i] == '-' && slug[i+1] >= '0' && slug[i+1] <= '9' {
			return slug[:i+1] + "v" + slug[i+1:]
		}
	}
	return slug
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// FindBestMatch resolves a (possibly reseller-tagged) model ID to the closest
// catalog row. It returns the match kind:
//
//	"exact"      — verbatim or provider-suffix match, same as Get/GetByShortID
//	"normalized" — match after stripping channel tags ("[aws] grok-4.6")
//	"wildcard"   — approximate: think/think spelling, "-vN" variant,
//	               thinking base, dated-version containment
//	               ("claude-opus-4-1" -> "…/claude-opus-4-1-20250805"),
//	               AIHubMix coding-plan prefix ("coding-glm-5.3" -> "…/glm-5.3"),
//	               coding-plan vendor infix ("coding-xiaomi-mimo-v2.6-pro" ->
//	               "…/mimo-v2.6-pro")
//	               or free-tier base ("coding-glm-5.3-free" -> "…/glm-5.3-free"
//	               or zeroed "…/glm-5.3"),
//	               umans channel prefix ("umans-glm-5.2" -> "…/glm-5.2")
//	               or umans lab-mode base ("umans-glm-5.3-flash-lab" ->
//	               "…/glm-5.3-flash")
//
// Callers should record "wildcard" distinctly (provider_models.source =
// "enriched-wildcard") so estimated pricing is distinguishable from exact
// catalog data. Unknown slugs return sql.ErrNoRows.
func (s *Store) FindBestMatch(modelID string) (*models.CatalogModel, string, error) {
	free := isFreeSlug(modelID)
	if m, sub := s.trySlug(modelID, signalsThinking(modelID)); m != nil {
		if sub == "" {
			// Verbatim hit on a *-free row already carries zero pricing;
			// a verbatim hit without it cannot happen for free slugs.
			return m, "exact", nil
		}
		if free {
			m = zeroFreeCosts(m)
		}
		return m, "wildcard", nil
	}
	norm := NormalizeModelID(modelID)
	if norm == "" {
		return nil, "", sql.ErrNoRows
	}
	thinking := signalsThinking(norm)
	normFree := isFreeSlug(norm)
	if norm != strings.TrimSpace(modelID) {
		if m, sub := s.trySlug(norm, thinking); m != nil {
			if sub == "" {
				return m, "normalized", nil
			}
			if normFree || free {
				m = zeroFreeCosts(m)
			}
			return m, "wildcard", nil
		}
	}
	// Dated-version containment ("claude-opus-4-1" -> "…-4-1-20250805").
	if len(norm) >= 4 {
		if m, err := s.getByContains(norm); err == nil {
			if thinking {
				m = ensureReasoning(m)
			}
			if normFree || free {
				m = zeroFreeCosts(m)
			}
			return m, "wildcard", nil
		}
		// Coding-plan slugs never contain-match (the catalog holds the base,
		// not the prefixed id), so retry containment on the stripped base:
		// ("coding-foo-9" -> "…/foo-9-20250101"). umans channel bases join the
		// retry only when versioned (a base with a digit): bare-word bases
		// ("umans-flash-lab" -> "flash") are channel product names and must
		// not contain-match unrelated rows ("…/gemini-3.5-flash").
		alts := codingFreeAlternates(norm)
		for _, alt := range umansAlternates(norm) {
			if strings.ContainsAny(alt, "0123456789") {
				alts = append(alts, alt)
			}
		}
		for _, alt := range alts {
			if len(alt) >= 4 {
				if m, err := s.getByContains(alt); err == nil {
					if thinking {
						m = ensureReasoning(m)
					}
					if normFree || free {
						m = zeroFreeCosts(m)
					}
					return m, "wildcard", nil
				}
			}
		}
	}
	// Segment backoff for suffixed variants ("gemini-3.5-flash-high" ->
	// "gemini-3.5-flash", "glm-5.2-venice" -> "glm-5.2").
	for cand := backoffSegment(norm); len(cand) >= 6; cand = backoffSegment(cand) {
		if m, _ := s.trySlug(cand, thinking); m != nil {
			if thinking {
				m = ensureReasoning(m)
			}
			if normFree || free {
				m = zeroFreeCosts(m)
			}
			return m, "wildcard", nil
		}
	}
	return nil, "", sql.ErrNoRows
}

// trySlug runs the per-slug match tiers: verbatim/suffix hits return
// subkind "", approximate (think spelling, "-vN" variant, thinking base,
// coding-plan prefix, free-tier base, umans channel/lab-mode base) hits
// return "wild".
func (s *Store) trySlug(slug string, thinking bool) (*models.CatalogModel, string) {
	if m, err := s.Get(slug); err == nil {
		return m, ""
	}
	if m, err := s.GetByShortID(slug); err == nil {
		return m, ""
	}
	for _, alt := range thinkAlternates(slug) {
		if m, err := s.GetByShortID(alt); err == nil {
			return ensureReasoning(m), "wild"
		}
	}
	for _, alt := range vAlternates(slug) {
		if m, err := s.GetByShortID(alt); err == nil {
			if thinking {
				m = ensureReasoning(m)
			}
			return m, "wild"
		}
	}
	// AIHubMix coding-plan channel ("coding-glm-5.3" -> "glm-5.3"), umans
	// channel ("umans-glm-5.2" -> "glm-5.2", "umans-glm-5.3-flash-lab" ->
	// "glm-5.3-flash") and free-tier base ("x-free" -> "x"). Each alternate
	// also tries the "-vN" spellings, since the strip can expose a
	// version-variant base ("coding-deepseek-3.2" -> "deepseek-v3.2").
	free := isFreeSlug(slug)
	for _, alt := range append(codingFreeAlternates(slug), umansAlternates(slug)...) {
		if m, err := s.GetByShortID(alt); err == nil {
			if thinking {
				m = ensureReasoning(m)
			}
			if free {
				m = zeroFreeCosts(m)
			}
			return m, "wild"
		}
		for _, valt := range vAlternates(alt) {
			if m, err := s.GetByShortID(valt); err == nil {
				if thinking {
					m = ensureReasoning(m)
				}
				if free {
					m = zeroFreeCosts(m)
				}
				return m, "wild"
			}
		}
	}
	if base, ok := cutThinkingSuffix(slug); ok && base != "" {
		if m, err := s.GetByShortID(base); err == nil {
			return ensureReasoning(m), "wild"
		}
		for _, alt := range vAlternates(base) {
			if m, err := s.GetByShortID(alt); err == nil {
				return ensureReasoning(m), "wild"
			}
		}
		if m, err := s.getByContains(base); err == nil {
			return ensureReasoning(m), "wild"
		}
	}
	return nil, ""
}

// stripCodingPrefix removes the AIHubMix coding-plan channel prefix
// ("coding-glm-5.3" -> "glm-5.3"). Only the coding- namespace observed in
// models.dev is handled; matching stays case-insensitive like the rest of
// the lookup chain. Reports whether a prefix was stripped.
func stripCodingPrefix(slug string) (string, bool) {
	if len(slug) > len("coding-") && strings.HasPrefix(strings.ToLower(slug), "coding-") {
		return slug[len("coding-"):], true
	}
	return slug, false
}

// stripCodingVendor removes a vendor infix from an AIHubMix coding-plan slug
// ("xiaomi-mimo-v2.6-pro" -> "mimo-v2.6-pro"). The coding channel embeds the
// model vendor name, but models.dev keys the base rows without it
// ("xiaomi/mimo-v2.6-pro"), so without this the lookup misses and the model
// stays unenriched. Only known vendor infixes are stripped; matching stays
// case-insensitive like the rest of the lookup chain. Reports whether a
// prefix was stripped.
func stripCodingVendor(slug string) (string, bool) {
	lower := strings.ToLower(slug)
	for _, v := range []string{"xiaomi-"} {
		if len(slug) > len(v) && strings.HasPrefix(lower, v) {
			return slug[len(v):], true
		}
	}
	return slug, false
}

// stripFreeSuffix removes the free-tier suffix ("glm-5.3-free" -> "glm-5.3").
// Reports whether a suffix was stripped.
func stripFreeSuffix(slug string) (string, bool) {
	if len(slug) > len("-free") && strings.HasSuffix(strings.ToLower(slug), "-free") {
		return slug[:len(slug)-len("-free")], true
	}
	return slug, false
}

// stripUmansPrefix removes the umans channel model prefix
// ("umans-glm-5.2" -> "glm-5.2"). The umans provider namespaces every model
// id with "umans-", but models.dev keys the underlying base rows without it
// ("zai/glm-5.2"), so without stripping, lookup misses and the model stays
// unenriched. Matching stays case-insensitive like the rest of the lookup
// chain. Reports whether a prefix was stripped.
func stripUmansPrefix(slug string) (string, bool) {
	if len(slug) > len("umans-") && strings.HasPrefix(strings.ToLower(slug), "umans-") {
		return slug[len("umans-"):], true
	}
	return slug, false
}

// stripLabSuffix removes the umans lab-mode suffix
// ("umans-glm-5.3-flash-lab" -> "umans-glm-5.3-flash"). Lab-mode ids wrap the
// base with the "umans-" prefix and this "-lab" suffix; models.dev keys the
// base row alone ("zai/glm-5.3-flash"). Matching stays case-insensitive like
// the rest of the lookup chain. Reports whether a suffix was stripped.
func stripLabSuffix(slug string) (string, bool) {
	if len(slug) > len("-lab") && strings.HasSuffix(strings.ToLower(slug), "-lab") {
		return slug[:len(slug)-len("-lab")], true
	}
	return slug, false
}

// isFreeSlug reports whether a slug names a free-tier variant.
func isFreeSlug(slug string) bool {
	_, ok := stripFreeSuffix(strings.TrimSpace(slug))
	return ok
}

// zeroFreeCosts zeroes pricing on a fallback match for a *-free slug so a
// paid base row never bills free-tier traffic. Exact *-free catalog rows
// already carry zero costs; this only affects approximate fallbacks
// ("coding-glm-5.3-free" -> "…/glm-5.3").
func zeroFreeCosts(m *models.CatalogModel) *models.CatalogModel {
	if m == nil {
		return nil
	}
	m.InputCost, m.OutputCost = 0, 0
	m.CacheReadCost, m.CacheWriteCost = 0, 0
	return m
}

// codingFreeAlternates proposes stripped candidates for AIHubMix coding-plan
// slugs, in priority order. Chained variants come last:
// ("coding-glm-5.3-free" -> ["coding-glm-5.3", "glm-5.3-free", "glm-5.3"]).
// Vendor-infixed slugs additionally propose the bare base, since models.dev
// keys base rows without the vendor name:
// ("coding-xiaomi-mimo-v2.6-pro" -> [..., "xiaomi-mimo-v2.6-pro",
// "mimo-v2.6-pro"]).
func codingFreeAlternates(slug string) []string {
	var out []string
	seen := map[string]bool{slug: true}
	add := func(cand string) {
		if cand != "" && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
	}
	if noFree, ok := stripFreeSuffix(slug); ok {
		add(noFree)
	}
	if noCoding, ok := stripCodingPrefix(slug); ok {
		add(noCoding)
	}
	if noFree, ok := stripFreeSuffix(slug); ok {
		if both, ok2 := stripCodingPrefix(noFree); ok2 {
			add(both)
		}
	}
	if noCoding, ok := stripCodingPrefix(slug); ok {
		if both, ok2 := stripFreeSuffix(noCoding); ok2 {
			add(both)
		}
	}
	// Bare-base fallbacks for vendor-infixed coding slugs. Each candidate
	// whose coding- prefix is stripped may still carry the vendor infix, so
	// propose the base too. These come last so exact coding rows and plain
	// coding-stripped rows keep priority.
	for _, cand := range append([]string{slug}, out...) {
		stripped := cand
		if noCoding, ok := stripCodingPrefix(stripped); ok {
			stripped = noCoding
		}
		if base, ok := stripCodingVendor(stripped); ok {
			add(base)
			if noFree, ok := stripFreeSuffix(base); ok {
				add(noFree)
			}
		}
	}
	return out
}

// umansAlternates proposes base slugs for umans channel ids in priority
// order — minimal stripping first, so the channel's own rows stay preferred
// ("umans-glm-5.3-flash-lab" -> "umans-glm-5.3-flash", the non-lab sibling,
// then "glm-5.3-flash-lab", then the bare base "glm-5.3-flash"). Plain
// channel ids propose the bare base ("umans-glm-5.2" -> "glm-5.2").
func umansAlternates(slug string) []string {
	var out []string
	seen := map[string]bool{slug: true}
	add := func(cand string) {
		if cand != "" && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
	}
	noUmans, isUmans := stripUmansPrefix(slug)
	noLab, isLab := stripLabSuffix(slug)
	if isLab {
		add(noLab)
	}
	if isUmans {
		add(noUmans)
	}
	if isUmans && isLab {
		if base, ok := stripLabSuffix(noUmans); ok {
			add(base)
		}
	}
	return out
}

// backoffSegment drops the last dash-separated segment
// ("gemini-3.5-flash-high" -> "gemini-3.5-flash").
func backoffSegment(slug string) string {
	if i := strings.LastIndex(slug, "-"); i > 0 {
		return slug[:i]
	}
	return ""
}

// getByContains matches dated or suffixed catalog variants of a slug,
// preferring priced rows then the shortest (closest-version) ID.
// Case-insensitive: reseller slugs do not always match snapshot casing.
func (s *Store) getByContains(slug string) (*models.CatalogModel, error) {
	pat := "%" + escapeLike(slug) + "%"
	return scanCatalogModel(s.db.QueryRow(db.Q(`SELECT `+catalogColumns+` FROM models_catalog WHERE LOWER(id) LIKE LOWER(?) ESCAPE '\' ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), LENGTH(id), id LIMIT 1`), pat))
}

func (s *Store) FetchAndSync() (int, error) {
	resp, err := s.client.Get(ModelsDevURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}
	// limit to 20MB
	resp.Body = io.NopCloser(io.LimitReader(resp.Body, 20<<20))
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var providers map[string]RawProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return 0, err
	}
	return s.syncFromProviders(providers)
}

func (s *Store) SyncFromBytes(body []byte) (int, error) {
	var providers map[string]RawProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return 0, err
	}
	return s.syncFromProviders(providers)
}

// catalogInsertColumns is the canonical column list for a models_catalog
// INSERT. `short_id` (migration 030) is written on every path so the lookup can
// be an index hit instead of a containment scan.
const catalogInsertColumns = `id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits, decision, image, video, short_id`

const catalogInsertPlaceholders = `?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?`

// catalogUpsertColumns are the columns a models.dev sync overwrites. For that
// path upstream is the source of truth, so a full overwrite is defensible — the
// curated seeds below deliberately do NOT do this (see seedRowsInto).
var catalogUpsertColumns = []string{"provider", "name", "description", "family", "context_window", "max_output", "input_cost", "output_cost", "cache_read_cost", "cache_write_cost", "reasoning", "tool_call", "structured_output", "attachment", "modalities", "open_weights", "knowledge_cutoff", "updated_at", "reasoning_type", "reasoning_levels", "reasoning_output_limits", "decision", "image", "video", "short_id"}

// curatedSeedVersionKey is the system_config marker recording which version of
// the curated registries has already been seeded. It is a content hash, so an
// unchanged registry writes nothing at all (not even 34 no-op upserts) and a
// changed one only has to insert the rows the operator does not have yet.
const curatedSeedVersionKey = "curated_seed_version"

// CostFor computes cost USD given tokens and model
func CostFor(m *models.CatalogModel, promptTokens, completionTokens int) float64 {
	if m == nil {
		return 0
	}
	return float64(promptTokens)*m.InputCost/1_000_000 + float64(completionTokens)*m.OutputCost/1_000_000
}

// CacheSplit is the normalized billing split of a request's input tokens:
// every input token lands in exactly one bucket, and the three add up to the
// true total input. Normalizing at extraction time is what lets the pricing
// below stay provider-agnostic, because the two upstream families disagree on
// how cached tokens are reported:
//
//	OpenAI      prompt_tokens INCLUDES prompt_tokens_details.cached_tokens
//	            (a subset), so Fresh = prompt_tokens - cached.
//	Anthropic   input_tokens EXCLUDES cache_read_input_tokens and
//	            cache_creation_input_tokens (siblings), so Fresh = input_tokens
//	            and the cache counts are additional.
//
// Getting this wrong double-charges one family and under-charges the other,
// so the split is decided where the field names are still visible.
type CacheSplit struct {
	Fresh      int
	CacheRead  int
	CacheWrite int
	Completion int
}

// TotalInput is the true input token count across all three input buckets.
func (c CacheSplit) TotalInput() int { return c.Fresh + c.CacheRead + c.CacheWrite }

// Any reports whether the request carried any usage at all.
func (c CacheSplit) Any() bool { return c.TotalInput()+c.Completion > 0 }

// CostForSplit prices a request using cache-aware rates.
//
// Cached input is billed at the model's cache-read rate and cache writes at
// its cache-write rate; the remainder at the standard input rate. When a rate
// is absent from the catalog (0) the corresponding tokens fall back to the
// standard input rate — the conservative choice, since assuming a discount
// that does not exist understates spend and silently under-counts budget
// consumption. That fallback also preserves pre-existing behaviour for models
// the catalog carries no cache pricing for.
func CostForSplit(m *models.CatalogModel, s CacheSplit) float64 {
	if m == nil {
		return 0
	}
	const perM = 1_000_000.0
	cacheReadRate := m.CacheReadCost
	if cacheReadRate <= 0 {
		cacheReadRate = m.InputCost
	}
	cacheWriteRate := m.CacheWriteCost
	if cacheWriteRate <= 0 {
		cacheWriteRate = m.InputCost
	}
	return float64(s.Fresh)*m.InputCost/perM +
		float64(s.CacheRead)*cacheReadRate/perM +
		float64(s.CacheWrite)*cacheWriteRate/perM +
		float64(s.Completion)*m.OutputCost/perM
}

func containsSlash(s string) bool {
	return strings.Contains(s, "/")
}

func parseReasoningOptions(m RawModel) (string, string, string) {
	if !m.Reasoning {
		return "none", "[]", "{}"
	}
	if len(m.ReasoningOptions) == 0 {
		// reasoning without options -> single toggle
		b, _ := json.Marshal([]string{"on"})
		return "toggle", string(b), "{}"
	}
	// prefer first option
	opt := m.ReasoningOptions[0]
	if opt.Type == "toggle" {
		b, _ := json.Marshal([]string{"off", "on"})
		return "toggle", string(b), "{}"
	}
	// effort type
	if len(opt.Values) > 0 {
		b, _ := json.Marshal(opt.Values)
		// default per-level output limits as empty map, UI will allow setting
		return opt.Type, string(b), "{}"
	}
	b, _ := json.Marshal([]string{"low", "medium", "high"})
	return "effort", string(b), "{}"
}

func (s *Store) syncFromProviders(providers map[string]RawProvider) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(db.Q(`INSERT INTO models_catalog(`+catalogInsertColumns+`) VALUES(`+catalogInsertPlaceholders+`)`) + db.UpsertEnd([]string{"id"}, catalogUpsertColumns))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	count := 0
	now := time.Now().UTC()
	for provID, prov := range providers {
		for _, m := range prov.Models {
			modalitiesBytes, _ := json.Marshal(m.Modalities)
			fullID := m.ID
			if len(fullID) < 3 || !containsSlash(fullID) {
				fullID = provID + "/" + m.ID
			}
			ctx := m.Limit.Context
			if ctx == 0 {
				ctx = m.Limit.Input
			}
			out := m.Limit.Output
			reasoningType, reasoningLevelsJSON, reasoningLimitsJSON := parseReasoningOptions(m)
			isDecision := m.Decision || decisionModality(string(modalitiesBytes)) || decision.IsDecisionModel(fullID)
			// models.dev's `modalities.output` IS the image-generation tag: it is
			// authoritative whenever present, so `image` and the emitted
			// modalities can never contradict each other. The curated registry is
			// consulted only when models.dev carries no modality data for the row
			// (a brand-new model, or a listing that omits modalities entirely).
			// ORing the two unconditionally used to flag reseller rows that
			// models.dev reports as text-only (`output: ["text"]`), producing
			// rows that claimed image output while advertising text output.
			var isImage bool
			if len(imagegen.OutputModalities(string(modalitiesBytes))) > 0 {
				isImage = imagegen.ImageModality(string(modalitiesBytes))
			} else {
				isImage = imagegen.IsImageModel(fullID)
			}
			isVideo := videogen.VideoModality(string(modalitiesBytes))
			if _, err := stmt.Exec(fullID, provID, m.Name, m.Description, m.Family, ctx, out, m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite, m.Reasoning, m.ToolCall, m.StructuredOutput, m.Attachment, string(modalitiesBytes), m.OpenWeights, m.Knowledge, now, reasoningType, reasoningLevelsJSON, reasoningLimitsJSON, db.BoolInt(isDecision), db.BoolInt(isImage), db.BoolInt(isVideo), db.ShortID(fullID)); err != nil {
				return count, err
			}
			count++
		}
	}
	// Curated rows are seeded INSIDE the sync transaction so a sync cannot leave
	// the models.dev rows committed while the curated rows failed, and so a seed
	// failure surfaces instead of being swallowed by `_, _ =`. This was P1-11:
	// the seeds used to run after Commit() on s.db with their errors discarded,
	// so a Postgres-only failure (the pq integer/bool class) silently left a
	// deployment with zero image and video models until the next daily sync.
	// (A zero return here means the curated_seed_version marker matched, i.e.
	// nothing needed writing — not an error.)
	if _, err := seedAllTx(tx); err != nil {
		return count, fmt.Errorf("curated model seed: %w", err)
	}
	nowValue := now.Format(time.RFC3339)
	if _, err := tx.Exec(db.Q(`INSERT INTO system_config(key,value,updated_at) VALUES('models_last_sync',?,?)`)+db.UpsertEnd([]string{"key"}, []string{"value", "updated_at"}), nowValue, now); err != nil {
		return count, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Every catalog row may have changed: drop the memo.
	s.InvalidateCache()
	return count, nil
}

// catalogSeedRow is one curated registry entry in the shape models_catalog
// stores. It exists so the three registries (decision / image / video) share a
// single INSERT path instead of three copy-pasted ones that drifted apart.
type catalogSeedRow struct {
	ID            string
	Provider      string
	Name          string
	Description   string
	Family        string
	ContextWindow int
	MaxOutput     int
	InputCost     float64
	OutputCost    float64
	Reasoning     bool
	ToolCall      bool
	StructuredOut bool
	Attachment    bool
	Modalities    string
	OpenWeights   bool
	Decision      bool
	Image         bool
	Video         bool
}

// decisionSeedRows projects the curated System One / decision registry.
func decisionSeedRows() []catalogSeedRow {
	all := decision.All()
	rows := make([]catalogSeedRow, 0, len(all))
	for _, dm := range all {
		rows = append(rows, catalogSeedRow{
			ID:            dm.CatalogID(),
			Provider:      dm.Namespace,
			Name:          dm.Name,
			Description:   dm.Description(),
			Family:        dm.Family,
			ContextWindow: dm.ContextWindow,
			InputCost:     dm.InputCostPerM,
			OutputCost:    dm.OutputCostPerM,
			Reasoning:     false,
			ToolCall:      false,
			StructuredOut: true,
			Attachment:    dm.Vision,
			Modalities:    dm.ModalitiesJSON(),
			OpenWeights:   dm.Family != "jev",
			Decision:      true,
		})
	}
	return rows
}

// imageSeedRows projects the curated image-generation registry. Image models
// bill per generated image, so the per-token columns are written as literal
// zeros: the registry carries InputCostPerM/OutputCostPerM for providers that
// price images per token, but writing them here would let CostForSplit price an
// image row as if it were a chat model.
func imageSeedRows() []catalogSeedRow {
	all := imagegen.All()
	rows := make([]catalogSeedRow, 0, len(all))
	for _, im := range all {
		rows = append(rows, catalogSeedRow{
			ID:          im.CatalogID(),
			Provider:    im.Namespace,
			Name:        im.Name,
			Description: im.Description(),
			Family:      im.Family,
			Reasoning:   false,
			ToolCall:    false,
			Attachment:  im.SupportsEdit,
			Modalities:  im.ModalitiesJSON(),
			OpenWeights: false,
			Image:       true,
		})
	}
	return rows
}

// videoSeedRows projects the curated video-generation registry. Video models
// bill per generated second; the per-token columns are zero for the same reason
// as the image rows.
func videoSeedRows() []catalogSeedRow {
	all := videogen.All()
	rows := make([]catalogSeedRow, 0, len(all))
	for _, vm := range all {
		rows = append(rows, catalogSeedRow{
			ID:          vm.CatalogID(),
			Provider:    vm.Namespace,
			Name:        vm.Name,
			Description: vm.Description(),
			Family:      vm.Family,
			Reasoning:   false,
			ToolCall:    false,
			Attachment:  vm.SupportsImageInput,
			Modalities:  vm.ModalitiesJSON(),
			OpenWeights: false,
			Video:       true,
		})
	}
	return rows
}

// curatedSeedRows is every curated row in one slice (decision, then image, then
// video).
func curatedSeedRows() []catalogSeedRow {
	rows := decisionSeedRows()
	rows = append(rows, imageSeedRows()...)
	rows = append(rows, videoSeedRows()...)
	return rows
}

// curatedSeedVersion is a content hash of the curated registries. It is stored in
// system_config so an unchanged binary writes nothing on boot or on sync, and
// bumps automatically whenever a registry entry changes (no hand-maintained
// version number to forget).
func curatedSeedVersion(rows []catalogSeedRow) string {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%.6f\x00%.6f\x00%s\x00%t%t%t%t%t\n",
			r.ID, r.Provider, r.Name, r.Family, r.ContextWindow, r.InputCost, r.OutputCost,
			r.Modalities, r.Decision, r.Image, r.Video, r.Attachment, r.OpenWeights)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// seedExecer is the subset of *sql.DB / *sql.Tx the seeder needs.
type seedExecer interface {
	Prepare(query string) (*sql.Stmt, error)
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// seedRowsInto writes curated rows with INSERT ... ON CONFLICT(id) DO NOTHING.
//
// This is the P0-4 fix. The previous statement was `ON CONFLICT(id) DO UPDATE
// SET` over every column — including input_cost, output_cost, context_window and
// the capability flags — so an operator's negotiated price was reverted to the
// baked-in value on every boot and every 24h sync (proven: 2.40 -> 0.24), and an
// operator's capability correction was silently re-flagged. That is a
// spend-attribution bug: CostForSplit and /v1/models image_cost/video_cost all
// read these columns, so under-reporting lets a key blow its budget.
//
// Insert-only means: seed rows the database does not have yet, never fight the
// operator over rows it does.
func seedRowsInto(ex seedExecer, rows []catalogSeedRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	stmt, err := ex.Prepare(db.Q(`INSERT INTO models_catalog(`+catalogInsertColumns+`) VALUES(`+catalogInsertPlaceholders+`)`) + db.InsertIgnoreEnd([]string{"id"}))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	n := 0
	for _, r := range rows {
		res, err := stmt.Exec(r.ID, r.Provider, r.Name, r.Description, r.Family, r.ContextWindow, r.MaxOutput,
			r.InputCost, r.OutputCost, 0, 0,
			r.Reasoning, r.ToolCall, r.StructuredOut, r.Attachment, r.Modalities, r.OpenWeights, "", now,
			"none", "[]", "{}", db.BoolInt(r.Decision), db.BoolInt(r.Image), db.BoolInt(r.Video), db.ShortID(r.ID))
		if err != nil {
			return n, err
		}
		// RowsAffected is 0 for an INSERT ... DO NOTHING that hit an existing
		// row, which is what makes "seeded N new rows" meaningful.
		if affected, aerr := res.RowsAffected(); aerr == nil {
			n += int(affected)
		}
	}
	return n, nil
}

// seedAllTx seeds all three curated registries inside an existing transaction,
// gated by the curated_seed_version marker so an unchanged registry writes
// nothing. A seed error is returned to the caller (which rolls the transaction
// back) instead of being swallowed.
func seedAllTx(tx *sql.Tx) (int, error) {
	rows := curatedSeedRows()
	version := curatedSeedVersion(rows)
	var stored string
	err := tx.QueryRow(db.Q(`SELECT value FROM system_config WHERE key=?`), curatedSeedVersionKey).Scan(&stored)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if err == nil && stored == version {
		// Registry unchanged since the last seed: nothing to do at all.
		return 0, nil
	}
	n, err := seedRowsInto(tx, rows)
	if err != nil {
		return n, err
	}
	// Record the marker. DO UPDATE here is deliberate and safe: it is our own
	// bookkeeping key, not operator data.
	if _, err := tx.Exec(db.Q(`INSERT INTO system_config(key,value,updated_at) VALUES(?,?,?)`)+db.UpsertEnd([]string{"key"}, []string{"value", "updated_at"}), curatedSeedVersionKey, version, time.Now().UTC()); err != nil {
		return n, err
	}
	return n, nil
}

// SeedAllCuratedModels seeds the curated decision, image AND video registries in
// ONE transaction, and is the single entry point a boot path should call.
//
// Why this exists (P1-11): the three seeds used to be reachable only through the
// daily models.dev sync (`SeedDecisionModels` additionally ran at boot), so a
// fresh deploy that could not reach models.dev listed 0 image/video models for
// up to 24h even though both registries are embedded in the binary. A single
// combined seeder is ~1.6 ms, idempotent, and insert-only.
//
// Intended call site (cmd/gateway/main.go is owned by another change, so this is
// exported rather than wired): replace the boot-time
// `catalogStore.SeedDecisionModels()` call (~main.go:107) with
//
//	if n, err := catalogStore.SeedAllCuratedModels(); err != nil {
//		log.Warn().Err(err).Msg("curated model seed failed")
//	} else if n > 0 {
//		log.Debug().Int("models", n).Msg("curated models seeded")
//	}
//
// It is safe to call on every boot: an unchanged registry writes nothing (see the
// curated_seed_version marker) and existing rows are never modified.
func (s *Store) SeedAllCuratedModels() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := seedAllTx(tx)
	if err != nil {
		return n, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.InvalidateCache()
	return n, nil
}

// SeedDecisionModels seeds the curated System One / decision-model registry.
//
// Insert-only and idempotent: rows the database already has (including any the
// operator has edited — price, capability flags, name) are left untouched, so a
// re-seed can never revert an operator override. Prefer SeedAllCuratedModels at
// boot; this stays exported because it is the narrow entry point for tests and
// for callers that deliberately want only the decision rows.
func (s *Store) SeedDecisionModels() (int, error) {
	return s.seedRowsStandalone(decisionSeedRows())
}

// SeedImageModels seeds the curated image-generation registry. Insert-only;
// token costs are written as zero because image models bill per image.
func (s *Store) SeedImageModels() (int, error) {
	return s.seedRowsStandalone(imageSeedRows())
}

// SeedVideoModels seeds the curated video-generation registry. Insert-only;
// token costs are written as zero because video models bill per second.
func (s *Store) SeedVideoModels() (int, error) {
	return s.seedRowsStandalone(videoSeedRows())
}

// seedRowsStandalone runs one registry seed in a single transaction (one Prepare
// and one Commit instead of 10-14 autocommit statements) and propagates the
// first error instead of discarding it.
func (s *Store) seedRowsStandalone(rows []catalogSeedRow) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := seedRowsInto(tx, rows)
	if err != nil {
		return n, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.InvalidateCache()
	return n, nil
}
