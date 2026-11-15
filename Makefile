VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help build web run dev test clean

help: ## List available Makefile targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "%-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

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

clean: ## Remove build output and local data
	rm -rf bin .data web/node_modules
	find web/dist -mindepth 1 ! -name .keep -delete
