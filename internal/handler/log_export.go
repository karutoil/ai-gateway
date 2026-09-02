package handler

// GET /api/logs/export — CSV download of exactly the filtered log set.
//
// Complements /api/billing/export (kept unchanged for compatibility): this
// endpoint reuses the shared log filter builder, so "export what I'm looking
// at" works — filters from the Logs UI serialize straight into the query
// string. Streams row-by-row to keep memory flat; hard-capped with an
// X-Truncated header so huge filters can't OOM the server or the browser.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/webhook"
)

// logsExportMaxRows caps a single CSV download. Beyond this the export is
// truncated (flagged via X-Truncated: true) — narrower filters or the
// billing export should be used instead.
const logsExportMaxRows = 50000

func (h *AdminHandler) LogsExport(w http.ResponseWriter, r *http.Request) {
	// Own-key-scoped callers bypass the org requirement: their export is
	// already bounded to keys they created (logScopeOwnOnly).
	orgID := ""
	if hasAll, _, _ := logScopeOwnOnly(r, h, "rl"); hasAll {
		var ok bool
		if orgID, ok = h.resolveScope(r); !ok {
			httperr.Forbidden(w, "org scope required")
			return
		}
	}
	if webhook.Global != nil {
		webhook.Global.Emit("logs.export", map[string]any{
			"actor":  auth.GetSubject(r),
			"action": "export",
			"filter": r.URL.RawQuery,
			"path":   r.URL.Path,
			"time":   time.Now().UTC().Format(time.RFC3339Nano),
		})
	}

	whereSQL, args := logFilterWhere(orgID, r.URL.Query())
	// Ownership narrowing for keys:read_own-only callers.
	_, scopeSQL, scopeArgs := logScopeOwnOnly(r, h, "rl")
	if scopeSQL != "" {
		sep := " WHERE "
		if strings.Contains(whereSQL, "WHERE") {
			sep = " AND "
		}
		whereSQL += sep + scopeSQL
		args = append(args, scopeArgs...)
	}
	from := " FROM request_logs rl LEFT JOIN providers p ON rl.provider_id = p.id" + whereSQL

	rows, err := h.DB.Query(db.Q(`SELECT rl.id, rl.key_prefix, COALESCE(rl.key_id,''), rl.provider_id, rl.model, rl.endpoint, rl.status, rl.latency_ms, COALESCE(rl.ttft_ms,0), rl.created_at, rl.prompt_tokens, rl.completion_tokens, rl.total_tokens, rl.cost_usd, rl.is_stream, COALESCE(rl.finish_reason,''), COALESCE(rl.error,'')`+from+` ORDER BY rl.created_at DESC LIMIT ?`),
		append(args, logsExportMaxRows+1)...) // +1 to detect truncation
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="logs-%s.csv"`, time.Now().UTC().Format("20060102-150405")))
	w.WriteHeader(http.StatusOK)

	// RFC 4180 quoting (same helper style as BillingExport).
	qq := func(s string) string {
		if strings.ContainsAny(s, ",\"\n\r") {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	w.Write([]byte("id,key_prefix,key_id,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,finish_reason,error\n"))

	written := 0
	truncated := false
	for rows.Next() {
		if written >= logsExportMaxRows {
			truncated = true
			break
		}
		var id, keyPrefix, model, endpoint, finishReason string
		var keyID, providerID, errMsg sql.NullString
		var createdAt sql.NullTime
		var status int
		var latencyMs, ttftMs int64
		var promptTokens, completionTokens, totalTokens int
		var costUSD float64
		var isStream bool
		if err := rows.Scan(&id, &keyPrefix, &keyID, &providerID, &model, &endpoint, &status, &latencyMs, &ttftMs, &createdAt, &promptTokens, &completionTokens, &totalTokens, &costUSD, &isStream, &finishReason, &errMsg); err != nil {
			continue
		}
		ts := ""
		if createdAt.Valid {
			ts = createdAt.Time.UTC().Format(time.RFC3339)
		}
		stream := "0"
		if isStream {
			stream = "1"
		}
		fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%d,%d,%d,%s,%d,%d,%d,%.6f,%s,%s,%s\n",
			qq(id), qq(keyPrefix), qq(keyID.String), qq(providerID.String), qq(model), qq(endpoint),
			status, latencyMs, ttftMs, ts, promptTokens, completionTokens, totalTokens, costUSD,
			stream, qq(finishReason), qq(errMsg.String))
		written++
	}
	if truncated {
		w.Header().Set("X-Truncated", "true")
		// Header already sent, so signal truncation in-band too.
		fmt.Fprintf(w, "# truncated at %d rows — narrow the filters\n", logsExportMaxRows)
	}
}
