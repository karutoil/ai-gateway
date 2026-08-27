.PHONY: build run dev test web ui clean harness-muse e2e-live smoke fmt fmt-check lint test-race docker-build ci

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

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint not found — install with:"; \
	  echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest  (or brew install golangci-lint)"; \
	  exit 0; }
	golangci-lint run ./...

test-race:
	go test ./... -race -count=1

docker-build:
	docker build -t ai-gateway:local .

# Local CI parity for .github/workflows/ci.yml backend job.
ci: fmt-check
	go vet ./...
	go build ./...
	$(MAKE) test-race

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

smoke:
	./scripts/smoke.sh

clean:
	rm -rf bin data web/dist cmd/gateway/web
