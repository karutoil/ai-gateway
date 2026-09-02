package passkey

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/user"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/rs/zerolog/log"
)

type Handler struct {
	UserStore *user.Store
	PassStore *Store
	WebAuthn  *webauthn.WebAuthn
	Config    *config.Config
	DB        *sql.DB
	sessions  sync.Map
}

type sessionEntry struct {
	Data      webauthn.SessionData
	UserID    string
	ExpiresAt time.Time
}

func NewHandler(db *sql.DB, userStore *user.Store, cfg *config.Config) (*Handler, error) {
	wa, err := NewWebAuthn()
	if err != nil {
		return nil, err
	}
	h := &Handler{UserStore: userStore, PassStore: NewStore(db), WebAuthn: wa, Config: cfg, DB: db}
	go h.cleanupSessions()
	return h, nil
}

func (h *Handler) cleanupSessions() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		h.sessions.Range(func(k, v any) bool {
			if e, ok := v.(sessionEntry); ok && now.After(e.ExpiresAt) {
				h.sessions.Delete(k)
			}
			return true
		})
	}
}

// orgForID resolves the org claim minted into a dashboard JWT for a user:
// their first org membership ("" = global). All passkey/recovery login paths
// share this helper so tokens can never carry an org the user is not in.
func (h *Handler) orgForID(userID string) string {
	if h.UserStore == nil || userID == "" {
		return ""
	}
	return h.UserStore.PrimaryOrgID(userID)
}

// ownUserID maps the JWT subject to the local dashboard user id ("" if
// unknown); used for self-scoping checks.
func (h *Handler) ownUserID(subject string) string {
	if h.UserStore == nil || subject == "" {
		return ""
	}
	if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
		return u.ID
	}
	return ""
}

func (h *Handler) loadWebAuthnUser(userID string) (WebAuthnUser, error) {
	u, err := h.UserStore.GetByID(userID)
	if err != nil {
		return WebAuthnUser{}, err
	}
	creds, _ := h.PassStore.ListCredentials(userID)
	return WebAuthnUser{DashboardUser: u, Creds: creds}, nil
}

func (h *Handler) BeginRegistration(w http.ResponseWriter, r *http.Request) {
	requesterRole := auth.GetRole(r)
	requesterSub := auth.GetSubject(r)
	if requesterSub == "" || requesterRole == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	self, _, err := h.UserStore.GetByUsername(requesterSub)
	if err != nil {
		http.Error(w, `{"error":"requester user not found"}`, http.StatusForbidden)
		return
	}
	// SECURITY: a registration ceremony may only ever target the caller's own
	// account unless an explicit admin requests another user by id. There is
	// deliberately NO default-to-admin fallback.
	targetUserID := self.ID
	if q := r.URL.Query().Get("user_id"); q != "" && q != self.ID {
		if requesterRole != "admin" {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		targetUserID = q
	}
	waUser, err := h.loadWebAuthnUser(targetUserID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	options, session, err := h.WebAuthn.BeginRegistration(waUser)
	if err != nil {
		log.Error().Err(err).Msg("webauthn begin registration failed")
		http.Error(w, `{"error":"failed to begin registration"}`, http.StatusInternalServerError)
		return
	}
	sessionID := waUser.ID + ":" + time.Now().Format(time.RFC3339Nano)
	h.sessions.Store(sessionID, sessionEntry{Data: *session, UserID: targetUserID, ExpiresAt: time.Now().Add(5 * time.Minute)})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"options": options,
		"session": sessionID,
	})
}

func (h *Handler) FinishRegistration(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-WebAuthn-Session")
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session")
	}
	if sessionID == "" {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if v, ok := body["session"]; ok {
				json.Unmarshal(v, &sessionID)
				// Put back credential body for webauthn to parse
				if cred, ok := body["credential"]; ok {
					r.Body = &nopCloser{data: cred}
				} else if cred, ok := body["response"]; ok {
					r.Body = &nopCloser{data: cred}
				} else {
					// try to use whole body without session
					delete(body, "session")
					rest, _ := json.Marshal(body)
					if len(rest) > 2 {
						r.Body = &nopCloser{data: rest}
					}
				}
			}
		}
	}
	if sessionID == "" {
		http.Error(w, `{"error":"session required"}`, http.StatusBadRequest)
		return
	}
	val, ok := h.sessions.Load(sessionID)
	if !ok {
		http.Error(w, `{"error":"session not found or expired"}`, http.StatusBadRequest)
		return
	}
	entry := val.(sessionEntry)
	h.sessions.Delete(sessionID)
	waUser, err := h.loadWebAuthnUser(entry.UserID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	// If we already consumed body for session, r.Body is credential; else r.Body is original
	credential, err := h.WebAuthn.FinishRegistration(waUser, entry.Data, r)
	if err != nil {
		log.Error().Err(err).Msg("finish registration failed")
		http.Error(w, `{"error":"failed to create credential: "}`+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.PassStore.SaveCredential(entry.UserID, *credential); err != nil {
		log.Error().Err(err).Msg("save credential failed")
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}
	_ = h.UserStore.SetPasskeyEnabled(entry.UserID, true)
	code := user.GenerateRecoveryCode()
	_ = h.UserStore.SetRecoveryCode(entry.UserID, code)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":        "ok",
		"recovery_code": code,
		"message":       "passkey enabled — save recovery code, required if passkey is lost",
	})
}

type nopCloser struct {
	data []byte
	off  int
}

func (n *nopCloser) Read(p []byte) (int, error) {
	if n.off >= len(n.data) {
		return 0, io.EOF
	}
	m := copy(p, n.data[n.off:])
	n.off += m
	if n.off >= len(n.data) {
		return m, io.EOF
	}
	return m, nil
}
func (n *nopCloser) Close() error { return nil }

func (h *Handler) BeginLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	username := body.Username
	if username == "" {
		username = r.URL.Query().Get("username")
	}
	if username != "" {
		u, _, err := h.UserStore.GetByUsername(username)
		if err != nil {
			// Anti-enumeration: unknown usernames get the SAME discoverable-
			// login options shape as a real begin (never a 404 oracle). The
			// stored session belongs to no user, so FinishLogin always fails.
			options, session, derr := h.WebAuthn.BeginDiscoverableLogin()
			if derr != nil {
				http.Error(w, `{"error":"failed to begin login"}`, http.StatusInternalServerError)
				return
			}
			sessionID := "decoy:" + time.Now().Format(time.RFC3339Nano)
			h.sessions.Store(sessionID, sessionEntry{Data: *session, UserID: "", ExpiresAt: time.Now().Add(5 * time.Minute)})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"options": options, "session": sessionID})
			return
		}
		waUser, _ := h.loadWebAuthnUser(u.ID)
		options, session, err := h.WebAuthn.BeginLogin(waUser)
		if err != nil {
			http.Error(w, `{"error":"failed to begin login"}`, http.StatusInternalServerError)
			return
		}
		sessionID := u.ID + ":" + time.Now().Format(time.RFC3339Nano)
		h.sessions.Store(sessionID, sessionEntry{Data: *session, UserID: u.ID, ExpiresAt: time.Now().Add(5 * time.Minute)})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"options": options, "session": sessionID})
		return
	}
	options, session, err := h.WebAuthn.BeginDiscoverableLogin()
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	sessionID := "discoverable:" + time.Now().Format(time.RFC3339Nano)
	h.sessions.Store(sessionID, sessionEntry{Data: *session, UserID: "", ExpiresAt: time.Now().Add(5 * time.Minute)})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"options": options, "session": sessionID})
}

func (h *Handler) FinishLogin(w http.ResponseWriter, r *http.Request) {
	// Try to get session from header/query or body
	sessionID := r.Header.Get("X-WebAuthn-Session")
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session")
	}
	var bodyRaw map[string]json.RawMessage
	var bodyBytes []byte
	if r.Body != nil {
		// Peek body
		bodyBytes = make([]byte, 0)
		// We need to read body for session extraction
		// If session in body, we need to extract
		// Use a temporary read
	}
	// For simplicity, if session not in header, try to decode body map
	if sessionID == "" {
		// Need to read body
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&bodyRaw); err == nil {
			if v, ok := bodyRaw["session"]; ok {
				json.Unmarshal(v, &sessionID)
				if cred, ok := bodyRaw["credential"]; ok {
					bodyBytes = cred
				} else if len(bodyRaw) == 1 {
					// only session? shouldn't happen
				} else {
					// remove session and re-marshal remaining as credential
					delete(bodyRaw, "session")
					bodyBytes, _ = json.Marshal(bodyRaw)
					if len(bodyRaw) == 1 {
						for _, v := range bodyRaw {
							bodyBytes = v
							break
						}
					}
				}
			}
		}
		if len(bodyBytes) > 0 {
			r.Body = &nopCloser{data: bodyBytes}
		} else if sessionID == "" {
			// fallback: body was empty, error
			http.Error(w, `{"error":"session required"}`, http.StatusBadRequest)
			return
		}
	} else {
		// session from header, body is already credential
		// need to ensure r.Body is readable - it already is
		// But if we haven't read it yet, it's fine
		// If we read bodyRaw earlier, we already handled
	}
	if sessionID == "" {
		http.Error(w, `{"error":"session required"}`, http.StatusBadRequest)
		return
	}
	val, ok := h.sessions.Load(sessionID)
	if !ok {
		http.Error(w, `{"error":"session expired"}`, http.StatusBadRequest)
		return
	}
	entry := val.(sessionEntry)
	h.sessions.Delete(sessionID)

	if strings.HasPrefix(sessionID, "discoverable:") {
		// Discoverable login
		credential, err := h.WebAuthn.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			uid := string(userHandle)
			if uid == "" {
				// try to find by credential
				return nil, fmt.Errorf("userHandle missing")
			}
			u, err := h.UserStore.GetByID(uid)
			if err != nil {
				return nil, err
			}
			creds, _ := h.PassStore.ListCredentials(uid)
			return WebAuthnUser{DashboardUser: u, Creds: creds}, nil
		}, entry.Data, r)
		if err != nil {
			log.Error().Err(err).Str("session_id", sessionID).Msg("webauthn finish discoverable login failed")
			http.Error(w, `{"error":"auth failed: "}`+err.Error(), http.StatusUnauthorized)
			return
		}
		// Need to find user from credential - use credential.ID to lookup?
		// The library returns credential, but we need user. For discoverable, the user is determined via userHandle
		// We already have it via the callback, but we need to get username for token
		// Use the returned credential's user? Instead, parse from request's userHandle again
		// Fallback: try to get username from JWT? Not available
		// We can look up credential owner by credential ID
		var username string
		var role string
		// Try to find owner of credential
		b64 := base64.RawURLEncoding.EncodeToString(credential.ID)
		var ownerID string
		h.DB.QueryRow("SELECT user_id FROM webauthn_credentials WHERE credential_id=?", b64).Scan(&ownerID)
		if ownerID != "" {
			if u, err := h.UserStore.GetByID(ownerID); err == nil {
				username = u.Username
				role = string(u.Role)
			}
		}
		if username == "" {
			// fallback to entry.UserID if set
			if entry.UserID != "" {
				if u, err := h.UserStore.GetByID(entry.UserID); err == nil {
					username = u.Username
					role = string(u.Role)
				}
			}
		}
		if username == "" {
			// SECURITY: fail closed. A discoverable ceremony whose owner cannot be
			// resolved must never mint a token, let alone an admin one.
			http.Error(w, `{"error":"credential owner not found"}`, http.StatusUnauthorized)
			return
		}
		u, _, err := h.UserStore.GetByUsername(username)
		if err != nil || u.Disabled {
			http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		role = string(u.Role)
		_ = h.PassStore.UpdateCounter(credential.ID, credential.Authenticator.SignCount)
		// Update last login for discovered user
		if ownerID != "" {
			_ = h.UserStore.UpdateLastLogin(ownerID)
		}
		tv, _ := h.UserStore.TokenVersionFor(username)
		token, err := auth.MakeTokenFull(h.Config.JWTSecret, username, h.orgForID(u.ID), role, tv)
		if err != nil {
			http.Error(w, `{"error":"failed to create token"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token, "username": username, "role": role})
		return
	}

	waUser, err := h.loadWebAuthnUser(entry.UserID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	if waUser.DashboardUser != nil && waUser.DashboardUser.Disabled {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	// If we consumed body earlier for session, r.Body is already credential
	// Otherwise r.Body is original credential
	cred, err := h.WebAuthn.FinishLogin(waUser, entry.Data, r)
	if err != nil {
		log.Error().Err(err).Str("user_id", waUser.ID).Msg("webauthn finish login failed")
		http.Error(w, `{"error":"auth failed"}`, http.StatusUnauthorized)
		return
	}
	_ = h.PassStore.UpdateCounter(cred.ID, cred.Authenticator.SignCount)
	_ = h.UserStore.UpdateLastLogin(waUser.ID)
	tv, _ := h.UserStore.TokenVersionFor(waUser.Username)
	token, err := auth.MakeTokenFull(h.Config.JWTSecret, waUser.Username, h.orgForID(waUser.ID), string(waUser.Role), tv)
	if err != nil {
		http.Error(w, `{"error":"failed to create token"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token, "username": waUser.Username, "role": string(waUser.Role)})
}

func (h *Handler) VerifyRecovery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username     string `json:"username"`
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	code := body.Code
	if code == "" {
		code = body.RecoveryCode
	}
	if body.Username == "" || code == "" {
		http.Error(w, `{"error":"username and recovery_code required"}`, http.StatusBadRequest)
		return
	}
	u, _, err := h.UserStore.GetByUsername(body.Username)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	ok, _ := h.UserStore.VerifyRecoveryCode(u.ID, code)
	if !ok {
		http.Error(w, `{"error":"invalid recovery code"}`, http.StatusUnauthorized)
		return
	}
	if u.Disabled {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	// Single-use semantics: burn the code and force regeneration.
	_ = h.UserStore.ConsumeRecoveryCode(u.ID)
	_ = h.UserStore.UpdateLastLogin(u.ID)
	tv, _ := h.UserStore.TokenVersionFor(u.Username)
	token, err := auth.MakeTokenFull(h.Config.JWTSecret, u.Username, h.orgForID(u.ID), string(u.Role), tv)
	if err != nil {
		http.Error(w, `{"error":"failed to create token"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"token": token, "username": u.Username, "role": string(u.Role), "recovery": true})
}

func (h *Handler) GenerateRecovery(w http.ResponseWriter, r *http.Request) {
	subject := auth.GetSubject(r)
	role := auth.GetRole(r)
	targetID := r.URL.Query().Get("user_id")
	if targetID == "" {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
			targetID = u.ID
		}
	}
	if targetID == "" {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	if targetID != "" && role != "admin" {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil && u.ID != targetID {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
	}
	code := user.GenerateRecoveryCode()
	if err := h.UserStore.SetRecoveryCode(targetID, code); err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"recovery_code": code})
}

func (h *Handler) DisablePasskey(w http.ResponseWriter, r *http.Request) {
	subject := auth.GetSubject(r)
	role := auth.GetRole(r)
	targetID := r.URL.Query().Get("user_id")
	if targetID == "" {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
			targetID = u.ID
		}
	}
	if targetID == "" {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	if targetID != "" && role != "admin" {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil && u.ID != targetID {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
	}
	_ = h.PassStore.DeleteAllForUser(targetID)
	_ = h.UserStore.DisablePasskey(targetID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "disabled"})
}

func (h *Handler) ListCredentials(w http.ResponseWriter, r *http.Request) {
	subject := auth.GetSubject(r)
	targetID := r.URL.Query().Get("user_id")
	if targetID == "" {
		if u, _, err := h.UserStore.GetByUsername(subject); err == nil {
			targetID = u.ID
		}
	}
	// SECURITY: self-or-admin only. Without this check any authenticated user
	// could enumerate any other user's WebAuthn credential IDs.
	if ownID := h.ownUserID(subject); targetID != ownID {
		if auth.GetRole(r) != "admin" {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
	}
	creds, _ := h.PassStore.ListCredentials(targetID)
	var out []map[string]any
	for _, c := range creds {
		out = append(out, map[string]any{
			"id":         base64.RawURLEncoding.EncodeToString(c.ID),
			"transports": c.Transport,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	json.NewEncoder(w).Encode(out)
}
