# Single entry point for humans, Claude and CI. Targets are no-ops until the matching part is scaffolded.
# Per-worktree ports/names come from .worktree.env (written by scripts/task-worktree.sh).
-include .worktree.env
export

GO_PKGS ?= ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
HAS_GO  := $(wildcard go.mod)
HAS_WEB := $(wildcard web/package.json)

.PHONY: help build test test-pkg test-integration lint cover dev clean web-deps docker

# Installs web dependencies once per worktree (fresh worktrees have no node_modules).
web-deps:
ifneq ($(HAS_WEB),)
	@[ -d web/node_modules ] || npm --prefix web ci
endif

help: ## list targets
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

build: web-deps ## build web assets then Go binaries
ifneq ($(HAS_WEB),)
	npm --prefix web run build
endif
ifneq ($(HAS_GO),)
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/...
endif

test: web-deps ## unit tests: Go (race, shuffled, uncached) + web
ifneq ($(HAS_GO),)
	go test -race -shuffle=on -count=1 $(GO_PKGS)
endif
ifneq ($(HAS_WEB),)
	npm --prefix web test -- --run
endif

test-pkg: ## fast loop for one package: make test-pkg P=./internal/runner/...
	go test -race -shuffle=on -count=1 $(P)

COMPOSE_TEST := $(wildcard deploy/compose.test.yml)

test-integration: ## integration tests (//go:build integration); test stack always torn down
ifneq ($(HAS_GO),)
ifneq ($(COMPOSE_TEST),)
	docker compose -f $(COMPOSE_TEST) up -d --wait; \
	  go test -race -count=1 -tags=integration $(GO_PKGS); rc=$$?; \
	  docker compose -f $(COMPOSE_TEST) down -v --remove-orphans; exit $$rc
else
	go test -race -count=1 -tags=integration $(GO_PKGS)
endif
endif

lint: web-deps ## vet + typecheck
ifneq ($(HAS_GO),)
	go vet $(GO_PKGS)
endif
ifneq ($(HAS_WEB),)
	npm --prefix web run typecheck
endif

docker: ## build the Control Plane image (ghcr.io/tr1gl1de/kindevai:local)
	docker compose build --build-arg VERSION=$(VERSION) cp

cover: ## coverage report to coverage.out
	go test -race -count=1 -coverprofile=coverage.out $(GO_PKGS) && go tool cover -func=coverage.out | tail -1

dev: ## run control plane on this worktree's port (web: npm --prefix web run dev in another shell)
	go run ./cmd/ai-platform serve --addr 127.0.0.1:$${KINDEV_PORT:-8080} --data-dir $${KINDEV_DATA_DIR:-./data}

clean: ## remove build/test leftovers of this worktree (keeps data/ and .env)
	rm -rf bin/ coverage.* *.test *.out web/dist/app web/coverage
ifneq ($(COMPOSE_TEST),)
	docker compose -f $(COMPOSE_TEST) down -v --remove-orphans 2>/dev/null || true
endif
