.PHONY: build test frontend-install frontend frontend-dev run docker-build docker-up docker-down docker-logs

GO ?= go
NPM ?= npm
DOCKER_COMPOSE ?= docker compose

build: frontend
	mkdir -p bin
	$(GO) build -trimpath -o bin/autosms ./cmd/autosms

test: frontend
	cd frontend && $(NPM) test
	$(GO) test ./...

frontend-install:
	cd frontend && if [ -f package-lock.json ]; then $(NPM) ci --no-audit --no-fund; else $(NPM) install --no-audit --no-fund; fi

frontend: frontend-install
	cd frontend && $(NPM) run build

frontend-dev: frontend-install
	cd frontend && $(NPM) run dev

run: frontend
	$(GO) run ./cmd/autosms

docker-build:
	$(DOCKER_COMPOSE) build

docker-up:
	$(DOCKER_COMPOSE) up --build

docker-down:
	$(DOCKER_COMPOSE) down

docker-logs:
	$(DOCKER_COMPOSE) logs -f autosms
