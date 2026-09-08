package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Tokens is the normalized OAuth credential set stored (encrypted) per provider.
// Access is short-lived, Refresh is long-lived. ExpiresAt is unix millis.
// Extra carries provider-specific fields (e.g. Antigravity project_id, email).
type Tokens struct {
	Access    string            `json:"access"`
	Refresh   string            `json:"refresh"`
	ExpiresAt int64             `json:"expires_at_ms"`
	Email     string            `json:"email,omitempty"`
	ProjectID string            `json:"project_id,omitempty"`
	RawExtra  map[string]string `json:"extra,omitempty"`
}

// Expired reports whether the access token needs refresh (with 5m skew).
func (t *Tokens) Expired(now time.Time) bool {
	if t == nil || t.Access == "" {
		return true
	}
	if t.ExpiresAt <= 0 {
		return false
	}
	return now.UnixMilli() >= t.ExpiresAt-5*60*1000
}

// Definition describes one OAuth-capable upstream so the gateway can drive the
// Authorization Code + PKCE flow generically. Adding a future provider is one
// Register() call with a Definition plus optional Hooks — no handler changes.
type Definition struct {
	// ID is the stable key stored on providers.oauth_def_id (e.g. "antigravity").
	ID string `json:"id"`
	// Name is the human label shown in the UI.
	Name string `json:"name"`
	// AuthURL is the provider authorization endpoint.
	AuthURL string `json:"auth_url"`
	// TokenURL is the provider token endpoint.
	TokenURL string `json:"token_url"`
	// Scopes requested at authorize time.
	Scopes []string `json:"scopes"`
	// ClientIDEnv / ClientSecretEnv allow operator override; ClientID/Secret are defaults.
	ClientIDEnv     string `json:"-"`
	ClientSecretEnv string `json:"-"`
	ClientID        string `json:"-"`
	ClientSecret    string `json:"-"`
	// UsePKCE enables S256 code_challenge/verifier (required for public clients).
	UsePKCE bool `json:"use_pkce"`
	// ExtraAuthParams are merged into the authorize URL (e.g. access_type=offline).
	ExtraAuthParams map[string]string `json:"extra_auth_params,omitempty"`
	// UserInfoURL, when set, is fetched with the access token to learn the email.
	UserInfoURL string `json:"-"`
	// PasteRedirectURI is the loopback callback advertised in paste mode
	// (the browser lands there, the user copies the URL). Defaults to
	// DefaultPasteRedirectURI when empty.
	PasteRedirectURI string `json:"-"`
}

// DefaultPasteRedirectURI is the loopback callback used in paste mode unless
// a definition overrides it.
const DefaultPasteRedirectURI = "http://localhost:51121/oauth-callback"

// ClientIDResolved returns env override or default.
func (d *Definition) ClientIDResolved() string {
	if d.ClientIDEnv != "" {
		if v := strings.TrimSpace(envGet(d.ClientIDEnv)); v != "" {
			return v
		}
	}
	return d.ClientID
}

// ClientSecretResolved returns env override or default.
func (d *Definition) ClientSecretResolved() string {
	if d.ClientSecretEnv != "" {
		if v := strings.TrimSpace(envGet(d.ClientSecretEnv)); v != "" {
			return v
		}
	}
	return d.ClientSecret
}

// Hooks customizes the generic flow per provider without forking handlers.
// Standard OAuth2 providers only need AfterExchange/AfterRefresh; providers
// with non-standard authorize or token endpoints (no client_id, JSON token
// exchange, long-lived session tokens) override BuildAuthURL, Exchange, or
// DoRefresh instead.
type Hooks struct {
	// AfterExchange enriches tokens post-code-exchange (fetch email, project id...).
	AfterExchange func(ctx context.Context, def *Definition, tok *Tokens, httpClient *http.Client) error
	// AfterRefresh enriches tokens post-refresh.
	AfterRefresh func(ctx context.Context, def *Definition, tok *Tokens, httpClient *http.Client) error
	// BuildAuthURL overrides the standard authorize-URL builder entirely.
	BuildAuthURL func(def *Definition, p PendingLogin) string
	// Exchange overrides the standard form-encoded code exchange entirely.
	Exchange func(ctx context.Context, def *Definition, p PendingLogin, code string, httpClient *http.Client) (*Tokens, error)
	// DoRefresh overrides the standard refresh_token grant entirely.
	DoRefresh func(ctx context.Context, def *Definition, refreshToken string, httpClient *http.Client) (*Tokens, error)
	// Redact scrubs provider errors for UI display.
	Redact func(string) string
}

type entry struct {
	def   Definition
	hooks Hooks
}

var (
	mu       sync.RWMutex
	registry = map[string]entry{}
)

// Register adds (or replaces) an OAuth provider definition.
// Call from provider package init() — e.g. antigravity.Register().
func Register(def Definition, hooks Hooks) {
	mu.Lock()
	defer mu.Unlock()
	registry[def.ID] = entry{def: def, hooks: hooks}
}

// Get returns a definition by id.
func Get(id string) (Definition, Hooks, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := registry[id]
	if !ok {
		return Definition{}, Hooks{}, false
	}
	return e.def, e.hooks, true
}

// List returns all registered definitions sorted by ID.
func List() []Definition {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Definition, 0, len(registry))
	for _, e := range registry {
		d := e.def
		d.ClientID = ""
		d.ClientSecret = ""
		out = append(out, d)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// PendingLogin tracks an in-flight authorize request between /start and /callback.
type PendingLogin struct {
	State        string
	DefID        string
	ProviderName string
	ProviderID   string // when linking to an existing provider row
	Verifier     string
	RedirectURI  string
	CreatedAt    time.Time
}

var (
	pendingMu sync.Mutex
	pending   = map[string]PendingLogin{}
)

// NewPending creates a pending login with PKCE verifier when required.
func NewPending(def Definition, providerName, providerID, redirectURI string) PendingLogin {
	state := randomString(32)
	verifier := ""
	if def.UsePKCE {
		verifier = randomString(64)
	}
	p := PendingLogin{
		State: state, DefID: def.ID, ProviderName: providerName,
		ProviderID: providerID, Verifier: verifier,
		RedirectURI: redirectURI, CreatedAt: time.Now().UTC(),
	}
	pendingMu.Lock()
	pending[state] = p
	// opportunistic expiry sweep
	for k, v := range pending {
		if time.Since(v.CreatedAt) > 15*time.Minute {
			delete(pending, k)
		}
	}
	pendingMu.Unlock()
	return p
}

// ConsumePending removes and returns a pending login (single-use, like the code).
func ConsumePending(state string) (PendingLogin, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pending[state]
	if !ok {
		return PendingLogin{}, false
	}
	delete(pending, state)
	if time.Since(p.CreatedAt) > 15*time.Minute {
		return PendingLogin{}, false
	}
	return p, true
}

// PeekPending returns a pending login without consuming (for headless paste flow).
func PeekPending(state string) (PendingLogin, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pending[state]
	if !ok || time.Since(p.CreatedAt) > 15*time.Minute {
		return PendingLogin{}, false
	}
	return p, true
}

// AuthURL builds the browser redirect URL for a pending login.
func AuthURL(def Definition, p PendingLogin) string {
	if _, hooks, ok := Get(def.ID); ok && hooks.BuildAuthURL != nil {
		return hooks.BuildAuthURL(&def, p)
	}
	return standardAuthURL(def, p)
}

// standardAuthURL builds an OAuth2 authorize URL with client_id + scopes.
func standardAuthURL(def Definition, p PendingLogin) string {
	q := url.Values{}
	q.Set("client_id", def.ClientIDResolved())
	q.Set("response_type", "code")
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", strings.Join(def.Scopes, " "))
	q.Set("state", p.State)
	if def.UsePKCE && p.Verifier != "" {
		sum := sha256.Sum256([]byte(p.Verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range def.ExtraAuthParams {
		q.Set(k, v)
	}
	return def.AuthURL + "?" + q.Encode()
}

// ExchangeCode performs the code->tokens exchange generically.
func ExchangeCode(ctx context.Context, def Definition, p PendingLogin, code string, httpClient *http.Client) (*Tokens, error) {
	if _, hooks, ok := Get(def.ID); ok && hooks.Exchange != nil {
		tok, err := hooks.Exchange(ctx, &def, p, code, httpClient)
		if err != nil {
			return nil, err
		}
		if _, hooks, ok := Get(def.ID); ok && hooks.AfterExchange != nil {
			if err := hooks.AfterExchange(ctx, &def, tok, httpClient); err != nil {
				return nil, err
			}
		}
		return tok, nil
	}
	form := url.Values{}
	form.Set("client_id", def.ClientIDResolved())
	if s := def.ClientSecretResolved(); s != "" {
		form.Set("client_secret", s)
	}
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", p.RedirectURI)
	if def.UsePKCE && p.Verifier != "" {
		form.Set("code_verifier", p.Verifier)
	}
	tok, err := doTokenRequest(ctx, def.TokenURL, form, httpClient)
	if err != nil {
		return nil, err
	}
	if _, hooks, ok := Get(def.ID); ok && hooks.AfterExchange != nil {
		if err := hooks.AfterExchange(ctx, &def, tok, httpClient); err != nil {
			return nil, err
		}
	}
	return tok, nil
}

// Refresh performs a refresh_token grant generically.
func Refresh(ctx context.Context, def Definition, refreshToken string, httpClient *http.Client) (*Tokens, error) {
	if _, hooks, ok := Get(def.ID); ok && hooks.DoRefresh != nil {
		tok, err := hooks.DoRefresh(ctx, &def, refreshToken, httpClient)
		if err != nil {
			return nil, err
		}
		if tok.Refresh == "" {
			tok.Refresh = refreshToken
		}
		if _, hooks, ok := Get(def.ID); ok && hooks.AfterRefresh != nil {
			if err := hooks.AfterRefresh(ctx, &def, tok, httpClient); err != nil {
				return nil, err
			}
		}
		return tok, nil
	}
	form := url.Values{}
	form.Set("client_id", def.ClientIDResolved())
	if s := def.ClientSecretResolved(); s != "" {
		form.Set("client_secret", s)
	}
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	tok, err := doTokenRequest(ctx, def.TokenURL, form, httpClient)
	if err != nil {
		return nil, err
	}
	if tok.Refresh == "" {
		tok.Refresh = refreshToken
	}
	if _, hooks, ok := Get(def.ID); ok && hooks.AfterRefresh != nil {
		if err := hooks.AfterRefresh(ctx, &def, tok, httpClient); err != nil {
			return nil, err
		}
	}
	return tok, nil
}

func doTokenRequest(ctx context.Context, tokenURL string, form url.Values, httpClient *http.Client) (*Tokens, error) {
	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, redactTokenError(string(body)))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("token endpoint returned invalid JSON: %w", err)
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access_token")
	}
	if data.RefreshToken == "" && form.Get("grant_type") == "authorization_code" {
		return nil, fmt.Errorf("no refresh token received — re-authorize with offline access")
	}
	exp := time.Now().UnixMilli() + data.ExpiresIn*1000 - 5*60*1000
	if data.ExpiresIn <= 0 {
		exp = time.Now().UnixMilli() + 55*60*1000
	}
	return &Tokens{Access: data.AccessToken, Refresh: data.RefreshToken, ExpiresAt: exp}, nil
}

// ParseCallbackURL extracts code/state from a pasted browser URL (headless flow).
// Accepts a full URL or a bare query string.
func ParseCallbackURL(raw, expectedState string) (code string, err error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", fmt.Errorf("paste the full callback URL from your browser address bar")
	}
	var u *url.URL
	if parsed, perr := url.Parse(text); perr == nil && parsed != nil && (parsed.Scheme != "" || strings.Contains(text, "code=")) {
		if parsed.Scheme == "" {
			parsed, _ = url.Parse("http://localhost/?" + strings.TrimPrefix(text, "?"))
		}
		u = parsed
	} else {
		u, _ = url.Parse("http://localhost/?" + strings.TrimPrefix(text, "?"))
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("provider returned error: %s", e)
	}
	code = q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		return "", fmt.Errorf("URL is missing code or state — paste the FULL callback URL")
	}
	if expectedState != "" && state != expectedState {
		return "", fmt.Errorf("state mismatch — that URL is from a different sign-in, start again")
	}
	return code, nil
}

func envGet(k string) string { return os.Getenv(k) }

// PKCEChallenge derives the S256 code_challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func redactTokenError(s string) string {
	// Keep provider error + description, drop anything long (tokens).
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
