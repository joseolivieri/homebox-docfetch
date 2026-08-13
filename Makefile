# Dev workflow. `make dev` is the tight loop: searxng in docker, docfetch
# native (go run) against config.dev.yaml. See README §Dev quickstart.

-include .env
export

COMPOSE     := docker compose -f docker-compose.yml -f docker-compose.dev.yml
DEV_CONFIG  := config.dev.yaml

.PHONY: help dev searxng dev-docker dev-docker-down log reset build test check env

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

env: ## create .env from the template if missing
	@test -f .env || { cp .env.example .env; echo "created .env — fill in DOCFETCH_HOMEBOX_URL / HOMEBOX_TOKEN / OPENROUTER_API_KEY"; }

dev: env searxng ## tight loop: searxng container + native `go run serve`
	@mkdir -p data
	go run ./cmd/docfetch serve --config $(DEV_CONFIG)

searxng: ## start only the searxng container (loopback :8080)
	$(COMPOSE) up -d searxng

dev-docker: env ## full stack in docker (builds the image)
	@mkdir -p data/docker
	@sed -e 's#http://localhost:8080#http://searxng:8080#' \
	     -e 's#\./data/dev\.db#/data/dev.db#' \
	     -e 's#"127\.0\.0\.1:8099"#":8099"#' \
	     $(DEV_CONFIG) > data/config.docker.yaml
	$(COMPOSE) up --build

dev-docker-down: ## stop the dev stack
	$(COMPOSE) down

log: ## recent activity events from the dev store
	go run ./cmd/docfetch log --config $(DEV_CONFIG) -n 50

once: ## single scan pass against the dev config
	go run ./cmd/docfetch once --config $(DEV_CONFIG)

probe: ## smoke-test the Homebox client (creates + deletes a temp entity)
	go run ./cmd/docfetch probe --config $(DEV_CONFIG)

reset: ## wipe dev state (sqlite store) — collection reset stance, D26
	rm -rf data
	@echo "dev state wiped (Homebox items are yours to clear)"

build: ## build the binary
	go build ./...

test: ## run the test suite
	go test ./...

check: build test ## build + test
