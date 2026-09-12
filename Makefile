GO ?= go
BUN ?= bun
MISE ?= mise
OPENCODE_BIN ?= opencode2
PI_BIN ?= pi

WEB_DIR := web
BIN_DIR := bin
BINARY := $(BIN_DIR)/samik-bot

.DEFAULT_GOAL := help

.PHONY: help setup install opencode-check pi-check fmt-check vet test web-build build check run run-local web-dev clean

help:
	@printf '%s\n' 'Targets:' \
		'  make setup       Install repository-pinned Go/Bun and dependencies' \
		'  make opencode-check  Verify the installed OpenCode 2 beta CLI' \
		'  make pi-check    Verify the installed pi coding agent CLI' \
		'  make check       Verify formatting, Go code, frontend, and binary build' \
		'  make build       Build the embedded dashboard and bin/samik-bot' \
		'  make run         Run the service from source with injected environment' \
		'  make run-local   Load .env for local development, then run the service' \
		'  make web-dev     Start Vite on :5173 with API proxying to :8080' \
		'  make clean       Remove local binary output'

setup:
	$(MISE) install
	$(MISE) exec -- $(MAKE) install

install:
	$(GO) mod download
	$(BUN) install --cwd $(WEB_DIR) --frozen-lockfile

opencode-check:
	@command -v "$(OPENCODE_BIN)" >/dev/null 2>&1 || { \
		printf '%s\n' "OpenCode binary '$(OPENCODE_BIN)' was not found; install the supported OpenCode 2 beta CLI or set OPENCODE_BIN."; \
		exit 1; \
	}
	@"$(OPENCODE_BIN)" --version
	@"$(OPENCODE_BIN)" run --help >/dev/null

pi-check:
	@command -v "$(PI_BIN)" >/dev/null 2>&1 || { \
		printf '%s\n' "pi binary '$(PI_BIN)' was not found; install the pi coding agent CLI or set PI_BIN."; \
		exit 1; \
	}
	@"$(PI_BIN)" --version
	@"$(PI_BIN)" --help >/dev/null

fmt-check:
	@unformatted="$$(gofmt -l $$(find cmd internal web -type f -name '*.go'))"; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n%s\n' 'Go files need formatting:' "$$unformatted"; \
		exit 1; \
	fi

vet: web-build
	$(GO) vet ./...

test: web-build
	$(GO) test ./...

web-build:
	$(BUN) run --cwd $(WEB_DIR) build

build: web-build
	mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -buildvcs=false -o $(BINARY) ./cmd/samik-bot

check: fmt-check vet test build

run: web-build
	$(GO) run ./cmd/samik-bot

run-local: web-build
	@test -f .env || { printf '%s\n' '.env not found; copy .env.example and fill required values.'; exit 1; }
	@set -a; . ./.env; set +a; $(GO) run ./cmd/samik-bot

web-dev:
	$(BUN) run --cwd $(WEB_DIR) dev --host 0.0.0.0

clean:
	rm -rf $(BIN_DIR)
