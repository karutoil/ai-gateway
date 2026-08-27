package audit

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/webhook"

	"github.com/google/uuid"
)

// Recorder is the Phase 1.6 audit scaffold. Phase 1.6 writes rows; Phase 3 enforces RBAC filtering.
type Recorder interface {
	Log(actor, action, targetType, targetID, meta string) error
}

type DBRecorder struct{ DB *sql.DB }

func NewDBRecorder(db *sql.DB) *DBRecorder { return &DBRecorder{DB: db} }

func (r *DBRecorder) Log(actor, action, targetType, targetID, meta string) error {
	if r.DB == nil {
		return nil
	}
	now := time.Now().UTC()
	_, err := r.DB.Exec(db.Q(`INSERT INTO audit_logs(id, actor, action, target_type, target_id, meta, created_at) VALUES(?,?,?,?,?,?,?)`),
		uuid.NewString(), actor, action, targetType, targetID, meta, now)
	// Webhook emit via webhook.Global when configured
	// Called from audit recorder so all audited writes fan out to webhook sink.
	if webhook.Global != nil {
		webhook.Global.Emit("audit."+action, map[string]any{
			"actor":       actor,
			"action":      action,
			"target_type": targetType,
			"target_id":   targetID,
			"meta":        meta,
			"created_at":  now.Format(time.RFC3339Nano),
		})
	}
	return err
}

var _ Recorder = (*DBRecorder)(nil)

// ShouldAudit reports whether a request should create an audit row (write methods on providers/keys/aliases).
func ShouldAudit(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
		return false
	}
	p := r.URL.Path
	return strings.Contains(p, "/providers") || strings.Contains(p, "/keys") || strings.Contains(p, "/aliases")
}

// targetFromPath derives a coarse target type from the URL path.
func targetFromPath(p string) string {
	switch {
	case strings.Contains(p, "/providers"):
		return "provider"
	case strings.Contains(p, "/keys"):
		return "key"
	case strings.Contains(p, "/aliases"):
		return "alias"
	default:
		return "unknown"
	}
}

// Middleware returns an HTTP middleware that writes an audit_logs row for
// POST/PUT/DELETE on providers/keys/aliases. Actor attribution order:
//  1. verified dashboard JWT subject (auth.GetSubject — spoof-proof),
//  2. verified gateway key prefix from the auth context,
//  3. "anonymous".
//
// The X-Actor / X-Gateway-Actor request headers are deliberately NOT trusted:
// any client could previously attribute its writes to another user.
func Middleware(rec Recorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ShouldAudit(r) {
				next.ServeHTTP(w, r)
				return
			}
			// Serve first so we only audit successful routing (still logs even on error for traceability).
			next.ServeHTTP(w, r)
			actor := actorFor(r)
			targetType := targetFromPath(r.URL.Path)
			meta := r.Method + " " + r.URL.Path
			_ = rec.Log(actor, strings.ToLower(r.Method), targetType, r.URL.Path, meta)
		})
	}
}

// actorFor resolves the acting identity from VERIFIED context only.
func actorFor(r *http.Request) string {
	if sub := auth.GetSubject(r); sub != "" {
		return sub
	}
	if k, ok := middleware.GatewayKeyFromContext(r.Context()); ok && k != nil && k.Prefix != "" {
		return "key:" + k.Prefix
	}
	return "anonymous"
}

// LogFromRequest is a helper for handlers that want explicit audit logging
// (e.g. when middleware is not wired). It is a no-op if rec is nil.
func LogFromRequest(rec Recorder, r *http.Request, targetID, meta string) {
	if rec == nil || !ShouldAudit(r) {
		return
	}
	actor := r.Header.Get("X-Actor")
	if actor == "" {
		actor = "admin"
	}
	_ = rec.Log(actor, strings.ToLower(r.Method), targetFromPath(r.URL.Path), targetID, meta)
}

// Wire is an alias for Middleware for callers that prefer Wire(rec) naming.
func Wire(rec Recorder) func(http.Handler) http.Handler { return Middleware(rec) }
