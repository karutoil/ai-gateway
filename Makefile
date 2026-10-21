.PHONY: build run dev test web ui clean harness-muse e2e-live smoke fmt fmt-check lint lint-debt test-race pg-test pg-up pg-down docker-build openapi-check ci ci-all

# Local Postgres used by `make pg-test` (internal/db/pgsmoke). Chosen so it does
# not collide with a native cluster on 5432 or the compose/prod stacks on
# 5433/5434.
PG_TEST_PORT ?= 5432
PG_TEST_USER ?= gateway
PG_TEST_PASSWORD ?= gateway
# The pgsmoke TestMain DROPs and recreates schema public, so this database MUST
# be dedicated to the test suite. Never point GATEWAY_PG_DSN at a real database.
PG_TEST_DB ?= gwtest
GATEWAY_PG_DSN ?= postgres://$(PG_TEST_USER):$(PG_TEST_PASSWORD)@127.0.0.1:$(PG_TEST_PORT)/$(PG_TEST_DB)?sslmode=disable
# Pinned tool versions — keep in step with .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.15.0
OPENAPI_SPEC_VALIDATOR ?= openapi-spec-validator

build:
	go build -o bin/gateway ./cmd/gateway

# Installs from package-lock.json for reproducible builds (npm ci).
web:
	cd web && npm ci && npm run build

ui: web

run: build
	ADMIN_PASSWORD=admin123 PORT=8080 ./bin/gateway

dev:
	ADMIN_PASSWORD=admin123 PORT=8080 go run ./cmd/gateway

test:
	go test ./... -v

fmt:
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then echo "gofmt required on:"; echo "$$files"; exit 1; fi

# Blocking lint — the set CI enforces (.golangci.yml). Zero findings expected;
# a non-zero exit IS the signal.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint not found — install the pinned version with:"; \
	  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"; \
	  echo "  (or: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b \$$(go env GOPATH)/bin $(GOLANGCI_LINT_VERSION))"; \
	  exit 1; }
	golangci-lint run ./...

# Advisory: the full linter set, including the debt this repo still carries
# (errcheck ~746, revive ~477, unused ~21, staticcheck ~28 at the time of
# writing — 1272 findings total).
# Informational only — always exits 0, use it to pick the next linter to clean
# up and then move it into .golangci.yml.
lint-debt:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found (see 'make lint')"; exit 0; }
	-golangci-lint run --no-config \
	  --enable-only errcheck,govet,staticcheck,unused,misspell,revive \
	  --max-issues-per-linter 0 --max-same-issues 0 ./... || true

test-race:
	go test ./... -race -count=1

# ── Postgres dialect suite (internal/db/pgsmoke) ────────────────────────────
# Skips itself when GATEWAY_PG_DSN is unset, so CI fails the job on any SKIP
# line. Locally: `make pg-up pg-test pg-down` (needs podman or docker).
#
# The wait loop needs bash (jails: /bin/sh is dash and has no /dev/tcp), so it
# is invoked explicitly. It polls `pg_isready` INSIDE the container rather than
# just opening a TCP connection: the port accepts connections several seconds
# before initdb finishes, and a test run in that window fails with a confusing
# "connection reset by peer".
pg-up:
	@command -v podman >/dev/null 2>&1 && RUNTIME=podman || RUNTIME=docker; \
	$$RUNTIME rm -f gwtest-pg >/dev/null 2>&1 || true; \
	$$RUNTIME run -d --name gwtest-pg \
	  -e POSTGRES_USER=$(PG_TEST_USER) \
	  -e POSTGRES_PASSWORD=$(PG_TEST_PASSWORD) \
	  -e POSTGRES_DB=$(PG_TEST_DB) \
	  -p $(PG_TEST_PORT):5432 \
	  docker.io/library/postgres:16.10-alpine; \
	printf "waiting for postgres on 127.0.0.1:%s" "$(PG_TEST_PORT)"; \
	for i in $$(seq 1 90); do \
	  if $$RUNTIME exec gwtest-pg pg_isready -U $(PG_TEST_USER) -d $(PG_TEST_DB) >/dev/null 2>&1; then \
	    echo ' ready'; exit 0; \
	  fi; \
	  printf '.'; sleep 1; \
	done; \
	echo ' timed out'; $$RUNTIME logs --tail 20 gwtest-pg; exit 1

pg-down:
	@command -v podman >/dev/null 2>&1 && RUNTIME=podman || RUNTIME=docker; \
	$$RUNTIME rm -f gwtest-pg >/dev/null 2>&1; echo "gwtest-pg removed"

# Runs the suite verbosely and FAILS if any test skipped — the same guard CI
# applies, so a broken DSN cannot read as a green "ok ... [no tests to run]".
# POSIX sh only: /bin/sh is dash on Debian/Ubuntu, which has no PIPESTATUS.
pg-test:
	@log=$$(mktemp); \
	GATEWAY_PG_DSN='$(GATEWAY_PG_DSN)' go test ./internal/db/pgsmoke -count=1 -v > $$log 2>&1; \
	status=$$?; \
	cat $$log; \
	if grep -qE '^[[:space:]]*--- SKIP' $$log; then \
	  echo "FAIL: pgsmoke skipped tests — is GATEWAY_PG_DSN reachable?"; \
	  echo "  DSN: $(GATEWAY_PG_DSN)"; \
	  rm -f $$log; exit 1; \
	fi; \
	rm -f $$log; exit $$status

# Validate the OpenAPI document(s). Needs openapi-spec-validator:
#   pipx install openapi-spec-validator   (or: pip install --user ...)
openapi-check:
	@command -v $(OPENAPI_SPEC_VALIDATOR) >/dev/null 2>&1 || { \
	  echo "$(OPENAPI_SPEC_VALIDATOR) not found — pipx install openapi-spec-validator"; exit 1; }
	$(OPENAPI_SPEC_VALIDATOR) openapi.yaml
	@# The binary serves the EMBEDDED copy (cmd/gateway/openapi.yaml), so a stale
	@# or invalid embedded file is a client-visible defect even when the root
	@# spec is fine — it shipped broken once. Blocking, matching ci.yml.
	@test -f cmd/gateway/openapi.yaml || { \
	  echo "FAIL: cmd/gateway/openapi.yaml is missing — /openapi.yaml will not be served"; exit 1; }
	@$(OPENAPI_SPEC_VALIDATOR) cmd/gateway/openapi.yaml || { \
	  echo "FAIL: the EMBEDDED spec served at /openapi.yaml is invalid"; exit 1; }
	@diff -q openapi.yaml cmd/gateway/openapi.yaml >/dev/null || { \
	  echo "FAIL: embedded spec drifted from openapi.yaml — run: cp openapi.yaml cmd/gateway/openapi.yaml"; exit 1; }

# ── Local CI parity for .github/workflows/ci.yml ───────────────────────────
# Backend job only: gofmt -> vet -> build -> race suite -> Postgres suite.
# CI additionally runs the frontend, openapi and docker jobs; `make ci-all`
# covers those where the tooling exists locally.
ci: fmt-check
	go vet ./...
	go build ./...
	$(MAKE) test-race
	$(MAKE) pg-test

# Everything CI does, skipping the pieces whose tooling is optional locally
# (golangci-lint, openapi-spec-validator are reported, not fatal, here — in CI
# they are blocking).
ci-all: fmt-check
	go vet ./...
	go build ./...
	$(MAKE) lint
	$(MAKE) test-race
	$(MAKE) pg-test
	-$(MAKE) openapi-check
	cd web && npm ci && npx tsc -b && npm run build

docker-build:
	docker build -t ai-gateway:local .

# Muse 1.2 live harness — real provider via ckff-muse with 4 real tools
harness-muse:
	go build -o /tmp/harness-muse ./cmd/harness-muse
	GATEWAY_URL=${GATEWAY_URL:-http://localhost:8989} MODEL=${MODEL:-muse-spark-1.2-contributor} /tmp/harness-muse

# Full live suite: harness-muse + python SDK harness + go e2e
harness-muse-full:
	./scripts/muse-harness.sh --with-go-e2e

# Go live E2E (requires gateway running)
e2e-live:
	GATEWAY_URL=${GATEWAY_URL:-http://localhost:8989} go test -tags=live -run TestMuseLive -v ./internal/e2e

smoke: build
	./scripts/smoke.sh

clean:
	rm -rf bin data web/dist cmd/gateway/web
