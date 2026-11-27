#!/usr/bin/env bash
set -euo pipefail

command=${SSH_ORIGINAL_COMMAND:-}
digest=${command#deploy-relay }
if [[ $command != "deploy-relay $digest" || ! $digest =~ ^[0-9a-f]{64}$ ]]; then
  echo 'Unsupported deployment command' >&2
  exit 1
fi

exec 9>/tmp/rei-nation-relay-deploy.lock
flock 9
archive=$(mktemp /tmp/rei-nation-relay.XXXXXX.tar)
stage=$(mktemp -d /tmp/rei-nation-relay.XXXXXX)
rollback=false
cleanup() {
  status=$?
  trap - EXIT
  if [[ $rollback == true ]]; then
    set +e
    sudo -n install -o root -g root -m 0755 /opt/tsfp-relay/tsfp-relay.go-previous /usr/local/bin/tsfp-relay
    sudo -n install -o root -g root -m 0644 /opt/tsfp-relay/tsfp-relay.service.go-previous /etc/systemd/system/tsfp-relay.service
    sudo -n systemctl daemon-reload
    sudo -n systemctl restart tsfp-relay.service
  fi
  rm -f "$archive"
  rm -rf "$stage"
  exit "$status"
}
trap cleanup EXIT
cat > "$archive"
if [[ $(sha256sum "$archive" | cut -d' ' -f1) != "$digest" ]]; then
  echo 'Relay package digest mismatch' >&2
  exit 1
fi
entries=$(tar -tf "$archive")
if [[ $entries != $'tsfp-relay\ntsfp-relay.service' ]]; then
  echo 'Unexpected relay package contents' >&2
  exit 1
fi
tar -xf "$archive" -C "$stage" --no-same-owner --no-same-permissions
"$stage/tsfp-relay" -h >/dev/null

sudo -n cp /usr/local/bin/tsfp-relay /opt/tsfp-relay/tsfp-relay.go-previous
sudo -n cp /etc/systemd/system/tsfp-relay.service /opt/tsfp-relay/tsfp-relay.service.go-previous
rollback=true
sudo -n install -o root -g root -m 0755 "$stage/tsfp-relay" /usr/local/bin/tsfp-relay
sudo -n install -o root -g root -m 0644 "$stage/tsfp-relay.service" /etc/systemd/system/tsfp-relay.service
sudo -n systemctl daemon-reload
cursor=$(sudo -n journalctl -u tsfp-relay.service -n 0 --show-cursor --no-pager | sed -n 's/^-- cursor: //p')
test -n "$cursor"
connected=false
if sudo -n systemctl restart tsfp-relay.service; then
  candidate_pid=$(sudo -n systemctl show --property MainPID --value tsfp-relay.service)
  [[ $candidate_pid =~ ^[1-9][0-9]*$ ]]
  for _ in {1..15}; do
    if sudo -n systemctl is-active --quiet tsfp-relay.service &&
      sudo -n journalctl -u tsfp-relay.service --after-cursor "$cursor" "_PID=$candidate_pid" --no-pager -o cat |
        grep -F '"msg":"relay control connected"' >/dev/null; then
      connected=true
      break
    fi
    sleep 1
  done
fi
if [[ $connected != true ]]; then
  exit 1
fi
rollback=false
