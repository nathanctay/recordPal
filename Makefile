# RecordPal — local development
#
# Build and run all three components (middleware, audio, ui) on this machine.
# For Raspberry Pi cross-compilation and deployment, see Makefile.pi.
#
#   make install      # one-time: fetch all deps
#   make dev          # run the whole pipeline locally
#   make help         # list everything

SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help install build build-server build-audio build-ui \
        run-server run-ui run-audio dev dev-noaudio test fmt clean

help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- Setup ---

install: ## Install UI deps and fetch Go/Rust deps
	cd ui && npm install
	cd middleware && go mod download
	cd audio && cargo fetch

## --- Build (native) ---

build: build-server build-audio build-ui ## Build all three components natively

build-server: ## Build the Go middleware -> middleware/bin/now-playing
	cd middleware && mkdir -p bin && go build -o bin/now-playing .

build-audio: ## Build the Rust audio worker (debug)
	cd audio && cargo build

build-ui: ## Build the React UI static bundle -> ui/dist
	cd ui && npm run build

## --- Run (each in the foreground) ---

run-server: ## Run the Go middleware (needs middleware/.env with AUDD_API_KEY)
	cd middleware && go run .

run-ui: ## Run the Vite dev server for the UI
	cd ui && npm run dev

run-audio: ## Run the Rust audio worker (records from the default mic)
	cd audio && cargo run

## --- Run everything together ---

dev: ## Start middleware + ui + audio together (Ctrl-C stops all)
	@echo "Starting middleware + ui + audio — Ctrl-C to stop all"
	@trap 'kill 0' INT TERM EXIT; \
		( cd middleware && go run . ) & \
		sleep 1; \
		( cd ui && npm run dev ) & \
		( cd audio && cargo run ) & \
		wait

dev-noaudio: ## Start middleware + ui only (no mic, no AudD calls)
	@echo "Starting middleware + ui — Ctrl-C to stop all"
	@trap 'kill 0' INT TERM EXIT; \
		( cd middleware && go run . ) & \
		sleep 1; \
		( cd ui && npm run dev ) & \
		wait

## --- Quality ---

test: ## Run middleware tests
	cd middleware && go test ./...

fmt: ## Format Go and Rust code
	cd middleware && gofmt -w .
	cd audio && cargo fmt

clean: ## Remove build artifacts
	rm -rf middleware/bin ui/dist
	cd audio && cargo clean
