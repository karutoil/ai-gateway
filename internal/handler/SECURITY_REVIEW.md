# HTTP Handler Security Review — Issue Report

Scope: every file under `internal/handler/` (post-merge f25d919), reviewed
line by line against the assignment checklist (IDOR, SQL injection via log
filters, unbounded queries, panic-inducing params, reflected XSS, missing
RBAC, CSRF, unbounded JSON bodies, open redirect). One wiring line in
`cmd/gateway/main.go` was changed out of necessity (see issue 3).

## Fixed issues

### 1. MCP: cross-org IDOR on all by-id endpoints (mcp.go) — HIGH
`mcp:write` is a **default permission for member and support roles**. The
list endpoints and Update/Delete were org-scoped, but `GET /mcp/servers/{id}`,
`GET /mcp/servers/{id}/tools`, `POST .../enable`, `.../disable`, `.../test`
and `.../discover` were not: any authenticated member could read another
org's server config (URL, command, args) and mutate its state by id.
Fix: `requireServerScope` helper (admin passes; global servers shared;
org-scoped callers only their own org; **404 not 403** so ids are not
enumerable). A non-admin `Update` can also no longer re-home a server into a
different org.
Tests: `TestMCPServerByIDEndpointsAreOrgScoped`,
`TestMCPUpdateCannotMoveServerBetweenOrgs`.

### 2. MCP: cross-org key-permission endpoints (mcp.go) — MEDIUM
`GET/PUT /api/keys/{id}/mcp` operated on any key id regardless of org
(mcp:read is even a **readonly** default). Cross-org grants are inert at
runtime (`AccessForKey` re-checks org), but the read leaks another tenant's
grant rows and full server roster, and the write pollutes them.
Fix: `requireKeyScope` — admins pass; org-scoped callers require
`gateway_keys.org_id = claim` (404 otherwise); unscoped non-admins pass only
while no organizations exist (single-tenant contract, same as
`AdminHandler.resolveScope`). Needs the new `MCPHandler.DB` field.
Tests: `TestMCPKeyPermissionsAreOrgScoped`.

### 3. One-line out-of-scope wiring (cmd/gateway/main.go) — flagged
`mcpHandler` construction now passes `DB: database` so the fix above is
active in production (without it the guard fails closed and would regress
member access). This is the only file touched outside `internal/handler/`.

### 4. Org: read routes had no RBAC gate (org.go) — MEDIUM
`GET /orgs` and `GET /orgs/{id}/members` had **no permission check at all**
(the other three org routes require `orgs:write`): every authenticated user
could enumerate orgs and memberships (user ids + roles), and a revoked
`orgs:read` was silently ignored. Fix: both gated with
`RequirePerm(rbac.PermOrgsRead)`.
Audit rows also attributed every org write to a hardcoded `"admin"`; they now
record the acting subject.
Tests: `TestOrgReadRoutesRequirePermission`, `TestOrgAuditRecordsActualActor`.

### 5. OAuth surface: cross-org provider access (oauth.go) — HIGH
`providers:write` is a **default for member and support roles**, but none of
the OAuth endpoints were org-scoped (unlike `PUT/DELETE /providers/{id}`,
which use `requireWriteTarget`):
- `POST /oauth/start` with a `provider_id`, or a same-named provider reuse,
  let a member attach OAuth tokens to **another org's (or a global) provider**;
- unscoped members could mint **global** provider rows anyone can then see;
- `POST /oauth/exchange` accepted a client-supplied `provider_id` without any
  ownership check;
- `GET .../oauth/status`, `.../usage`, `POST .../oauth/refresh` and
  `DELETE .../oauth` (disconnect = credential wipe) worked on any provider id.
Fix: `requireProviderScope` on Start/Exchange/Status/Refresh/Disconnect/Usage
(404, not 403); name-reuse of an out-of-scope provider is refused like a
duplicate create; non-admin creates are pinned to the caller's verified org
claim and denied when unscoped.
Tests: `TestOAuthProviderScopeForNonAdmins`.

### 6. Webhooks: partial update destroyed fields (webhooks.go) — MEDIUM
`PUT /webhooks/{id}` unconditionally overwrote `events`, `format` and
`enabled`: renaming a webhook cleared its event filter (silently widening
delivery to ALL events), reset discord/slack formats to json, and
**re-enabled disabled webhooks**. The URL check also accepted any
"http"-prefixed junk (`httpjunk:...`), unlike Create.
Fix: strict `http(s)://` validation on both create and update, length caps,
and COALESCE/nullable-arg semantics so omitted fields keep their stored
value. `List` now also sets `Content-Type: application/json`.
Tests: `TestWebhookUpdatePreservesOmittedFields`,
`TestWebhookUpdateRejectsNonHTTPURL`.

### 7. Unbounded latency percentile queries (admin.go, key_analytics.go) — LOW
`Stats` and `KeyAnalytics` loaded **every** latency row of the window into
memory (LogGroup already capped at 100k, perfBy at 200k). Both now use
`latencySampleMax = 200000` (rows are pre-sorted by SQL, so percentiles stay
exact up to the cap).

### 8. Invalid JSON error envelopes (admin.go, users.go) — LOW
`RotateKey` and `PutUserPermissions` hand-concatenated `err.Error()` into
JSON bodies — a store error containing a quote (e.g. `unknown permission
"bad\"perm"`) produced an unparseable response. Both now use the `httperr`
envelope. Tests: `TestPutUserPermissionsErrorEnvelopeIsJSON`.

### 9. Unguarded mutation surface in a dead route table (discovery.go) — LOW
`DiscoveryHandler.Routes` is not what `cmd/gateway` mounts (routes are wired
individually, each with `RequirePerm`), but the method registered mutating
routes with no gate — a footgun for any future mounting. All mutating routes
in it are now gated on `catalog:write`.

## Reviewed and found OK (no change needed)

- **SQL injection via log filters**: `logFilterWhere` is fully parameterized
  (LIKE wildcards in `q` are escaped via `logSearchLIKE`; the plain `model`
  filter can inject `%`/`_` but that is a match-breadth nuisance, not
  injection). `logOrderClause` whitelists sort columns; `logGroupDims` is a
  fixed map; `attributionBy`/`costBy`/`perfBy` expressions are internal
  literals only. `requireWriteTarget`'s table arg is a call-site constant.
- **CSRF**: `middleware.CSRFProtection` wraps all `/api` mutations
  (cmd/gateway/main.go) and the session cookie is `HttpOnly; SameSite=Lax`
  (Secure on https). Bearer-token clients are unaffected.
- **JSON body size limits**: every decode path in the package uses
  `http.MaxBytesReader` (re-verified file by file).
- **Open redirect**: the only redirect (`oauth.Callback`) targets a fixed
  path with a server-generated uuid param.
- **Panics from query params**: every `strconv.Atoi` error is handled;
  reversed analytics from/to ranges are tolerated; unknown `group_by`
  dimension → 400.
- **Pagination**: `/logs`, `/logs/group`, `/audit`, attribution and perf
  queries are all bounded; ListUsers/ListOrgs/ListMembers/ListAliases are
  unbounded but over naturally-small operator tables.
- **Secrets in responses**: MCP server JSON redacts secrets (`json:"-"`);
  provider responses mask `api_key`; PAT/webhook secrets are one-time.
- **IDOR already correctly handled** (pre-existing): logs/stats/billing/
  analytics org + `keys:read_own` ownership narrowing; `pat.Revoke` filters
  by user id; `requireWriteTarget` on key/provider mutations.

## Known remaining risks (not fixed — out of scope or accepted)

- **SSRF by privileged users**: webhook and MCP server URLs can target
  internal addresses (e.g. cloud metadata). Both are gated to
  settings:write / mcp:write (admin-only defaults for settings:write). A
  network-level egress policy is the right fix (deployment concern).
- **Audit trail org scoping**: `/api/audit` is global, gated by
  `audit:read` (admin-only by default) — consistent with the current
  permission model, but worth revisiting if support roles ever get
  `audit:read`.
- The `X-Total-Count`/`X-Rows-Skipped` headers and CSV exports are scoped by
  the same filters as the row queries — re-verified.
