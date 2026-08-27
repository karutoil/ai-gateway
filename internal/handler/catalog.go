package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"

	"github.com/go-chi/chi/v5"
)

type CatalogHandler struct {
	Store *catalog.Store
	DB    *sql.DB
}

func (h *CatalogHandler) Routes(r chi.Router) {
	r.Get("/catalog", h.List)
	r.Get("/catalog/by-id", h.GetByQuery)
	r.Get("/catalog/{id}", h.Get)
	r.Get("/catalog/*", h.GetWildcard)
	// Mutating catalog routes are admin-only: without this gate even
	// "readonly" could trigger upstream syncs, rewrite aliases and edit
	// system settings.
	r.With(middleware.RequireRole("admin")).Post("/sync", h.Sync)
	r.Get("/status", h.Status)
	// aliases
	r.Get("/aliases", h.ListAliases)
	r.With(middleware.RequireRole("admin")).Post("/aliases", h.CreateAlias)
	r.With(middleware.RequireRole("admin")).Delete("/aliases/{alias}", h.DeleteAlias)
	// settings
	r.Get("/settings", h.GetSettings)
	r.With(middleware.RequireRole("admin")).Put("/settings", h.PutSettings)
	r.With(middleware.RequireRole("admin")).Delete("/settings/{key}", h.DeleteSetting)
}

func (h *CatalogHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := q.Get("q")
	provider := q.Get("provider")
	reasoning := q.Get("reasoning") == "true"
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	list, err := h.Store.List(filter, provider, reasoning, limit, offset)
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	count, _ := h.Store.Count()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"data": list, "total": count})
}

func (h *CatalogHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := h.Store.Get(id)
	if err != nil {
		m, err = h.Store.GetByShortID(id)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func (h *CatalogHandler) GetByQuery(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, `{"error":"id required"}`, 400)
		return
	}
	m, err := h.Store.Get(id)
	if err != nil {
		m, err = h.Store.GetByShortID(id)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func (h *CatalogHandler) GetWildcard(w http.ResponseWriter, r *http.Request) {
	// chi wildcard is "/*" -> param "*"
	id := chi.URLParam(r, "*")
	if id == "" {
		// fallback to URL path after /catalog/
		id = r.URL.Path
		// strip prefix
		if idx := len("/api/models/catalog/"); idx < len(id) {
			id = id[idx:]
		}
	}
	m, err := h.Store.Get(id)
	if err != nil {
		m, err = h.Store.GetByShortID(id)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func (h *CatalogHandler) Sync(w http.ResponseWriter, r *http.Request) {
	// allow optional body with raw json to sync offline
	n, err := h.Store.FetchAndSync()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"synced": n})
}

func (h *CatalogHandler) Status(w http.ResponseWriter, r *http.Request) {
	var last string
	h.DB.QueryRow(db.Q(`SELECT value FROM system_config WHERE key='models_last_sync'`)).Scan(&last)
	count, _ := h.Store.Count()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"count": count, "last_sync": last})
}

func (h *CatalogHandler) ListAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(`SELECT alias, target, created_at FROM model_aliases ORDER BY alias`)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, 500)
		return
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var alias, target string
		var created time.Time
		rows.Scan(&alias, &target, &created)
		out = append(out, map[string]interface{}{"alias": alias, "target": target, "created_at": created})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *CatalogHandler) CreateAlias(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Alias  string `json:"alias"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Alias == "" || body.Target == "" {
		http.Error(w, `{"error":"alias and target required"}`, 400)
		return
	}
	// validate alias: alphanumeric + - _ .
	if len(body.Alias) > 64 || len(body.Target) > 256 {
		http.Error(w, `{"error":"alias or target too long"}`, 400)
		return
	}
	for _, c := range body.Alias {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/') {
			http.Error(w, `{"error":"alias contains invalid character"}`, 400)
			return
		}
	}
	_, err := h.DB.Exec(db.Q(`INSERT INTO model_aliases(alias,target,created_at) VALUES(?,?,?)`)+db.UpsertEnd([]string{"alias"}, []string{"target", "created_at"}), body.Alias, body.Target, time.Now().UTC())
	if err != nil {
		http.Error(w, `{"error":"save failed"}`, 500)
		return
	}
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(body)
}

func (h *CatalogHandler) DeleteAlias(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	h.DB.Exec(db.Q(`DELETE FROM model_aliases WHERE alias=?`), alias)
	w.WriteHeader(204)
}

// internalSettingKey reports keys managed by the system itself that must not
// be surfaced as editable rows (or overwritten via the settings API).
func internalSettingKey(k string) bool {
	return strings.HasPrefix(k, "models_") || strings.HasPrefix(k, "_")
}

func (h *CatalogHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	rows, _ := h.DB.Query(`SELECT key, value FROM system_config`)
	out := map[string]string{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			rows.Scan(&k, &v)
			if internalSettingKey(k) {
				continue
			}
			out[k] = v
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *CatalogHandler) PutSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	if len(body) > 50 {
		http.Error(w, `{"error":"too many keys"}`, 400)
		return
	}
	skipped := []string{}
	for k, v := range body {
		if internalSettingKey(k) {
			skipped = append(skipped, k)
			continue
		}
		if len(k) > 128 || len(v) > 4096 {
			skipped = append(skipped, k)
			continue
		}
		h.DB.Exec(db.Q(`INSERT INTO system_config(key,value,updated_at) VALUES(?,?,?)`)+db.UpsertEnd([]string{"key"}, []string{"value", "updated_at"}), k, v, time.Now().UTC())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"saved": len(body) - len(skipped), "skipped": skipped})
}

// DeleteSetting removes a key from system_config. Previously PUT only
// upserted, so "Remove → Save" in the dashboard silently resurrected the key
// on reload; removals now have a real endpoint the UI calls per key.
func (h *CatalogHandler) DeleteSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" || internalSettingKey(key) {
		http.Error(w, `{"error":"invalid key"}`, 400)
		return
	}
	if len(key) > 128 {
		http.Error(w, `{"error":"invalid key"}`, 400)
		return
	}
	if _, err := h.DB.Exec(db.Q(`DELETE FROM system_config WHERE key=?`), key); err != nil {
		http.Error(w, `{"error":"delete failed"}`, 500)
		return
	}
	w.WriteHeader(204)
}
