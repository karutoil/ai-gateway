package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"ai-gateway/internal/httperr"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/rbac"

	"github.com/go-chi/chi/v5"
)

// OAuthHandler drives the generic Authorization Code + PKCE flow for any
// registered internal/oauth definition. Providers stay thin: adding a future
// OAuth upstream is one Register() call, no handler changes.
type OAuthHandler struct {
	Providers *provider.Store
	PublicURL string
}

func (h *OAuthHandler) Routes(r chi.Router) {
	r.With(middleware.RequirePerm(rbac.PermProvidersWrite)).Post("/oauth/start", h.Start)
	r.With(middleware.RequirePerm(rbac.PermProvidersWrite)).Post("/oauth/exchange", h.Exchange)
	r.With(middleware.RequirePerm(rbac.PermProvidersWrite)).Post("/providers/{id}/oauth/refresh", h.Refresh)
	r.With(middleware.RequirePerm(rbac.PermProvidersDelete)).Delete("/providers/{id}/oauth", h.Disconnect)
	// Readable with providers:read so the UI can show connect badges.
	r.With(middleware.RequireAnyPerm(rbac.PermProvidersWrite, rbac.PermProvidersTest)).Get("/oauth/providers", h.ListDefs)
	r.With(middleware.RequireAnyPerm(rbac.PermProvidersWrite, rbac.PermProvidersTest)).Get("/providers/{id}/oauth/status", h.Status)
}

// PublicRoutes holds the server-side redirect for custom OAuth apps.
// No dashboard auth: the single-use state is the capability.
func (h *OAuthHandler) PublicRoutes(r chi.Router) {
	r.Get("/oauth/callback", h.Callback)
}

func (h *OAuthHandler) ListDefs(w http.ResponseWriter, r *http.Request) {
	defs := oauth.List()
	if defs == nil {
		defs = []oauth.Definition{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(defs)
}

type oauthStartReq struct {
	DefID        string `json:"def_id"`
	ProviderName string `json:"provider_name"`
	ProviderID   string `json:"provider_id"`
}

func defToProviderType(defID string) models.ProviderType {
	switch defID {
	case "antigravity":
		return models.ProviderAntigravity
	case "devin":
		return models.ProviderDevin
	default:
		return models.ProviderOpenAICompatible
	}
}

func (h *OAuthHandler) gatewayCallbackURL(r *http.Request) string {
	if h.PublicURL != "" {
		return strings.TrimRight(h.PublicURL, "/") + "/api/oauth/callback"
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	return scheme + "://" + host + "/api/oauth/callback"
}

// Start creates (or reuses) a provider row and returns the browser auth URL.
// Default is paste mode (loopback redirect + copy/paste) so bundled public
// clients work with zero OAuth-app setup. Operators with their own OAuth app
// get server mode via the X-OAuth-Mode: redirect header when they registered
// the gateway callback URL with the provider.
func (h *OAuthHandler) Start(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body oauthStartReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.DefID == "" {
		body.DefID = "antigravity"
	}
	def, _, ok := oauth.Get(body.DefID)
	if !ok {
		httperr.Invalid(w, "unknown oauth provider "+body.DefID)
		return
	}
	var p *models.Provider
	if body.ProviderID != "" {
		var err error
		p, err = h.Providers.GetByID(body.ProviderID)
		if err != nil {
			httperr.NotFound(w, "provider not found")
			return
		}
	} else {
		name := strings.TrimSpace(body.ProviderName)
		if name == "" {
			name = body.DefID
		}
		if existing, err := h.Providers.GetByName(name); err == nil && existing != nil {
			p = existing
		} else {
			created, err := h.Providers.CreateWithOrg(name, defToProviderType(body.DefID), "", "", "")
			if err != nil {
				httperr.Invalid(w, err.Error())
				return
			}
			p, _ = h.Providers.GetByID(created.ID)
		}
	}
	if p == nil {
		httperr.Write(w, http.StatusInternalServerError, "provider resolve failed", httperr.TypeProxy)
		return
	}
	// Paste mode uses the loopback redirect the public client allows.
	// Server mode (custom apps) uses the gateway callback; opt in with
	// X-OAuth-Mode: redirect when the operator registered that URL in Google Cloud.
	redirectURI := def.PasteRedirectURI
	if redirectURI == "" {
		redirectURI = oauth.DefaultPasteRedirectURI
	}
	mode := "paste"
	if strings.EqualFold(r.Header.Get("X-OAuth-Mode"), "redirect") {
		redirectURI = h.gatewayCallbackURL(r)
		mode = "redirect"
	}
	pending := oauth.NewPending(def, p.Name, p.ID, redirectURI)
	url := oauth.AuthURL(def, pending)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"auth_url": url, "state": pending.State,
		"provider_id": p.ID, "provider_name": p.Name,
		"mode": mode, "redirect_uri": redirectURI,
	})
}

type oauthExchangeReq struct {
	State       string `json:"state"`
	ProviderID  string `json:"provider_id"`
	CallbackURL string `json:"callback_url"`
	Code        string `json:"code"`
}

// Exchange completes paste mode: the user pastes the localhost callback URL.
func (h *OAuthHandler) Exchange(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body oauthExchangeReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Invalid(w, "invalid json")
		return
	}
	pending, ok := oauth.PeekPending(body.State)
	if !ok {
		httperr.Invalid(w, "login session expired or unknown — start again")
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		var err error
		code, err = oauth.ParseCallbackURL(body.CallbackURL, body.State)
		if err != nil {
			httperr.Invalid(w, err.Error())
			return
		}
	}
	def, _, ok := oauth.Get(pending.DefID)
	if !ok {
		httperr.Invalid(w, "unknown oauth provider")
		return
	}
	tok, err := oauth.ExchangeCode(r.Context(), def, pending, code, nil)
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "oauth exchange failed: "+err.Error(), httperr.TypeProxy)
		return
	}
	// Single-use state: consume only on success so the user can retry a bad paste.
	oauth.ConsumePending(body.State)
	providerID := body.ProviderID
	if providerID == "" {
		providerID = pending.ProviderID
	}
	if err := h.Providers.SetOAuthTokens(providerID, pending.DefID, tok); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to store oauth tokens", httperr.TypeProxy)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "provider_id": providerID,
		"email": tok.Email, "project_id": tok.ProjectID,
	})
}

// Callback completes server mode (custom OAuth apps): Google redirects here.
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		httperr.Invalid(w, "provider returned error: "+e)
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		httperr.Invalid(w, "missing code or state")
		return
	}
	pending, ok := oauth.ConsumePending(state)
	if !ok {
		httperr.Invalid(w, "login session expired — start again")
		return
	}
	def, _, ok := oauth.Get(pending.DefID)
	if !ok {
		httperr.Invalid(w, "unknown oauth provider")
		return
	}
	tok, err := oauth.ExchangeCode(r.Context(), def, pending, code, nil)
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "oauth exchange failed: "+err.Error(), httperr.TypeProxy)
		return
	}
	if err := h.Providers.SetOAuthTokens(pending.ProviderID, pending.DefID, tok); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to store oauth tokens", httperr.TypeProxy)
		return
	}
	// Back to the dashboard with a success flag (no secrets in query).
	http.Redirect(w, r, "/providers?oauth=connected&provider="+pending.ProviderID, http.StatusFound)
}

func (h *OAuthHandler) Status(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := h.Providers.GetByID(id)
	if err != nil {
		httperr.NotFound(w, "provider not found")
		return
	}
	h.Providers.EnrichOAuthOne(p)
	tok, defID, terr := h.Providers.OAuthTokens(p)
	connected := terr == nil && tok != nil && tok.Refresh != ""
	out := map[string]any{
		"provider_id": id, "connected": connected,
		"def_id": p.OAuthDefID, "email": p.OAuthEmail, "project_id": p.OAuthProject,
		"expires_at": p.OAuthExpires,
	}
	if defID != "" && p.OAuthDefID == "" {
		out["def_id"] = defID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *OAuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := h.Providers.GetByID(id)
	if err != nil {
		httperr.NotFound(w, "provider not found")
		return
	}
	access, project, email, err := h.Providers.EnsureFreshAccess(r.Context(), p, nil)
	if err != nil {
		httperr.Write(w, http.StatusBadGateway, "oauth refresh failed: "+err.Error(), httperr.TypeProxy)
		return
	}
	_ = access
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "email": email, "project_id": project})
}

func (h *OAuthHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.Providers.GetByID(id); err != nil {
		httperr.NotFound(w, "provider not found")
		return
	}
	if err := h.Providers.ClearOAuth(id); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "disconnect failed", httperr.TypeProxy)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
