#!/usr/bin/env bash
# Run inside the Ubuntu 22.04 daemon builder image. The host launcher mounts
# only locally built artifacts, this script, private credentials, and a
# writable diagnostics directory.
set -euo pipefail
umask 077

: "${UR_ACCEPT_DEB:?set UR_ACCEPT_DEB}"
: "${UR_ACCEPT_VERSION:?set UR_ACCEPT_VERSION}"
: "${UR_ACCEPT_REPEAT:?set UR_ACCEPT_REPEAT}"
: "${UR_ACCEPT_FIXTURE:?set UR_ACCEPT_FIXTURE}"
: "${UR_ACCEPT_TESTS:?set UR_ACCEPT_TESTS}"
: "${UR_ACCEPT_ARTIFACTS:?set UR_ACCEPT_ARTIFACTS}"

agent=/opt/urnetwork-acceptance/agent
credentials=/opt/urnetwork-acceptance/credentials
daemon=/usr/lib/urnetwork/urnetworkd
work=/opt/urnetwork-acceptance/state
socket=/opt/urnetwork-acceptance/control.sock
daemon_pid=

cleanup() {
  exit_status=$?
  if [ -n "$daemon_pid" ] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "$daemon_pid" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$daemon_pid" 2>/dev/null; then
      kill -KILL "$daemon_pid" 2>/dev/null || true
    fi
    wait "$daemon_pid" 2>/dev/null || true
  fi
  if dpkg-query -W urnetwork-daemon >/dev/null 2>&1; then
    if ! timeout 45 dpkg --purge urnetwork-daemon >"$UR_ACCEPT_ARTIFACTS/purge.log" 2>&1; then
      echo "failed to purge urnetwork-daemon" >&2
      exit_status=1
    fi
  fi
  if dpkg-query -W urnetwork-daemon >/dev/null 2>&1; then
    echo "urnetwork-daemon remained installed after purge" >&2
    exit_status=1
  fi
  exit "$exit_status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

dpkg -i "$UR_ACCEPT_DEB" >"$UR_ACCEPT_ARTIFACTS/install.log" 2>&1
installed_version="$(dpkg-query -W -f='${Version}' urnetwork-daemon)"
if [ "$installed_version" != "$UR_ACCEPT_VERSION" ]; then
  echo "installed package version $installed_version does not match $UR_ACCEPT_VERSION" >&2
  exit 1
fi

version_line="$($daemon --version)"
case "$version_line" in
  "urnetworkd $UR_ACCEPT_VERSION ("*) ;;
  *) echo "unexpected daemon version: $version_line" >&2; exit 1 ;;
esac
sdk_version="${version_line##*sdk }"
sdk_version="${sdk_version%)}"
[ -n "$sdk_version" ] || { echo "daemon did not report its SDK version" >&2; exit 1; }

mkdir -p "$work" "$UR_ACCEPT_ARTIFACTS"
export URNETWORK_CONTROL_SOCKET="$socket"
export URNETWORK_STATE_DIR="$work/daemon"
export URNETWORK_LOG_DIR="$UR_ACCEPT_ARTIFACTS/sdk-logs"
"$daemon" --foreground >"$UR_ACCEPT_ARTIFACTS/daemon.log" 2>&1 &
daemon_pid=$!
for _ in $(seq 1 150); do
  [ -S "$socket" ] && break
  kill -0 "$daemon_pid" 2>/dev/null || {
    echo "urnetworkd exited before its control socket became ready" >&2
    tail -80 "$UR_ACCEPT_ARTIFACTS/daemon.log" >&2 || true
    exit 1
  }
  sleep 0.2
done
[ -S "$socket" ] || { echo "timed out waiting for $socket" >&2; exit 1; }

set +e
URNETWORK_CONTROL_SOCKET="$socket" "$agent" \
  -credentials "$credentials" \
  -tests "$UR_ACCEPT_TESTS" \
  -fixture "$UR_ACCEPT_FIXTURE" \
  -active-client "$UR_ACCEPT_ARTIFACTS/active-client-id" \
  -state-dir "$work/agent" \
  -sdk-version "$sdk_version" \
  -app-version "$UR_ACCEPT_VERSION" \
  -service-version "$UR_ACCEPT_VERSION" \
  -repeat "$UR_ACCEPT_REPEAT" \
  >"$UR_ACCEPT_ARTIFACTS/result.json" \
  2> >(tee "$UR_ACCEPT_ARTIFACTS/agent.log" >&2)
agent_status=$?
set -e

if [ "$agent_status" -ne 0 ]; then
  tail -120 "$UR_ACCEPT_ARTIFACTS/daemon.log" >&2 || true
  exit "$agent_status"
fi
grep -q '"ok":true' "$UR_ACCEPT_ARTIFACTS/result.json" || {
  echo "acceptance agent did not emit a successful result" >&2
  exit 1
}
if ip link show urnet0 >/dev/null 2>&1; then
  echo "urnet0 remained after disconnect" >&2
  exit 1
fi
