package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

type ProfileHandler struct {
	Store *user.Store
	DB    *sql.DB
}

func (h *ProfileHandler) Routes(r chi.Router) {
	r.Get("/profile", h.GetProfile)
	r.Put("/profile", h.UpdateProfile)
	r.Post("/profile/password", h.ChangePassword)
	r.Get("/profile/activity", h.GetActivity)
	r.Get("/profile/logins", h.GetLogins)
}

func (h *ProfileHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		// Fallback to admin single user case
		if sub == "admin" {
			if u2, _, err2 := h.Store.GetByUsername("admin"); err2 == nil {
				json.NewEncoder(w).Encode(u2)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u)
}

func (h *ProfileHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var body struct {
		DisplayName *string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.DisplayName != nil {
		if err := h.Store.UpdateDisplayName(u.ID, strings.TrimSpace(*body.DisplayName)); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	updated, _ := h.Store.GetByID(u.ID)
	json.NewEncoder(w).Encode(updated)
}

func (h *ProfileHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
		Password    string `json:"password"` // alias
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	oldPw := body.OldPassword
	if oldPw == "" {
		oldPw = body.Password
	}
	newPw := body.NewPassword
	if newPw == "" {
		newPw = body.Password
		if oldPw == newPw {
			http.Error(w, `{"error":"new_password required"}`, http.StatusBadRequest)
			return
		}
	}
	if oldPw == "" || newPw == "" {
		http.Error(w, `{"error":"old and new password required"}`, http.StatusBadRequest)
		return
	}
	if len(newPw) < user.MinPasswordLen {
		http.Error(w, fmt.Sprintf(`{"error":"new password too short (minimum %d characters)"}`, user.MinPasswordLen), http.StatusBadRequest)
		return
	}
	if err := h.Store.ChangePasswordWithOld(u.ID, oldPw, newPw); err != nil {
		// Store errors here are policy messages ("old password incorrect",
		// minimum length) and are safe to surface.
		httperr.Invalid(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *ProfileHandler) GetActivity(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Fetch last 20 audit logs for this actor
	rows, err := h.DB.Query(db.Q(`SELECT id, actor, action, target_type, target_id, meta, created_at FROM audit_logs WHERE actor=? ORDER BY created_at DESC LIMIT 20`), sub)
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type entry struct {
		ID         string `json:"id"`
		Actor      string `json:"actor"`
		Action     string `json:"action"`
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Meta       string `json:"meta"`
		CreatedAt  string `json:"created_at"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		var meta sql.NullString
		// DATETIME columns must scan into time values: go-sqlite3 converts
		// timestamp-declared columns to time.Time, and scanning into a string
		// errors on every row (silently emptying the feed).
		var created sql.NullTime
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &meta, &created); err != nil {
			continue
		}
		if meta.Valid {
			e.Meta = meta.String
		}
		if created.Valid {
			e.CreatedAt = created.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, e)
	}
	if out == nil {
		out = []entry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *ProfileHandler) GetLogins(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Return user login info + recent audit logins
	type loginInfo struct {
		LastLoginAt *string `json:"last_login_at"`
		LoginCount  int     `json:"login_count"`
		CreatedAt   string  `json:"created_at"`
		Recent      []any   `json:"recent"`
	}
	var lastLogin *string
	if u.LastLoginAt != nil {
		s := u.LastLoginAt.Format("2006-01-02T15:04:05Z07:00")
		lastLogin = &s
	}
	// Fetch audit logs where action=login or contains login
	rows, _ := h.DB.Query(db.Q(`SELECT action, created_at FROM audit_logs WHERE actor=? ORDER BY created_at DESC LIMIT 10`), sub)
	var recent []map[string]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var act string
			var created sql.NullTime
			if err := rows.Scan(&act, &created); err == nil && created.Valid {
				recent = append(recent, map[string]string{"action": act, "at": created.Time.UTC().Format(time.RFC3339)})
			}
		}
	}
	if recent == nil {
		recent = []map[string]string{}
	}
	var recentAny []any
	for _, r := range recent {
		recentAny = append(recentAny, r)
	}
	// If no audit logs, fallback to last_login_at
	if len(recentAny) == 0 && lastLogin != nil {
		recentAny = append(recentAny, map[string]string{"action": "login", "at": *lastLogin})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(loginInfo{
		LastLoginAt: lastLogin,
		LoginCount:  u.LoginCount,
		CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Recent:      recentAny,
	})
}
