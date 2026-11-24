#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
project="rei-nation-test-$$"
compose=(docker compose --project-name "$project" --env-file /dev/null)
if [[ -n "$(docker compose ls --all --filter "name=^$project$" --quiet)" ]]; then
  printf 'The test project %s already exists.\n' "$project" >&2
  exit 1
fi

export REI_NATION_BIND="${REI_NATION_TEST_BIND:-127.0.0.21}"
if [[ "$REI_NATION_BIND" != 127.* ]]; then
  printf 'The test bind address must be on loopback.\n' >&2
  exit 1
fi
export REI_NATION_PUBLIC_IP="$REI_NATION_BIND"
export REI_NATION_GAMES=nascar04 REI_NATION_RELAY=always
export REI_NATION_HEALTH_PORT="${REI_NATION_TEST_HEALTH_PORT:-58088}"
export REI_NATION_PG_PORT="${REI_NATION_TEST_PG_PORT:-55488}"
export REI_NATION_IMAGE="$project:local"
export POSTGRES_PASSWORD
POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export REI_NATION_SMTP_ADDR='' REI_NATION_SMTP_FROM=''
export REI_NATION_SMTP_USER='' REI_NATION_SMTP_PASSWORD=''

cleanup() {
  local status=$?
  if (( status != 0 )); then
    "${compose[@]}" logs --no-color || true
  fi
  "${compose[@]}" down --volumes
  if docker image inspect "$REI_NATION_IMAGE" >/dev/null 2>&1; then
    docker image rm "$REI_NATION_IMAGE" >/dev/null
  fi
}
trap cleanup EXIT

"${compose[@]}" up -d --build --wait --wait-timeout 90
export REI_NATION_TEST_ADDR="$REI_NATION_BIND:10600"
export REI_NATION_TEST_BUDDY_ADDR="$REI_NATION_BIND:5000"
export REI_NATION_TEST_RELAY_ADDR="$REI_NATION_BIND"
export REI_NATION_TEST_PUBLIC_IP="$REI_NATION_PUBLIC_IP"
export REI_NATION_TEST_DATABASE_URL="postgres://eanation:$POSTGRES_PASSWORD@127.0.0.1:$REI_NATION_PG_PORT/eanation?sslmode=disable"

go test -race ./internal/games/nascar04 -count=1 -v
