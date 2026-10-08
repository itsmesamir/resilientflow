COMPOSE := docker compose -f deploy/docker-compose/docker-compose.yml
PGURL   ?= postgres://resilientflow:resilientflow@localhost:5433/resilientflow?sslmode=disable

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start Postgres and wait until it accepts connections
	$(COMPOSE) up -d --wait

.PHONY: down
down: ## Stop containers, keep the data volume
	$(COMPOSE) down

.PHONY: nuke
nuke: ## Stop containers and delete the data volume
	$(COMPOSE) down -v

.PHONY: psql
psql: ## Open a psql shell against the dev database
	psql "$(PGURL)"

.PHONY: build
build: ## Compile every binary into ./bin
	go build -o bin/ ./cmd/...

.PHONY: demo
demo: ## Run the lost-response scenario and print what the outside world holds
	go run ./cmd/demo

.PHONY: test
test: ## Unit tests only (no database required)
	go test ./... -count=1

.PHONY: test-all
test-all: ## Tests with the race detector and coverage
	go test ./... -count=1 -race -cover

.PHONY: test-integration
test-integration: ## Tests that need a live Postgres
	PGURL="$(PGURL)" go test ./... -count=1 -tags=integration

.PHONY: fmt
fmt: ## Format and vet
	go fmt ./...
	go vet ./...
