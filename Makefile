VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web run dev test clean

build: ## Build the pitlane binary (with the embedded UI)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/pitlane ./cmd/pitlane

web: ## Build the web UI into web/dist
	cd web && pnpm install --frozen-lockfile && pnpm build

run: web build ## Run locally against the local Docker daemon (no login)
	./bin/pitlane --data ./.data --insecure-no-auth --listen 127.0.0.1:8080

dev: ## Run the UI dev server (proxying to a Pitlane on :8080)
	cd web && pnpm dev

test:
	go vet ./... && go test ./...

clean:
	rm -rf bin .data web/node_modules
	find web/dist -mindepth 1 ! -name .keep -delete
