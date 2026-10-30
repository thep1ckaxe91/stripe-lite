#!/usr/bin/env bash
set -euo pipefail

# A supplied URL must point to a disposable database with the Payments migrations applied.
if [[ -n "${PAYMENTS_TEST_DATABASE_URL:-}" ]]; then
    exec "$@"
fi
command -v docker >/dev/null || { echo 'Docker is required for the PostgreSQL tests.' >&2; exit 1; }
TEST_DIR=$(mktemp -d)
CONTAINER=''
cleanup() {
    if [[ -n "$CONTAINER" ]]; then docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; fi
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT
export POSTGRES_PASSWORD
POSTGRES_PASSWORD=$(openssl rand -hex 24)
APP_PASSWORD=$(openssl rand -hex 24)
CONTAINER=$(docker run -d --rm --label stripe-lite.test=payments \
    --tmpfs /var/lib/postgresql/data -e POSTGRES_PASSWORD \
    -p 127.0.0.1::5432 \
    -v "$PWD/database/migrations/002_payments.sql:/docker-entrypoint-initdb.d/002_payments.sql:ro" \
    -v "$PWD/database/migrations/003_payment_requests.sql:/docker-entrypoint-initdb.d/003_payment_requests.sql:ro" \
    postgres:16-alpine)
READY=0
for ((i=0;i<100;i++)); do
    if docker exec "$CONTAINER" psql -U postgres -tAc "SELECT to_regclass('public.payment_requests') IS NOT NULL" 2>/dev/null | tr -d '[:space:]' | grep -qx t; then
        # Initdb uses a temporary socket-only server. Wait for its TCP replacement.
        if docker exec "$CONTAINER" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then READY=1; break; fi
    fi
    sleep 0.2
done
[[ "$READY" == 1 ]] || { docker logs "$CONTAINER"; exit 1; }
docker exec -i "$CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 >/dev/null <<SQL
CREATE ROLE stripe_lite_test LOGIN PASSWORD '$APP_PASSWORD';
GRANT stripe_lite_payments_app TO stripe_lite_test;
SQL
TEST_PORT=$(docker port "$CONTAINER" 5432/tcp | awk -F: '{print $NF}')
export PAYMENTS_TEST_DATABASE_URL="postgres://stripe_lite_test:$APP_PASSWORD@127.0.0.1:$TEST_PORT/postgres?sslmode=disable"
export PAYMENTS_TEST_ADMIN_URL="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:$TEST_PORT/postgres?sslmode=disable"
"$@"
