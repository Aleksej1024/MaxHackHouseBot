#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> go vet"
go vet ./...

echo "==> golangci-lint"
GOLANGCI_LINT="${GOLANGCI_LINT:-go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2}"
$GOLANGCI_LINT run ./...

echo "==> go test -race ./..."
go test -race ./...

echo "==> интеграционные тесты (Postgres и Redis в testcontainers)"
go test -race -count=1 -tags=integration ./...

echo "==> проверка миграций: все up / все down / снова up на чистой БД"
MIGRATE_IMAGE="${MIGRATE_IMAGE:-migrate/migrate:v4.18.3}"
PG_CONTAINER="maxhouse-ci-postgres-$$"
cleanup() { docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --name "$PG_CONTAINER" \
    -e POSTGRES_USER=bot -e POSTGRES_PASSWORD=bot -e POSTGRES_DB=bot \
    postgres:16-alpine >/dev/null

for i in $(seq 1 30); do
    if docker exec "$PG_CONTAINER" pg_isready -U bot -d bot >/dev/null 2>&1; then
        break
    fi
    if [ "$i" = "30" ]; then
        echo "Postgres не поднялся за отведённое время" >&2
        exit 1
    fi
    sleep 1
done

MIGRATION_DSN="pgx5://bot:bot@127.0.0.1:5432/bot?sslmode=disable"
run_migrate() {
    docker run --rm \
        --network "container:${PG_CONTAINER}" \
        -v "$(pwd)/migrations:/migrations:ro" \
        "$MIGRATE_IMAGE" -path=/migrations -database="$MIGRATION_DSN" "$@"
}
run_migrate up
run_migrate down -all
run_migrate up

echo "==> docker build"
docker build -t maxhouse-bot:ci .

echo "==> CI OK"
