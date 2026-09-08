package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-gateway/internal/devin"
	"ai-gateway/internal/oauth"
)

func init() {
	oauth.Register(oauth.Definition{
		ID:               "devin",
		Name:             "Devin",
		AuthURL:          devin.WebappURL() + "/auth/cli/continue",
		TokenURL:         devin.APIURL() + "/auth/cli/token",
		UsePKCE:          true,
		PasteRedirectURI: devin.PasteRedirectURI,
	}, oauth.Hooks{
		BuildAuthURL: func(def *oauth.Definition, p oauth.PendingLogin) string {
			q := url.Values{}
			q.Set("redirect_uri", p.RedirectURI)
			q.Set("state", p.State)
			q.Set("prompt", "select_account")
			q.Set("code_challenge", oauth.PKCEChallenge(p.Verifier))
			q.Set("code_challenge_method", "S256")
			return def.AuthURL + "?" + q.Encode()
		},
		Exchange: func(ctx context.Context, def *oauth.Definition, p oauth.PendingLogin, code string, httpClient *http.Client) (*oauth.Tokens, error) {
			return exchangeDevinCode(ctx, p.Verifier, code, httpClient)
		},
		DoRefresh: func(ctx context.Context, def *oauth.Definition, refreshToken string, httpClient *http.Client) (*oauth.Tokens, error) {
			// Devin session tokens are long-lived; the reference client
			// treats refresh as a no-op returning the same credentials.
			return &oauth.Tokens{
				Access: refreshToken, Refresh: refreshToken,
				ExpiresAt: devin.TokenExpiry(refreshToken),
			}, nil
		},
	})
}

// exchangeDevinCode performs Devin's JSON token exchange:
// POST {api}/auth/cli/token {code, code_verifier} -> {token}.
func exchangeDevinCode(ctx context.Context, verifier, code string, httpClient *http.Client) (*oauth.Tokens, error) {
	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint := strings.TrimRight(devin.APIURL(), "/") + "/auth/cli/token"
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("devin token exchange failed (%d): %s", resp.StatusCode, truncateForError(string(raw)))
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("devin token endpoint returned invalid JSON: %w", err)
	}
	if data.Token == "" {
		return nil, fmt.Errorf("devin token exchange returned no token")
	}
	return &oauth.Tokens{
		Access: data.Token, Refresh: data.Token,
		ExpiresAt: devin.TokenExpiry(data.Token),
	}, nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
