# OAuth providers (Antigravity first, generic by design)

The gateway speaks API-key upstreams **and** OAuth upstreams through one
registry. Antigravity (Google OAuth → Cloud Code Assist) is the first entry;
the next OAuth provider should be ~30 lines, no handler or UI changes.

## User flow (Antigravity)

1. Providers → **Connect Google Antigravity** (or set Type to `antigravity (OAuth)` then Connect).
2. Google sign-in opens in a new tab. After approval the browser lands on
   `http://localhost:51121/oauth-callback?…` which cannot load — copy that full
   URL from the address bar.
3. Paste it into the Connect modal → gateway exchanges the code, stores
   encrypted tokens, discovers models.
4. Use models as `antigravity/gemini-3.8-flash`, `antigravity/claude-sonnet-4-6`, etc.
   via `/v1/chat/completions`, `/v1/messages`, or `/v1/responses`.

Tokens refresh automatically on use (5-minute skew). Health shows
`up (oauth)` when refresh works, `down` with “reconnect” when revoked.
Disconnect clears tokens but keeps the provider row.

## Operator env

| Variable | Purpose |
|---|---|
| `ANTIGRAVITY_BASE_URL` | Override API base (default daily host with sandbox + prod fallbacks). |
| `ANTIGRAVITY_PROJECT_ID` | Pin Cloud project instead of discovery. |
| `ANTIGRAVITY_CLIENT_ID` / `ANTIGRAVITY_CLIENT_SECRET` | Private Google OAuth app. Default is the public Antigravity desktop client (paste mode). |
| `ANTIGRAVITY_RUNTIME_MODEL` | Pin runtime id, bypass routing (debugging). |
| `ANTIGRAVITY_USER_AGENT` | Override request User-Agent. |

Custom OAuth apps: register `https://<gateway>/api/oauth/callback` as an
authorized redirect in Google Cloud, set the client env vars, then start login
with header `X-OAuth-Mode: redirect` to use server-side callback instead of paste.

## Adding a future OAuth provider

1. Create `internal/oauth/providers/<id>.go`:

```go
package providers

import "ai-gateway/internal/oauth"

func init() {
    oauth.Register(oauth.Definition{
        ID: "myprovider", Name: "My Provider",
        AuthURL: "https://accounts.example.com/authorize",
        TokenURL: "https://accounts.example.com/token",
        Scopes: []string{"models:read"},
        ClientIDEnv: "MYPROVIDER_CLIENT_ID",
        ClientSecretEnv: "MYPROVIDER_CLIENT_SECRET",
        UsePKCE: true,
    }, oauth.Hooks{
        AfterExchange: func(ctx context.Context, def *oauth.Definition, tok *oauth.Tokens, c *http.Client) error {
            tok.ProjectID = "default" // optional enrichment
            return nil
        },
    })
}
```

2. Map the definition to a provider type in
   `internal/handler/oauth.go:defToProviderType` (or reuse
   `openai_compatible` when the wire protocol is already OpenAI).
3. If the wire protocol is bespoke (like Antigravity), add
   `internal/<id>/translate.go` with `BuildRequest` + `ParseChunk`, then branch
   in `internal/proxy` the way `isAntigravity` does.
4. No UI, migration, or route changes needed: `/api/oauth/providers`,
   `/api/oauth/start`, `/api/oauth/exchange`, and the Providers OAuth card read
   the registry dynamically.

## Security notes

- Refresh/access tokens are AES-256-GCM encrypted with `MASTER_KEY` (same as
  API keys). Rotating `MASTER_KEY` without re-encrypting breaks stored OAuth.
- Pending logins (state + PKCE verifier) live in memory, single-use, 15-minute
  expiry. Restarting mid-login only requires starting again.
- Paste-mode redirect is fixed to loopback
  `http://localhost:51121/oauth-callback` so the bundled public client needs no
  per-deployment Google Cloud registration.
- `GET /api/oauth/callback` is public by necessity (Google redirects there);
  the single-use state is the capability, consumed atomically.
