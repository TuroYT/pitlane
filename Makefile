VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Go tools run with `go run`, so they are built with the toolchain go.mod asks
# for. Renovate proposes new versions (see renovate.json).
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
GOLANGCI_LINT_VERSION ?= v2.15.0
# renovate: datasource=go depName=gotest.tools/gotestsum
GOTESTSUM_VERSION ?= v1.13.0

GOLANGCI_LINT = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOTESTSUM = go run gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

.PHONY: help build web run dev test lint golangci-lint test-race clean

help: ## List available Makefile targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "%-13s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the pitlane binary (with the embedded UI)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/pitlane ./cmd/pitlane

web: ## Build the web UI into web/dist
	cd web && pnpm install --frozen-lockfile && pnpm build

run: web build ## Run locally against the local Docker daemon (no login)
	./bin/pitlane --data ./.data --insecure-no-auth --listen 127.0.0.1:8080

dev: ## Run the UI dev server (proxying to a Pitlane on :8080)
	cd web && pnpm dev

test: ## Run Go vet and tests
	go vet ./... && go test ./...

lint: ## What CI's "Go / lint" checks: gofmt, go vet, go.mod tidiness, golangci-lint
	@test -z "$$(gofmt -l ./cmd ./internal ./web)" || { gofmt -l ./cmd ./internal ./web; echo "run gofmt -w on the files above"; exit 1; }
	go vet ./...
	go mod tidy -diff
	$(MAKE) --no-print-directory golangci-lint

golangci-lint: ## Run golangci-lint only (extra flags in GOLANGCI_LINT_ARGS)
	$(GOLANGCI_LINT) run $(GOLANGCI_LINT_ARGS) ./...

test-race: ## What CI's "Go / test (race)" runs: race detector and coverage
	$(GOTESTSUM) $(GOTESTSUM_ARGS) -- -race -covermode=atomic -coverprofile=coverage.out ./...

clean: ## Remove build output and local data
	rm -rf bin .data web/node_modules coverage.out
	find web/dist -mindepth 1 ! -name .keep -delete
