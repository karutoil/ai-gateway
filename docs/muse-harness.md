# Muse Spark 1.2 — Live Harness & Real Provider Tests

This doc describes the **real-provider test harness** for `muse-spark-1.2-contributor` via `ckff-muse` (anthropic @ https://ckff.dev). It verifies **all gateway endpoints** with **real tools** to ensure OpenAI ↔ Anthropic ↔ Responses translation is correct.

## Quick Start

```bash
# 1. Ensure gateway running (port 8989 as per .env)
PORT=8989 ADMIN_PASSWORD=admin123 ./bin/gateway &
# or: make dev  # PORT=8080 variant

# 2. Ensure provider exists (creates ckff-muse if missing)
./scripts/setup-muse-provider.sh
# or: GATEWAY_URL=http://localhost:8989 CKFF_API_KEY=sk-... ./scripts/setup-muse-provider.sh

# 3. Run the comprehensive Go harness (real tools, all endpoints)
make harness-muse
# or: GATEWAY_URL=http://localhost:8989 go run ./cmd/harness-muse
# or: GATEWAY_URL=http://localhost:8989 /tmp/harness-muse  # after make harness-muse

# 4. Run Go live E2E (go test with -tags=live)
make e2e-live
# or: GATEWAY_URL=http://localhost:8989 go test -tags=live -run TestMuseLive -v ./internal/e2e

# 5. Full suite (harness-muse + python SDK + go e2e)
make harness-muse-full
# or: ./scripts/muse-harness.sh --with-go-e2e
```

## What It Tests

### Provider
- **Provider:** `ckff-muse` / `ckff` (type `anthropic`, base `https://ckff.dev`)
- **Model:** `muse-spark-1.2-contributor` (context 1M, max_output 131k, reasoning effort, tool_call true)
- **Reasoning levels:** `minimal, low, medium, high, xhigh` (tested: low/medium/high)

### Real Tools (4)
All harnesses use **the same 4 real tools** to verify translation both ways:

| Tool | Purpose | Params |
|------|---------|--------|
| `get_weather` | Weather lookup | `location` (string), `unit` (celsius/fahrenheit) |
| `calculate` | Math | `expression` (string) |
| `search_docs` | KB search | `query` (string), `limit` (int) |
| `create_task` | Task creation | `title`, `description`, `priority` (low/medium/high) |

OpenAI format: `{type:"function", function:{name, description, parameters}}`
Anthropic format: `{name, description, input_schema}`
Gateway must translate both directions correctly (including `tool_choice` auto).

### Endpoints Covered (harness-muse: 31 tests)

**OpenAI Chat Completions** (`/v1/chat/completions`, `/chat/completions`):
- basic non-stream, with system prompt, stream, no /v1 prefix
- reasoning `low/medium/high` and invalid (`ultra` → 400)
- tools auto (1 and 4 tools), tools required (lenient: provider only supports auto), stream with tools, multi-turn tool flow, reasoning+tools

**Completions (legacy)** (`/v1/completions`):
- `prompt` → `messages` translation for Muse (Muse requires messages)

**Embeddings** (`/v1/embeddings`):
- lenient: Muse (anthropic) may return 400, but gateway must not crash (no 404/500)

**Models** (`/v1/models`, `/models`):
- enriched list with provider_models + catalog + aliases

**Anthropic Messages** (`/v1/messages`, `/messages`):
- basic non-stream, no /v1, with system, stream, reverse stream, tools (1/4), stream with tools, multi-turn tool flow

**Responses API** (`/v1/responses`, `/responses`):
- input string, no /v1, input_text blocks, with instructions, stream, stream via chat (Responses→Chat→Anthropic)

**Negative & Edge:**
- no auth → 401, unknown model → 503/400, invalid JSON → 400/500 (lenient), provider `tool_choice=required` → SKIP with warning

**Observability:**
- logs have `ttft_ms`, tokens, cost; metrics endpoint

## Harness Details

### `cmd/harness-muse/main.go` (Preferred for AI)
- **Language:** Go, single binary, no extra deps
- **Client:** `http.Client{Timeout:45s}` for all requests, streaming deadline 25s
- **Auth:** auto-login via `ADMIN_PASSWORD`, auto-creates gateway key `harness-muse-<ts>`
- **Provider:** auto-ensures `ckff-muse` exists (creates via `CKFF_API_KEY` if missing, default key from `scripts/fix_ckff.sh`)
- **Output:** per-test `[name] PASS/FAIL` with SKIP notes for provider limitations
- **Exit:** 0 on all PASS (SKIP counts as PASS), 1 on any FAIL
- **Run:** `GATEWAY_URL=http://localhost:8989 /tmp/harness-muse`
- **Build:** `go build -o /tmp/harness-muse ./cmd/harness-muse`

### `internal/e2e/muse_live_test.go` (Go test, tag live)
- **Tag:** `//go:build live` — only runs with `-tags=live`
- **Usage:** `GATEWAY_URL=http://localhost:8989 go test -tags=live -run TestMuseLive -v ./internal/e2e`
- **Sub-tests:** 15 sub-tests (Health, Models, ChatBasic/Stream/Tools/Reasoning, AnthropicBasic/Stream/Tools, ResponsesBasic/InputText/Stream, Completions, ModelsNoAuth)
- **Isolation:** each sub-test is independent, uses same gatewayKey
- **Timeout:** 45s per request, 42s total in CI (as of 2026-08-20)

### `scripts/harness_official_sdks.py` (Python, Official SDKs)
- **SDKs:** `openai` (base_url=`GATEWAY_URL/v1`) and `anthropic` (base_url=`GATEWAY_URL`)
- **Coverage:** 9 tests: OpenAI chat non-stream/stream/tools, Anthropic messages non-stream/no-v1/tools/stream, Responses, Completions (lenient)
- **Usage:** `GATEWAY_URL=http://localhost:8989 python3 scripts/harness_official_sdks.py`
- **Install:** `pip install openai anthropic requests`
- **Note:** Official SDKs handle base_url quirks (anthropic appends `/v1/messages`). Harness tests both `/v1/messages` and `/messages`.

### `scripts/muse-harness.sh` (Full Orchestrator)
- **Steps:** ensure gateway → ensure provider → build harness-muse → run harness-muse → (optional) python harness → (optional go e2e)
- **Usage:** `./scripts/muse-harness.sh` or `./scripts/muse-harness.sh --with-go-e2e`
- **Env:** `GATEWAY_URL`, `MODEL`, `PROVIDER`, `CKFF_API_KEY`, `ADMIN_PASSWORD`

### `scripts/setup-muse-provider.sh` (Provider Bootstrap)
- **Purpose:** Idempotent provider creation + model discover + gateway key
- **Usage:** `./scripts/setup-muse-provider.sh`
- **Output:** prints curl test command

## Provider Setup (for AI)

If gateway has no provider, the harness will auto-create it, but you can also setup manually:

```bash
ADMIN_TOKEN=$(curl -s -X POST http://localhost:8989/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)
curl -s -X POST http://localhost:8989/api/providers \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"ckff-muse\",\"type\":\"anthropic\",\"base_url\":\"https://ckff.dev\",\"api_key\":\"$MUSE_API_KEY\"}" | jq .

# Discover models
PROV_ID=$(curl -s http://localhost:8989/api/providers -H "Authorization: Bearer $ADMIN_TOKEN" | jq -r '.[] | select(.name=="ckff-muse") | .id')
curl -s -X POST http://localhost:8989/api/providers/$PROV_ID/discover -H "Authorization: Bearer $ADMIN_TOKEN" | jq .

# Verify
curl -s "http://localhost:8989/api/provider-models?limit=100" -H "Authorization: Bearer $ADMIN_TOKEN" | jq '.data[] | select(.model_id=="muse-spark-1.2-contributor")'
```

API key default is from `scripts/fix_ckff.sh`. Override via `CKFF_API_KEY` env.

## Known Provider Limitations (handled as SKIP)

- `tool_choice` only supports `"auto"` on ckff-muse (ckff.dev). Tests for `required`/`none`/named will SKIP with warning, not FAIL.
- Muse may not call tool for some prompts even with `tool_choice:auto` — harness treats no-tool-call as SKIP (lenient).
- `completions` for Muse is translated to chat (prompt→messages) — harness checks not 404/bad_response, not strict content.
- `embeddings` not supported for anthropic Muse — harness checks not 404, allows 400.

## Makefile Targets

```makefile
make harness-muse       # build + run Go harness (real provider, 4 tools)
make harness-muse-full  # + python SDK + go e2e
make e2e-live           # go test -tags=live ./internal/e2e
```

## For PI / AI Agent

**Preferred:** `cmd/harness-muse` (Go) — zero deps, deterministic, covers all endpoints, handles provider quirks as SKIP, uses real tools.

```bash
# PI example
GATEWAY_URL=http://localhost:8989 go run ./cmd/harness-muse
# or built binary
go build -o /tmp/harness-muse ./cmd/harness-muse && GATEWAY_URL=http://localhost:8989 /tmp/harness-muse
```

**Alternative:** `internal/e2e/muse_live_test.go` for `go test` integration:

```bash
GATEWAY_URL=http://localhost:8989 go test -tags=live -run TestMuseLive -v ./internal/e2e
```

**Python:** `scripts/harness_official_sdks.py` for SDK-level verification (requires `pip install openai anthropic`).

All harnesses share same 4 real tools and same model `muse-spark-1.2-contributor`.

## Troubleshooting

- **Gateway not running:** `./scripts/muse-harness.sh` will auto-start `bin/gateway` on `$PORT`, or run `PORT=8989 ./bin/gateway &`
- **Provider missing:** Run `./scripts/setup-muse-provider.sh`
- **Model not in provider_models:** `curl -X POST /api/providers/<id>/discover` and wait 5s
- **Logs:** `curl http://localhost:8989/api/logs?limit=5 -H "Authorization: Bearer $ADMIN_TOKEN" | jq .`
- **Metrics:** `curl http://localhost:8989/metrics`
- **TTFT check:** harness verifies logs have `ttft_ms` >0

## References

- `openapi.yaml` — gateway API spec
- `ARCHITECTURE.md` — phase plan, data model
- `internal/translate/translate.go` — Anthropic↔OpenAI + Responses→Chat
- `internal/proxy/proxy.go` — streaming translation (handleAnthropicStreamViaOpenAI etc.)
