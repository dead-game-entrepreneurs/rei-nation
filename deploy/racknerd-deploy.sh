#!/usr/bin/env bash
set -euo pipefail

root=/home/logan/rei-nation
command=${SSH_ORIGINAL_COMMAND:-}
revision=${command#deploy }
if [[ $command != "deploy $revision" || ! $revision =~ ^[0-9a-f]{40}$ ]]; then
  echo 'Unsupported deployment command' >&2
  exit 1
fi

exec 9>"$root/deploy.lock"
flock 9
archive=$(mktemp "$root/.archive.XXXXXX")
stage=$(mktemp -d "$root/.release.XXXXXX")
trap 'rm -f "$archive"; rm -rf "$stage"' EXIT
cat > "$archive"
if tar -tf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
  echo 'Unsafe archive path' >&2
  exit 1
fi
tar -xf "$archive" -C "$stage" --no-same-owner --no-same-permissions
test -f "$stage/Dockerfile" && test -f "$stage/compose.yaml"

release="$root/releases/$revision"
install -d -m 0750 "$root/releases"
if [[ ! -d $release ]]; then
  mv "$stage" "$release"
  stage=$(mktemp -d "$root/.release.XXXXXX")
fi

candidate="rei-nation:$revision"
docker build -t "$candidate" "$release"
if current=$(docker inspect --format '{{.Image}}' rei-nation-rei-nation-1 2>/dev/null); then
  docker tag "$current" rei-nation:rollback
fi
docker tag "$candidate" rei-nation:production
compose_backup=$(mktemp "$root/.compose.XXXXXX")
cp "$root/compose.yaml" "$compose_backup"
install -m 0644 "$release/compose.yaml" "$root/compose.yaml"

cd "$root"
if ! REI_NATION_IMAGE=rei-nation:production docker compose --env-file .env up -d --no-build --wait --wait-timeout 90; then
  install -m 0644 "$compose_backup" "$root/compose.yaml"
  if docker image inspect rei-nation:rollback >/dev/null 2>&1; then
    docker tag rei-nation:rollback rei-nation:production
    REI_NATION_IMAGE=rei-nation:production docker compose --env-file .env up -d --no-build --wait --wait-timeout 90
  fi
  exit 1
fi
ln -sfn "$release" "$root/current"
rm -f "$compose_backup"
