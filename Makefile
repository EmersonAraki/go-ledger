# sumzero -- development tasks.
#
# `make check` is what CI runs and what you should run before pushing.

DB_URL      ?= postgres://sumzero:sumzero@localhost:5432/sumzero?sslmode=disable
TEST_DB_URL ?= postgres://sumzero:sumzero@localhost:5432/sumzero_test?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start Postgres and Redis
	docker compose up -d

.PHONY: down
down: ## Stop services
	docker compose down

.PHONY: migrate
migrate: ## Apply pending migrations
	DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the newest migration
	DATABASE_URL="$(DB_URL)" go run ./cmd/migrate down 1

.PHONY: version
version: ## Print the current schema version
	DATABASE_URL="$(DB_URL)" go run ./cmd/migrate version

.PHONY: run
run: ## Run the API server
	DATABASE_URL="$(DB_URL)" go run ./cmd/api

.PHONY: build
build: ## Build binaries into ./bin
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate

.PHONY: test
test: ## Run all tests with the race detector
	TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race -count=1 ./...

.PHONY: test-short
test-short: ## Run tests that need no database
	go test -race -count=1 ./...

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w .

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: check
check: ## Everything CI runs: format check, vet, lint, tests
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run 'make fmt'"; exit 1)
	go vet ./...
	golangci-lint run
	TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race -count=1 ./...

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/
