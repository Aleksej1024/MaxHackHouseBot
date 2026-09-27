# Сборка и запуск. Список целей: make help.

# Линтер закреплённой версии собирается текущим Go (системный golangci-lint,
# собранный старым Go, отказывается проверять проект на Go 1.26).
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

COMPOSE      := docker compose
COMPOSE_PROD := docker compose -f docker-compose.yml -f docker-compose.prod.yml
WEBHOOK_CERTS_DIR ?= ./deploy/certs

# Переменные из .env нужны локальному запуску и миграциям.
ENV_FILE ?= .env
LOAD_ENV := set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a;

.PHONY: help build run local deps-up deps-down up down logs ps check-env test-e2e \
	prod-up prod-down prod-logs check-certs \
	migrate-up migrate-down test test-race test-integration lint mocks docker-build ci

help: ## показать цели
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

# --- Локальный запуск: бот на хосте, Postgres и Redis в Docker ---

build: ## собрать бинарник bin/bot
	CGO_ENABLED=0 go build -o bin/bot ./cmd/bot

deps-up: ## поднять Postgres и Redis в Docker (порты на localhost)
	$(COMPOSE) up -d --wait postgres redis

deps-down: ## остановить всё окружение (данные в volume сохраняются)
	$(COMPOSE) down

run: check-env ## запустить бота на хосте с переменными из .env
	@$(LOAD_ENV) go run ./cmd/bot

local: deps-up migrate-up run ## deps-up + миграции + run

# --- Всё в Docker (dev) ---

up: check-env ## собрать и поднять бот, Postgres, Redis, миграции в Docker
	$(COMPOSE) up -d --build

down: ## остановить dev-окружение
	$(COMPOSE) down

logs: ## логи бота (dev)
	$(COMPOSE) logs -f bot

ps: ## состояние контейнеров
	$(COMPOSE) ps

# --- Prod ---

prod-up: check-env check-certs ## собрать и поднять prod: webhook через nginx на 443, логи в файл
	$(COMPOSE_PROD) up -d --build

prod-down: ## остановить prod
	$(COMPOSE_PROD) down

prod-logs: ## логи nginx и контейнера бота (сами логи бота — в volume botlogs)
	$(COMPOSE_PROD) logs -f nginx bot

check-certs:
	@for f in fullchain.pem privkey.pem; do \
		[ -f "$(WEBHOOK_CERTS_DIR)/$$f" ] || { echo "нет $(WEBHOOK_CERTS_DIR)/$$f — положите сертификат и ключ (README, «Продакшен»)"; exit 1; }; \
	done

# --- Миграции ---

migrate-up: ## применить миграции (Postgres из docker compose)
	$(COMPOSE) run --rm migrate

migrate-down: ## откатить последнюю миграцию
	@$(LOAD_ENV) $(COMPOSE) run --rm migrate -path=/migrations \
		-database="pgx5://$${DB_USER:-bot}:$${DB_PASSWORD:-bot}@postgres:5432/$${DB_NAME:-bot}?sslmode=disable" down 1

check-env:
	@[ -f $(ENV_FILE) ] || { echo "нет $(ENV_FILE): скопируйте .env.example в .env и задайте MAX_BOT_TOKEN"; exit 1; }

# --- Проверки ---

test: ## unit-тесты
	go test ./...

test-e2e: ## сквозные сценарии (internal/e2e), без Docker и сети
	go test -race -count=1 -v ./internal/e2e/

test-race: ## unit-тесты с детектором гонок
	go test -race ./...

test-integration: ## unit + интеграционные тесты на реальных Postgres и Redis (нужен Docker)
	go test -race -count=1 -tags=integration ./...

lint: ## golangci-lint (конфиг .golangci.yml, включая интеграционные тесты)
	$(GOLANGCI_LINT) run ./...


docker-build: ## собрать Docker-образ
	docker build -t maxhouse-bot .

ci: ## все проверки CI (scripts/ci.sh)
	./scripts/ci.sh
