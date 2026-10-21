package proxy

// Per-request attribution: which human/agent a request belongs to, and which
// application sent it. This is the dimension the gateway was missing — it
// attributed usage only to the gateway key, and one key can front many users.
//
// Sources, in precedence order:
//
//	end_user  X-End-User header  >  body "user" (OpenAI)  >  body "metadata.user_id" (Anthropic)
//	app       X-App-Id header    >  X-Title / X-OpenRouter-Title  >  HTTP-Referer
//
// The body `user` field matters because OpenAI clients already send it, so a
// client that does nothing at all still gets per-end-user attribution — the
// same zero-plumbing route APISIX takes with llm_end_user_id. The headers
// exist for callers that want attribution without a body field, and the
// referer/title pair mirrors OpenRouter's app-attribution headers so an
// OpenRouter-shaped client works unchanged.
//
// Both values are attacker-influenced, so every write is length-capped and
// control characters are stripped before storage.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// HeaderEndUser names the end user explicitly, overriding any body field.
	HeaderEndUser = "X-End-User"
	// HeaderAppID names the calling application explicitly.
	HeaderAppID = "X-App-Id"
	// HeaderTitle / HeaderReferer mirror OpenRouter's attribution headers so a
	// client written against OpenRouter attributes correctly here unchanged.
	HeaderTitle       = "X-Title"
	HeaderTitleAlt    = "X-OpenRouter-Title"
	HeaderReferer     = "HTTP-Referer"
	HeaderRefererAlt  = "Referer"
	maxAttributionLen = 200
)

// Attribution is the per-request identity beyond the gateway key. Empty fields
// mean "not supplied" and are stored as SQL NULL, not as empty strings, so
// reports can tell "unattributed" apart from "attributed to an empty value".
type Attribution struct {
	EndUser string
	App     string
}

// Any reports whether anything was attributed.
func (a Attribution) Any() bool { return a.EndUser != "" || a.App != "" }

type attributionCtxKey struct{}

// WithAttribution stores the request's attribution in its context.
func WithAttribution(ctx context.Context, a Attribution) context.Context {
	if !a.Any() {
		return ctx
	}
	return context.WithValue(ctx, attributionCtxKey{}, a)
}

// AttributionFrom reads the attribution stored by WithAttribution. A request
// that was never attributed yields the zero value, which is the common case
// for callers that send no attribution at all.
func AttributionFrom(ctx context.Context) Attribution {
	a, _ := ctx.Value(attributionCtxKey{}).(Attribution)
	return a
}

// attributionFromRequest extracts attribution from headers and, for the body
// fields, from an already-parsed JSON body. body may be nil.
func attributionFromRequest(r *http.Request, body []byte) Attribution {
	var a Attribution
	if r != nil {
		a.EndUser = firstNonEmpty(headersGet(r, HeaderEndUser))
		a.App = firstNonEmpty(headersGet(r, HeaderAppID), headersGet(r, HeaderTitle), headersGet(r, HeaderTitleAlt), appFromReferer(r))
	}
	if len(body) > 0 {
		if a.EndUser == "" {
			a.EndUser = endUserFromBody(body)
		}
	}
	a.EndUser = sanitizeAttribution(a.EndUser)
	a.App = sanitizeAttribution(a.App)
	return a
}

func headersGet(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.Header.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// endUserFromBody reads the OpenAI top-level "user" field, falling back to
// Anthropic's metadata.user_id. Both are cheap: the body is already parsed on
// these paths, and a nil/odd shape simply yields "".
func endUserFromBody(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if v, ok := m["user"].(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	// Anthropic carries the caller's identifier under metadata.
	if md, ok := m["metadata"].(map[string]any); ok {
		if v, ok := md["user_id"].(string); ok {
			return v
		}
	}
	return ""
}

// appFromReferer reduces a referer to its origin. Distinct paths on one site
// are the same application, and collapsing them keeps the app dimension from
// fragmenting per page.
func appFromReferer(r *http.Request) string {
	ref := firstNonEmpty(r.Header.Get(HeaderReferer), r.Header.Get(HeaderRefererAlt))
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		// Not a parseable URL (some clients send a bare name) — keep it whole
		// rather than dropping attribution on the floor.
		return ref
	}
	return u.Scheme + "://" + u.Host
}

// sanitizeAttribution bounds an attacker-influenced value before storage:
// trims, drops control characters, and truncates on a rune boundary. Without
// the cap, a client could write arbitrarily large strings across a column
// that is indexed. A byte-blind cut (s[:200]) can split a multi-byte UTF-8
// rune; Postgres then rejects the whole request_logs INSERT with "invalid
// byte sequence" and the row is silently lost — so the cap must stay
// rune-safe (truncateUTF8 backs off to the previous rune boundary).
func sanitizeAttribution(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > maxAttributionLen {
		cleaned = truncateUTF8(cleaned, maxAttributionLen)
	}
	if !utf8.ValidString(cleaned) {
		// Defensive: control stripping cannot split runes, but never store
		// invalid UTF-8 (Postgres text columns reject it).
		cleaned = strings.ToValidUTF8(cleaned, "")
	}
	return cleaned
}
