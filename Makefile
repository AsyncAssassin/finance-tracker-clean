.DEFAULT_GOAL := help
GO ?= go

# Настройки базы для интеграционных тестов берутся из .env, если он есть.
-include .env
POSTGRES_USER ?= finance
POSTGRES_PASSWORD ?=
POSTGRES_PORT ?= 5432
REDIS_PORT ?= 6379

# Local tools use a separate stack; the public stack keeps its backend isolated.
DEV_COMPOSE = docker compose -p finance-tracker-clean-dev -f compose.yaml -f compose.dev.yaml

# Do not pass administrator, sibling-service or migration passwords to child processes.
SERVICE_ENV = env -u POSTGRES_PASSWORD -u AUTH_DB_PASSWORD -u LEDGER_DB_PASSWORD -u AUTH_MIGRATION_PASSWORD -u LEDGER_MIGRATION_PASSWORD

help: ## Список целей
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

# Ledger и auth стартуют в фоне, каждый на адресе, по которому его ищет gateway;
# gateway запускается, когда оба отвечают на health-проверку. Если сервис упал
# при старте (порт занят, база недоступна, нет JWT_SECRET), цель завершается с
# ошибкой, а не поднимает gateway, отвечающий 502.
run: build ## Запустить ledger (:9090), auth (:9091) и gateway (:8080) локально, Ctrl+C останавливает все; переменные из .env
	@set -a; [ -f .env ] && . ./.env; set +a; \
	ledger_addr=$${LEDGER_GRPC_ADDR:-localhost:9090}; auth_addr=$${AUTH_GRPC_ADDR:-localhost:9091}; \
	PGUSER=ledger_app PGPASSWORD="$${LEDGER_DB_PASSWORD:-}" GRPC_ADDR=$$ledger_addr $(SERVICE_ENV) -u JWT_SECRET -u REDIS_PASSWORD -u AUTH_REGISTRATION_EMAILS ./bin/ledger & ledger=$$!; \
	PGUSER=auth_app PGPASSWORD="$${AUTH_DB_PASSWORD:-}" GRPC_ADDR=$$auth_addr $(SERVICE_ENV) -u REDIS_PASSWORD ./bin/auth & auth=$$!; \
	trap 'kill $$ledger $$auth 2>/dev/null; wait $$ledger $$auth' EXIT INT TERM; \
	wait_healthy() { \
		until GRPC_ADDR=$$3 ./bin/$$1 -healthcheck 2>/dev/null; do \
			kill -0 $$2 2>/dev/null || { echo "make run: $$1 не запустился, причина указана в логе выше" >&2; exit 1; }; \
			sleep 0.5; \
		done; \
	}; \
	wait_healthy ledger $$ledger $$ledger_addr; \
	wait_healthy auth $$auth $$auth_addr; \
	$(SERVICE_ENV) -u REDIS_PASSWORD -u AUTH_REGISTRATION_EMAILS -u PGUSER -u PGPASSWORD -u DATABASE_URL ./bin/gateway

migrate: build ## Применить миграции локальной базы с отдельными ролями
	@set -a; [ -f .env ] && . ./.env; set +a; \
	PGUSER=auth_migrator PGPASSWORD="$${AUTH_MIGRATION_PASSWORD:-}" $(SERVICE_ENV) -u JWT_SECRET -u REDIS_PASSWORD -u AUTH_REGISTRATION_EMAILS ./bin/migrate -service auth && \
	PGUSER=ledger_migrator PGPASSWORD="$${LEDGER_MIGRATION_PASSWORD:-}" $(SERVICE_ENV) -u JWT_SECRET -u REDIS_PASSWORD -u AUTH_REGISTRATION_EMAILS ./bin/migrate -service ledger

db: ## Поднять PostgreSQL и Redis из compose (для make run и test-integration)
	$(DEV_COMPOSE) up -d --build --wait db redis

down-dev: ## Остановить базу и кеш для локальной разработки, сохранив данные
	$(DEV_COMPOSE) down

build: ## Собрать бинарники в bin/
	$(GO) build -o bin/ ./cmd/...

test: ## Юнит-тесты
	$(GO) test ./...

race: ## Тесты с детектором гонок
	$(GO) test -race ./...

# Интеграционные тесты работают в отдельной базе finance_test (они делают
# TRUNCATE) и в базе Redis 15 (FLUSHDB), чтобы не трогать данные стека.
# Учётные данные идут через PGUSER/PGPASSWORD, а не в URL. Пароль не нужно экранировать.
TEST_DATABASE_URL ?= postgres://localhost:$(POSTGRES_PORT)/finance_test?sslmode=disable
TEST_REDIS_URL ?= redis://localhost:$(REDIS_PORT)/15
TEST_DB_ENV = PGUSER=$(POSTGRES_USER) PGPASSWORD=$(POSTGRES_PASSWORD) TEST_DATABASE_URL=$(TEST_DATABASE_URL) TEST_REDIS_URL=$(TEST_REDIS_URL)
test-db: db ## Создать базу finance_test для интеграционных тестов, если её нет
	@$(DEV_COMPOSE) exec -T db sh -c 'until pg_isready -q -U "$$POSTGRES_USER"; do sleep 1; done; \
		psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -tAc "SELECT 1 FROM pg_database WHERE datname = '"'"'finance_test'"'"'" | grep -q 1 \
		|| createdb -U "$$POSTGRES_USER" finance_test'

INTEGRATION_PKGS = ./internal/ledger/repo/postgres/... ./internal/auth/repo/postgres/... ./internal/pkg/pgkit/... ./internal/ledger/cache/...
test-integration: test-db ## Интеграционные тесты на PostgreSQL и Redis: репозитории, pgkit, кеш отчётов
	@$(TEST_DB_ENV) $(GO) test -race -count=1 $(INTEGRATION_PKGS)

E2E_BASE_URL ?= http://localhost:8080
e2e: ## Сквозной сценарий по HTTP против запущенного стека (make up): API и скрипт Google Таблицы
	E2E_BASE_URL=$(E2E_BASE_URL) $(GO) test -tags e2e -count=1 -v ./e2e/
	@if command -v node >/dev/null; then E2E_BASE_URL=$(E2E_BASE_URL) node --test sheets/test/live.test.js; \
	else echo "node не установлен: проверка скрипта таблицы пропущена"; fi

sheets-test: ## Тесты скрипта Google Таблицы (sheets/Code.gs) без Google; нужен Node 22+
	node --test 'sheets/test/*.test.js'

cover: ## Покрытие → cover.out без сгенерированного proto-кода (только юнит-тесты)
	$(GO) test -race -covermode=atomic -coverpkg=./... -coverprofile=cover.out ./...
	@grep -v '\.pb\.go:' cover.out > cover.out.tmp && mv cover.out.tmp cover.out
	$(GO) tool cover -func=cover.out | tail -1

PROTO_OUT ?= api/proto
proto: ## Сгенерировать Go-код из api/proto (нужны protoc, protoc-gen-go, protoc-gen-go-grpc)
	protoc --proto_path=api/proto \
		--go_out="$(PROTO_OUT)" --go_opt=paths=source_relative \
		--go-grpc_out="$(PROTO_OUT)" --go-grpc_opt=paths=source_relative \
		$$(find api/proto -name '*.proto')

cover-all: test-db ## Покрытие вместе с интеграционными тестами, включая PostgreSQL и Redis
	@$(TEST_DB_ENV) $(MAKE) --no-print-directory cover

STATICCHECK = honnef.co/go/tools/cmd/staticcheck@v0.8.1
GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.1.4
check: lint ## gofmt, vet, staticcheck, tidy и актуальность сгенерированного proto
	$(GO) run $(STATICCHECK) ./...
	$(GO) mod tidy -diff
	@set -eu; \
	if ! command -v protoc >/dev/null; then echo "protoc не установлен: проверка proto пропущена"; exit 0; fi; \
	proto_tmp=$$(mktemp -d); trap 'rm -rf "$$proto_tmp"' EXIT HUP INT TERM; \
	$(MAKE) --no-print-directory proto PROTO_OUT="$$proto_tmp" >/dev/null; \
	(cd api/proto && find . -name '*.pb.go' | sort) > "$$proto_tmp/expected"; \
	(cd "$$proto_tmp" && find . -name '*.pb.go' | sort) > "$$proto_tmp/generated"; \
	diff -u "$$proto_tmp/expected" "$$proto_tmp/generated"; \
	while IFS= read -r file; do \
		diff -u -I '^//[[:space:]]*protoc' "api/proto/$$file" "$$proto_tmp/$$file"; \
	done < "$$proto_tmp/generated"

vuln: ## Известные уязвимости в зависимостях и stdlib (govulncheck)
	$(GO) run $(GOVULNCHECK) ./...

vuln-check: vuln ## Проверка уязвимостей без исключений, как в CI

lint: ## gofmt + go vet (и для e2e)
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt: не отформатированы:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/

up: ## Поднять стек в Docker Compose и дождаться готовности всех сервисов
	docker compose up --build -d --wait

down: ## Остановить стек (данные в volume сохраняются)
	docker compose down

logs: ## Логи стека
	docker compose logs -f

.PHONY: help run migrate db down-dev test-db build test race test-integration e2e sheets-test cover cover-all proto check vuln vuln-check lint up down logs
