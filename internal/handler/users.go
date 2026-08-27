package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

type UsersHandler struct {
	Store  *user.Store
	Config *config.Config
	DB     *sql.DB
}

func (h *UsersHandler) Routes(r chi.Router) {
	r.Get("/admin/users", h.ListUsers)
	r.Post("/admin/users", h.CreateUser)
	r.Get("/admin/users/me", h.GetMe)
	r.Put("/admin/users/{id}", h.UpdateUser)
	r.Delete("/admin/users/{id}", h.DeleteUser)
	r.Post("/admin/users/{id}/reset-password", h.ResetPassword)
}

func (h *UsersHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	// admin only
	if auth.GetRole(r) != "admin" {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	list, err := h.Store.List()
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []user.DashboardUser{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (h *UsersHandler) GetMe(w http.ResponseWriter, r *http.Request) {
	sub := auth.GetSubject(r)
	if sub == "" || sub == "admin" {
		// try to find admin user
		u, _, err := h.Store.GetByUsername("admin")
		if err == nil {
			json.NewEncoder(w).Encode(u)
			return
		}
		// fallback to token subject
		json.NewEncoder(w).Encode(map[string]string{"username": sub, "role": auth.GetRole(r)})
		return
	}
	u, _, err := h.Store.GetByUsername(sub)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(u)
}

func (h *UsersHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if auth.GetRole(r) != "admin" {
		http.Error(w, `{"error":"forbidden: admin only"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	if body.Username == "" || body.Password == "" {
		http.Error(w, `{"error":"username and password required"}`, http.StatusBadRequest)
		return
	}
	if !user.IsValidRole(body.Role) {
		http.Error(w, `{"error":"valid role required (admin|support|member|readonly)"}`, http.StatusBadRequest)
		return
	}
	u, err := h.Store.Create(body.Username, body.Password, body.Role, body.DisplayName)
	if err != nil {
		// Store create errors are policy messages (validation/uniqueness).
		httperr.Invalid(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(u)
}

func (h *UsersHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	if auth.GetRole(r) != "admin" {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Role        *string `json:"role"`
		DisplayName *string `json:"display_name"`
		Password    *string `json:"password"`
		Disabled    *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Role != nil {
		if !user.IsValidRole(*body.Role) {
			http.Error(w, `{"error":"invalid role"}`, http.StatusBadRequest)
			return
		}
		if err := h.Store.UpdateRole(id, user.Role(*body.Role)); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	if body.Password != nil && *body.Password != "" {
		if err := h.Store.UpdatePassword(id, *body.Password); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	// For display_name and disabled, direct DB update — disabled transitions
	// go through the store so outstanding sessions are revoked.
	if body.DisplayName != nil {
		if err := h.Store.UpdateDisplayName(id, *body.DisplayName); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	if body.Disabled != nil {
		if err := h.Store.SetDisabled(id, *body.Disabled); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
	}
	u, err := h.Store.GetByID(id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(u)
}

func (h *UsersHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if auth.GetRole(r) != "admin" {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	// Prevent deleting self and last admin
	sub := auth.GetSubject(r)
	if u, _, err := h.Store.GetByUsername(sub); err == nil && u.ID == id {
		http.Error(w, `{"error":"cannot delete self"}`, http.StatusBadRequest)
		return
	}
	// Check if it's last admin
	users, _ := h.Store.List()
	adminCount := 0
	for _, u := range users {
		if u.Role == "admin" && !u.Disabled {
			adminCount++
		}
	}
	if target, err := h.Store.GetByID(id); err == nil && target.Role == "admin" && adminCount <= 1 {
		http.Error(w, `{"error":"cannot delete last admin"}`, http.StatusBadRequest)
		return
	}
	if err := h.Store.Delete(id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *UsersHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if auth.GetRole(r) != "admin" {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, `{"error":"password required"}`, http.StatusBadRequest)
		return
	}
	if err := h.Store.UpdatePassword(id, body.Password); err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
