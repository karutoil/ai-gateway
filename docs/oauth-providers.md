# OAuth providers (generic registry)

The gateway speaks API-key upstreams **and** OAuth upstreams through one
registry (`internal/oauth`). Each entry needs one `Register()` call — no
handler, route, migration, or UI changes.

| ID | Upstream | Flow | Transport |
|---|---|---|---|
| `antigravity` | Google → Cloud Code Assist | Standard OAuth2 PKCE | JSON over HTTPS |
| `devin` | Devin/Cognition | Custom PKCE (no client_id, JSON token exchange, long-lived session token) | Connect-proto protobuf over HTTPS |

## User flow (Antigravity)

1. Providers → **Connect Google Antigravity** (or set Type to `antigravity (OAuth)` then Connect).
2. Google sign-in opens in a new tab. After approval the browser lands on
   `http://localhost:51121/oauth-callback?…` which cannot load — copy that full
   URL from the address bar.
3. Paste it into the Connect modal → gateway exchanges the code, stores
   encrypted tokens, discovers models.
4. Use models as `antigravity/gemini-3.8-flash`, `antigravity/claude-sonnet-4-6`, etc.
   via `/v1/chat/completions`, `/v1/messages`, or `/v1/responses`.

## User flow (Devin)

1. Providers → **Connect Devin** (or set Type to `devin (OAuth)` then Connect).
2. Devin sign-in opens in a new tab. After approval the browser lands on
   `http://127.0.0.1:59653/callback?…` which cannot load — copy that full
   URL from the address bar.
3. Paste it into the Connect modal → gateway exchanges the code for a session
   token, stores it encrypted, discovers models.
4. Use models as `devin/swe-1-7`, `devin/swe-1-6`, etc.
   via `/v1/chat/completions`, `/v1/messages`, or `/v1/responses`.

Devin tokens are long-lived; refresh is a no-op that keeps the same
credentials. Health probes the session with a user-JWT request. Devin bills
via seat/quota, so logged token costs are zero — quota detail is not yet
surfaced (future work).

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
| `DEVIN_API_URL` | Override Devin web-API base (OAuth token exchange; default `https://api.devin.ai`). |
| `DEVIN_HOST` | Override Devin transport host (default `https://server.codeium.com`). |
| `DEVIN_WEBAPP_URL` | Override Devin authorize base (default `https://app.devin.ai`). |

Custom OAuth apps: register `https://<gateway>/api/oauth/callback` as an
authorized redirect in Google Cloud, set the client env vars, then start login
with header `X-OAuth-Mode: redirect` to use server-side callback instead of paste.

## Adding a future OAuth provider

Standard OAuth2 providers need only a definition plus optional enrichment:

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

Non-standard flows (like Devin: no client_id, JSON token exchange,
long-lived session token) override the relevant hook instead — see
`internal/oauth/providers/devin.go`:

- `Hooks.BuildAuthURL` replaces the authorize-URL builder.
- `Hooks.Exchange` replaces the form-encoded code exchange.
- `Hooks.DoRefresh` replaces the refresh_token grant (Devin: no-op).
- `Definition.PasteRedirectURI` sets the loopback callback for paste mode.

Then:

2. Map the definition to a provider type in
   `internal/handler/oauth.go:defToProviderType` (or reuse
   `openai_compatible` when the wire protocol is already OpenAI).
3. If the wire protocol is bespoke, add `internal/<id>/` with request
   building + response parsing, then branch in `internal/proxy` the way
   `isAntigravity`/`isDevin` do. Reuse `openAIChunkStreamer`,
   `serveChunksAsAnthropic`, and `serveChunksAsResponses` for the client
   dialects instead of reimplementing them.
4. Seed discovery from `internal/discovery` (static fallback + live merge),
   add a health case in `internal/provider/health.go`, extend the OpenAPI
   provider enum, and add the type to the Providers page select.
5. No route or OAuth-card changes needed: `/api/oauth/*` and the Providers
   OAuth card read the registry dynamically.

## Security notes

- Refresh/access tokens are AES-256-GCM encrypted with `MASTER_KEY` (same as
  API keys). Rotating `MASTER_KEY` without re-encrypting breaks stored OAuth.
- Pending logins (state + PKCE verifier) live in memory, single-use, 15-minute
  expiry. Restarting mid-login only requires starting again.
- Paste-mode redirect is per-definition loopback
  (`http://localhost:51121/oauth-callback` for Antigravity,
  `http://127.0.0.1:59653/callback` for Devin) so bundled public clients need
  no per-deployment app registration.
- `GET /api/oauth/callback` is public by necessity (providers redirect there);
  the single-use state is the capability, consumed atomically.
