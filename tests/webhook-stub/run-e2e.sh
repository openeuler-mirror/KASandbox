#!/usr/bin/env bash
# End-to-end stub test for the internal E2B API endpoints used by the webhook
# service. Boots throwaway Postgres + Redis containers, runs migrations, seeds
# a team and a base template, builds and starts packages/api, then runs the
# webhook-stub scenario runner against it over real HTTP.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PG_PORT="${PG_PORT:-15432}"
REDIS_PORT="${REDIS_PORT:-16379}"
API_PORT="${API_PORT:-13200}"

PG_CONTAINER="kasandbox-stub-pg-$$"
REDIS_CONTAINER="kasandbox-stub-redis-$$"
API_PID=""
API_LOG="${SCRIPT_DIR}/api.log"

TEAM_ID="11111111-1111-1111-1111-111111111111"
BASE_ENV_ID="stub-base-env-01"
ADMIN_TOKEN="test-admin-token"

cleanup() {
  if [[ -n "${API_PID}" ]] && kill -0 "${API_PID}" 2>/dev/null; then
    kill "${API_PID}" 2>/dev/null || true
    wait "${API_PID}" 2>/dev/null || true
  fi
  docker rm -f "${PG_CONTAINER}" "${REDIS_CONTAINER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Starting Postgres (${PG_CONTAINER}, port ${PG_PORT}) and Redis (${REDIS_CONTAINER}, port ${REDIS_PORT})"
docker run -d --name "${PG_CONTAINER}" \
  -p 127.0.0.1:"${PG_PORT}":5432 \
  -e POSTGRES_PASSWORD=postgres \
  postgres:16-alpine >/dev/null
docker run -d --name "${REDIS_CONTAINER}" \
  -p 127.0.0.1:"${REDIS_PORT}":6379 \
  redis:8-alpine >/dev/null

echo "==> Waiting for Postgres and Redis to become ready"
for i in $(seq 1 30); do
  if docker exec "${PG_CONTAINER}" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  if [[ "${i}" == "30" ]]; then
    echo "ERROR: Postgres did not become ready in 30s" >&2
    exit 1
  fi
  sleep 1
done
for i in $(seq 1 30); do
  if docker exec "${REDIS_CONTAINER}" redis-cli ping 2>/dev/null | grep -q PONG; then
    break
  fi
  if [[ "${i}" == "30" ]]; then
    echo "ERROR: Redis did not become ready in 30s" >&2
    exit 1
  fi
  sleep 1
done

echo "==> Running goose migrations"
cd "${REPO_ROOT}/packages/db"
GOOSE_DBSTRING="postgres://postgres:postgres@127.0.0.1:${PG_PORT}/postgres?sslmode=disable" \
  go tool goose -table _migrations -dir migrations postgres up

echo "==> Seeding team ${TEAM_ID} and base env ${BASE_ENV_ID}"
docker exec -i "${PG_CONTAINER}" psql -U postgres -d postgres -v ON_ERROR_STOP=1 <<'SQL'
DELETE FROM public.envs WHERE id = 'stub-base-env-01';
DELETE FROM public.teams WHERE id = '11111111-1111-1111-1111-111111111111';
INSERT INTO public.teams (id, name, tier, email, slug) VALUES ('11111111-1111-1111-1111-111111111111', 'Test Team', 'base_v1', 't@example.com', 'test-team');
INSERT INTO public.envs (id, team_id, public, updated_at, source) VALUES ('stub-base-env-01', '11111111-1111-1111-1111-111111111111', true, NOW(), 'template');
SQL

echo "==> Building packages/api"
cd "${REPO_ROOT}/packages/api"
go build -o bin/api .

echo "==> Starting API on port ${API_PORT} (log: ${API_LOG})"
ADMIN_TOKEN="${ADMIN_TOKEN}" \
POSTGRES_CONNECTION_STRING="postgres://postgres:postgres@127.0.0.1:${PG_PORT}/postgres?sslmode=disable" \
REDIS_URL="127.0.0.1:${REDIS_PORT}" \
LOKI_URL=unset \
NODE_ID=stub-node \
SANDBOX_ACCESS_TOKEN_HASH_SEED=stub-seed \
API_GRPC_PORT="${GRPC_PORT:-15009}" \
./bin/api --port "${API_PORT}" >"${API_LOG}" 2>&1 &
API_PID=$!

for i in $(seq 1 60); do
  if ! kill -0 "${API_PID}" 2>/dev/null; then
    echo "ERROR: API process exited early; last log lines:" >&2
    tail -n 50 "${API_LOG}" >&2 || true
    exit 1
  fi
  if (echo >/dev/tcp/127.0.0.1/"${API_PORT}") 2>/dev/null; then
    break
  fi
  if [[ "${i}" == "60" ]]; then
    echo "ERROR: API port ${API_PORT} not reachable in 60s; last log lines:" >&2
    tail -n 50 "${API_LOG}" >&2 || true
    exit 1
  fi
  sleep 1
done

echo "==> Running webhook-stub scenarios"
cd "${SCRIPT_DIR}"
GOWORK=off go run . \
  -api "http://127.0.0.1:${API_PORT}" \
  -token "${ADMIN_TOKEN}" \
  -team "${TEAM_ID}" \
  -base-env "${BASE_ENV_ID}"

echo "==> All done"
