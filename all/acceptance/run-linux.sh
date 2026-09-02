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
: "${UR_ACCEPT_PEER_PROVIDER_CLIENT:?set UR_ACCEPT_PEER_PROVIDER_CLIENT}"

agent=/opt/urnetwork-acceptance/agent
credentials=/opt/urnetwork-acceptance/credentials
daemon=/usr/lib/urnetwork/urnetworkd
work=/opt/urnetwork-acceptance/state
socket=/opt/urnetwork-acceptance/control.sock
daemon_pid=
daemon_cgroup=
agent_pid=
teardown_watchdog_pid=

watch_teardown_stall() {
  while kill -0 "$agent_pid" 2>/dev/null; do
    if grep -q '\[peerconn\]teardown stalled' "$UR_ACCEPT_ARTIFACTS/daemon.log" 2>/dev/null; then
      echo "acceptance: WebRTC teardown stalled; capturing Go stacks and failing" >&2
      : >"$UR_ACCEPT_ARTIFACTS/teardown-stall.detected"
      for stalled_pid in "$daemon_pid" "$agent_pid"; do
        if kill -0 "$stalled_pid" 2>/dev/null; then
          kill -QUIT "$stalled_pid" 2>/dev/null || true
        fi
      done
      return
    fi
    sleep 0.2
  done
}

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
  if [ -n "$daemon_cgroup" ] && [ -d "$daemon_cgroup" ]; then
    if ! rmdir "$daemon_cgroup"; then
      echo "failed to remove the daemon acceptance cgroup $daemon_cgroup" >&2
      exit_status=1
    fi
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

# The cgroup-BPF socket marker applies to every process below the cgroup where
# urnetworkd attaches it. Running the daemon and the acceptance agent in the
# container's shared docker/<id> cgroup would therefore mark the agent too: its
# public probe would bypass the tunnel and the test would measure its own test
# harness defect. Move only the daemon into a child before exec, so every
# socket it and its ip/nft helpers create is marked while the agent remains the
# unmarked control whose traffic must traverse urnet0.
container_cgroup="$(sed -n 's/^0::\///p' /proc/self/cgroup)"
[ -n "$container_cgroup" ] || {
  echo "the Linux acceptance container has no named cgroup-v2 path" >&2
  exit 1
}
daemon_cgroup="/sys/fs/cgroup/$container_cgroup/urnetworkd-acceptance-$$"
mkdir "$daemon_cgroup"
(
  printf '%s\n' "$BASHPID" >"$daemon_cgroup/cgroup.procs"
  exec "$daemon" --foreground
) >"$UR_ACCEPT_ARTIFACTS/daemon.log" 2>&1 &
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

daemon_cgroup_path="$(sed -n 's/^0::\///p' "/proc/$daemon_pid/cgroup")"
expected_daemon_cgroup="${daemon_cgroup#/sys/fs/cgroup/}"
if [ "$daemon_cgroup_path" != "$expected_daemon_cgroup" ] || \
   [ "$daemon_cgroup_path" = "$container_cgroup" ]; then
  echo "urnetworkd cgroup isolation failed: daemon=$daemon_cgroup_path container=$container_cgroup" >&2
  exit 1
fi
echo "urnetworkd isolated in cgroup 0::/$daemon_cgroup_path"

set +e
URNETWORK_CONTROL_SOCKET="$socket" "$agent" \
  -credentials "$credentials" \
  -tests "$UR_ACCEPT_TESTS" \
  -fixture "$UR_ACCEPT_FIXTURE" \
  -active-client "$UR_ACCEPT_ARTIFACTS/active-client-id" \
  -peer-provider-client "$UR_ACCEPT_PEER_PROVIDER_CLIENT" \
  -state-dir "$work/agent" \
  -sdk-version "$sdk_version" \
  -app-version "$UR_ACCEPT_VERSION" \
  -service-version "$UR_ACCEPT_VERSION" \
  -repeat "$UR_ACCEPT_REPEAT" \
  >"$UR_ACCEPT_ARTIFACTS/result.json" \
  2> >(tee "$UR_ACCEPT_ARTIFACTS/agent.log" >&2) &
agent_pid=$!
watch_teardown_stall &
teardown_watchdog_pid=$!
wait "$agent_pid"
agent_status=$?
if kill -0 "$teardown_watchdog_pid" 2>/dev/null; then
  kill -TERM "$teardown_watchdog_pid" 2>/dev/null || true
fi
wait "$teardown_watchdog_pid" 2>/dev/null || true
set -e

if [ -f "$UR_ACCEPT_ARTIFACTS/teardown-stall.detected" ] ||
   grep -q '\[peerconn\]teardown stalled' "$UR_ACCEPT_ARTIFACTS/daemon.log" 2>/dev/null; then
  echo "acceptance daemon reported a WebRTC peer teardown stall" >&2
  exit 1
fi
if [ "$agent_status" -ne 0 ]; then
  tail -120 "$UR_ACCEPT_ARTIFACTS/daemon.log" >&2 || true
  exit "$agent_status"
fi
grep -q '"ok":true' "$UR_ACCEPT_ARTIFACTS/result.json" || {
  echo "acceptance agent did not emit a successful result" >&2
  exit 1
}

# The kernel compatibility decision must describe every installed tunnel
# generation. On Docker Desktop, the preceding nft probe is affirmative
# evidence that a socket-cgroup expression would fail; a successful Connecting
# and Connected apply with cgroup_match=0 therefore proves the builder omitted
# it and retained the proven mark. On a full kernel both generations must keep
# the cgroup belt instead.
filter_generations="$(grep -Ec '\[filter\] (connecting|connected).*socket_mark=1 cgroup_match=[01]' "$UR_ACCEPT_ARTIFACTS/daemon.log" || true)"
if [ "$filter_generations" -lt 2 ]; then
  echo "daemon did not publish the egress mode for both filter generations" >&2
  exit 1
fi
if grep -q 'Using the proven cgroup-BPF socket mark without the nft cgroup belt' "$UR_ACCEPT_ARTIFACTS/daemon.log"; then
  if grep -Eq '\[filter\] (connecting|connected).*cgroup_match=1' "$UR_ACCEPT_ARTIFACTS/daemon.log" || \
     [ "$(grep -Ec '\[filter\] (connecting|connected).*socket_mark=1 cgroup_match=0' "$UR_ACCEPT_ARTIFACTS/daemon.log" || true)" -lt 2 ]; then
    echo "daemon fallback log disagrees with the installed filter generations" >&2
    exit 1
  fi
elif [ "$(grep -Ec '\[filter\] (connecting|connected).*socket_mark=1 cgroup_match=1' "$UR_ACCEPT_ARTIFACTS/daemon.log" || true)" -lt 2 ]; then
  echo "daemon neither retained the nft cgroup belt nor declared its measured fallback" >&2
  exit 1
fi
if ip link show urnet0 >/dev/null 2>&1; then
  echo "urnet0 remained after disconnect" >&2
  exit 1
fi
