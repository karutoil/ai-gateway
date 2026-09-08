package providers

import (
	"context"
	"net/http"
	"os"
	"strings"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/oauth"
)

func envOr(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func init() {
	oauth.Register(oauth.Definition{
		ID:       "antigravity",
		Name:     "Google Antigravity",
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: []string{
			"https://www.googleapis.com/auth/aicode",
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
			"https://www.googleapis.com/auth/cclog",
			"https://www.googleapis.com/auth/experimentsandconfigs",
		},
		ClientIDEnv:     "ANTIGRAVITY_CLIENT_ID",
		ClientSecretEnv: "ANTIGRAVITY_CLIENT_SECRET",
		ClientID:        antigravity.DecodedDefaultClientID(),
		ClientSecret:    antigravity.DecodedDefaultClientSecret(),
		UsePKCE:         true,
		ExtraAuthParams: map[string]string{
			"access_type": "offline",
			"prompt":      "consent",
		},
		UserInfoURL: "https://www.googleapis.com/oauth2/v1/userinfo?alt=json",
	}, oauth.Hooks{
		AfterExchange: func(ctx context.Context, def *oauth.Definition, tok *oauth.Tokens, httpClient *http.Client) error {
			email := antigravity.FetchUserEmail(tok.Access, httpClient)
			project := antigravity.LoadCodeAssist(tok.Access, httpClient)
			if project == "" {
				project = antigravity.DefaultProjectID(email)
			}
			// Env pin wins over discovery (mirrors pi-antigravity resolveProjectId).
			if pinned := envOr("ANTIGRAVITY_PROJECT_ID", "NOAGY_PROJECT_ID"); pinned != "" {
				project = pinned
			}
			tok.Email = email
			tok.ProjectID = project
			return nil
		},
		AfterRefresh: func(ctx context.Context, def *oauth.Definition, tok *oauth.Tokens, httpClient *http.Client) error {
			// Refresh responses carry no project/email — keep what login stored.
			// When missing (legacy rows), rediscover once.
			if tok.ProjectID == "" {
				project := antigravity.LoadCodeAssist(tok.Access, httpClient)
				if project == "" {
					project = antigravity.DefaultProjectID(tok.Email)
				}
				if pinned := envOr("ANTIGRAVITY_PROJECT_ID", "NOAGY_PROJECT_ID"); pinned != "" {
					project = pinned
				}
				tok.ProjectID = project
			}
			if tok.Email == "" {
				tok.Email = antigravity.FetchUserEmail(tok.Access, httpClient)
			}
			return nil
		},
	})
}
