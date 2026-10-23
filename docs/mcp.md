# Centralized MCP servers

The gateway owns your Model Context Protocol (MCP) servers. Register each one
**once**, then point every harness — Pi, OpenCode, Claude Code, Cursor, or your
own script — at a single authenticated endpoint (`/mcp`). The same registry is
also reachable from the LLM APIs through LiteLLM-compatible `mcp` tool
references, so an agent using the gateway's chat/messages/responses endpoints
can call your tools without any client-side MCP configuration at all.

- **One endpoint** — `POST /mcp` (Streamable HTTP), plus per-server and
  multi-server path forms.
- **Namespaced tools** — a tool `echo` on server `github` is exposed as
  `github-echo`, so tools from different servers never collide.
- **Per-key authorization** — an API key sees only the servers and tools it was
  granted.
- **Gateway-side execution** — with `require_approval: "never"`, the gateway
  runs the model's tool calls itself (bounded loop) and feeds the results back.
- **Encrypted credentials** — upstream tokens, static headers and stdio env are
  AES-256-GCM encrypted with the same master key that protects provider keys,
  and are never returned by the API.

---

## 1. Register a server

Dashboard → **MCP Servers** → **Add MCP server**. Or with the admin API:

```bash
curl -s -X POST "$GW/api/mcp/servers" \
  -H "Authorization: Bearer $DASHBOARD_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "GitHub",
    "transport": "http",
    "url": "https://mcp.example.com/mcp",
    "auth_type": "bearer",
    "auth_value": "ghp_…"
  }'
```

| Field | Meaning |
|---|---|
| `name` | Display name. A tool namespace (`slug`) is derived from it. |
| `slug` | Optional explicit namespace prefix. Must be unique; auto-suffixed on collision. |
| `transport` | `http` (Streamable HTTP, the default), `sse` (legacy HTTP+SSE) or `stdio` (a process the gateway launches locally). |
| `url` | Endpoint for `http`/`sse`. |
| `command`, `args` | Executable and arguments for `stdio`. |
| `env` | `KEY=value` map for `stdio` (encrypted). |
| `headers` | Static headers sent on every upstream request (encrypted). |
| `auth_type` | `none`, `bearer` (`Authorization: Bearer …`), `api_key` (header, defaults to `X-API-Key`) or `header` (custom name, verbatim value). |
| `auth_header` | Header name for `api_key`/`header`. |
| `auth_value` | The credential (encrypted). **On update, leave blank to keep the stored one.** |
| `enabled` | Whether the server participates in `/mcp` and in LLM references. |

Saving kicks off discovery immediately; the row's health badge and tool count
fill in within a second or two. **Test** re-probes and refreshes the tool cache
on demand, and each entry can be edited, enabled/disabled or deleted from the
same table.

A background health loop re-probes enabled servers every
`MCP_HEALTH_INTERVAL_SECONDS` (default 60) and reconnects with exponential
backoff (2s → 60s) after a failure, so a restarting upstream heals itself.

## 2. Connect a harness

The endpoint is `/mcp` on the gateway origin. Authenticate with a gateway key
(`sk-gw-…`). MCP clients reserve `Authorization` for upstream OAuth, so the
preferred header is LiteLLM's convention:

```json
{
  "mcpServers": {
    "ai-gateway": {
      "url": "https://gateway.example.com/mcp",
      "headers": { "x-litellm-api-key": "Bearer sk-gw-…" }
    }
  }
}
```

`Authorization: Bearer sk-gw-…` and `x-api-key` are also accepted, so a client
that can only set one header still works.

### Scoping a connection

| Path | Exposes |
|---|---|
| `/mcp` | every server and tool the key may use |
| `/<server>/mcp` | that one server |
| `/mcp/<server>` | that one server (LiteLLM's own grammar) |
| `/mcp/<a>,<b>` | multiple servers, comma-separated |

Path selection only **narrows** access — it can never grant a tool the key was
not already given. Naming a server the key cannot use answers `404` with a
JSON-RPC error rather than an empty tool list, because an empty list reads like
a broken server.

`x-mcp-servers: github,filesystem` (or `?servers=`) does the same per request.

### REST helpers

For scripted use with no MCP client and no LLM in the loop:

```bash
# which servers may this key use?
curl -s "$GW/v1/mcp/server" -H "x-litellm-api-key: Bearer $KEY"

# the namespaced tool catalogue
curl -s "$GW/mcp-rest/tools/list" -H "x-litellm-api-key: Bearer $KEY"

# call one tool
curl -s -X POST "$GW/mcp-rest/tools/call" \
  -H "x-litellm-api-key: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"server_id":"<id>","name":"echo","arguments":{"text":"hi"}}'
```

## 3. Per-key permissions

Dashboard → *MCP Servers* → **MCP access** next to a key (or
`GET`/`PUT /api/keys/{id}/mcp`).

- A key with **no** grants sees every enabled server and tool. That keeps
  single-tenant deployments permissive by default.
- Granting **at least one** server switches that key to an explicit allowlist:
  only granted servers are visible, and everything else disappears from
  `/mcp`.
- `allowed_tools` inside a grant restricts the key to those tools; an empty
  list allows every tool on that granted server.

Revoking all grants returns the key to the permissive default.

## 4. LiteLLM-compatible MCP references

Add an `mcp` entry to `tools` in a chat/completions, messages or responses
request. `server_url` is the literal sentinel `litellm_proxy` — never a network
URL (a real URL is rejected, so a client cannot make the gateway fetch an
arbitrary host).

```json
{
  "model": "openai/gpt-4o-mini",
  "messages": [{"role": "user", "content": "Find the open bugs in my repo"}],
  "tools": [
    {
      "type": "mcp",
      "server_url": "litellm_proxy",
      "require_approval": "never",
      "allowed_tools": []
    }
  ]
}
```

| `server_url` | Selects |
|---|---|
| `litellm_proxy` | every server the key may use |
| `litellm_proxy/mcp/github` | one server |
| `litellm_proxy/mcp/github,filesystem` | those servers |

| Field | Behaviour |
|---|---|
| `allowed_tools` | Filters the expanded tools by name (`search_issues`) or namespaced name (`github-search_issues`). Empty/absent permits every authorized tool. |
| `require_approval: "never"` | Gateway-side automatic execution: the gateway runs the tool calls and feeds the results back to the model until it answers. |
| anything else (`"always"`, `"on_request"`, a per-tool map, absent) | No automatic execution. The reference still expands into real function tools and the model's tool calls are returned **to the caller**, who keeps control of approval. |

References are expanded into ordinary function tools in the dialect of the
inbound endpoint (`functions` for chat completions, `input_schema` blocks for
messages, flat `function` items for responses), and the `mcp` entry itself is
removed before the request reaches the provider. Requests without MCP
references are forwarded byte-for-byte unchanged.

### Automatic execution

With `require_approval: "never"` the gateway:

1. sends the request with the expanded tools,
2. parses the model's tool calls (JSON or SSE, all three dialects),
3. executes each authorized call through the MCP client,
4. appends the assistant tool-call turn and the tool results in the inbound
   protocol's shape,
5. repeats until the model answers without calling a tool.

Safety limits, all enforced server-side and all configurable:

| Guard | Default | Env |
|---|---|---|
| Max tool rounds per request | 25 | `MCP_MAX_ROUNDS` |
| One MCP operation (connect/list/call) and one LLM round | 90s | `MCP_CALL_TIMEOUT_SECONDS` |
| Whole loop per client request | 600s | `MCP_LOOP_TIMEOUT_SECONDS` |

Execution stops with a machine-readable reason instead of looping:

- `mcp_max_rounds_exceeded` — the round cap was reached.
- `mcp_repeated_tool_call` — the model issued an **identical** call again
  (same names and arguments) instead of making progress. Without this a stuck
  model would re-execute a side-effecting tool up to the round cap.
- `mcp_timeout` — the loop budget expired.
- A tool that reports an error does **not** abort: its error text is returned
  to the model as the tool result, which is what lets the model recover.

Execution metadata is reported both ways:

- **Headers** (non-streaming): `X-MCP-Rounds`, `X-MCP-Tool-Calls`,
  `X-MCP-Tools`, `X-MCP-Servers`, plus `X-MCP-Error` and
  `X-MCP-Approval-Required` when they apply.
- **Structured body fields** (both streaming and non-streaming):
  `mcp_rounds`, `mcp_tool_calls[]` (`server`, `tool`, `name`, `call_id`,
  `round`, `is_error`, `duration_ms`) and `mcp_error`.

**Streaming.** A live stream cannot be inspected for tool calls, so each round
is buffered and the final answer is emitted as a well-formed SSE stream for the
dialect (chat deltas + `[DONE]`, Anthropic `message_start`/`content_block_*`/
`message_delta`/`message_stop`, or a `response.completed` event). Keepalive
comments are sent while the tool rounds run. Streaming clients read the
execution metadata from the terminal event, because response headers are
already committed by then.

### What the model sees

Only tools in the plan are offered — i.e. the intersection of
(reference selection) × (key grants) × (`allowed_tools`). A model cannot call a
tool that was not offered, and the server refuses by name if it tries.

## 5. Audit

Every tool execution writes a row (server, tool, namespaced name, key prefix,
endpoint, status, error, duration). Arguments and results are deliberately
**not** stored: they routinely contain source code and secrets. Recent rows are
listed on the MCP Servers page; `GET /api/mcp/calls` returns them as JSON.

## 6. Configuration reference

| Env var | Default | Meaning |
|---|---|---|
| `MCP_MAX_ROUNDS` | `25` | Tool-execution rounds per request. |
| `MCP_CALL_TIMEOUT_SECONDS` | `90` | Timeout for one connect/list/call and for one LLM round inside the loop. |
| `MCP_LOOP_TIMEOUT_SECONDS` | `600` | Total budget for the gateway-side loop. |
| `MCP_HEALTH_INTERVAL_SECONDS` | `60` | Background probe/reconnect interval. |
| `MCP_STDIO_ENABLED` | `false` | Allow `stdio`-transport servers, which spawn a local binary as the gateway user. Even when enabled, only a **global admin** may create one. |
| `MCP_ALLOW_PRIVATE_UPSTREAMS` | `false` in production | Allow server URLs that resolve to loopback / RFC1918 / link-local / cloud-metadata addresses. Off in production (SSRF guard); on outside production so a local sidecar works. |

No extra secret is needed: MCP credentials use `MASTER_KEY`, the same AES-GCM
key that protects provider API keys.

## 7. Security notes

- `env` / `headers` are **tri-state** on update: omit the field to keep the
  stored blob, send `{}` to clear it. A blank `auth_value` keeps the stored
  credential. (An update used to write NULL over both blobs, so renaming a
  server destroyed its `GITHUB_TOKEN` and static auth headers irrecoverably.)
- A spawned `stdio` server inherits only an allowlist of harmless process
  variables (PATH/HOME/locale/proxy/cache), minus anything credential-shaped.
  It never sees `MASTER_KEY`, `JWT_SECRET`, `DATABASE_URL` or the provider keys
  in the gateway's environment — a third-party `npx -y @vendor/…-mcp` package is
  freshly downloaded code and must not be able to exfiltrate them.
- Revoking every grant from a key denies everything. "No grant rows" and "never
  configured" used to be indistinguishable, and an unconfigured key is allowed
  every enabled tool — so "revoke all" granted the widest possible access.
- Grants are re-checked at **call time**, so an edit takes effect on live
  sessions instead of only on the next `initialize`.
- A tool call is retried only on a genuine transport error (`io.EOF`,
  `ECONNRESET`, `net.Error`, …). Matching on the error *text* retried upstream
  tool failures that merely mentioned "session" or "closed", duplicating
  committed side effects.
- Credentials are encrypted at rest and never serialized to the dashboard; the
  API only reports *whether* a credential is configured (`has_auth`,
  `has_headers`, `has_env`).
- Upstream credentials are attached at the HTTP transport layer, so they never
  appear in a request body, a log line or a response.
- `/mcp` requires a gateway key and runs behind the same budget and rate-limit
  middleware as `/v1`, so a stolen key cannot bypass spend controls.
- `server_url` must be the `litellm_proxy` sentinel; an arbitrary URL is
  rejected so the gateway cannot be used as a server-side request forgery hop.
- A browser navigation to `/mcp` (`Accept: text/html`) serves the dashboard;
  every API/MCP request gets the protocol. Both paths pass through the same
  auth, budget and rate-limit chain.
- `stdio` servers execute a local binary as the gateway's user. Only register
  commands you would run yourself.
