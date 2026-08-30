#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
#
# Provision the iOS simulator used by apple/test-main.sh and verify the local
# Apple SDK/signing prerequisites.
#
# Usage:
#   ./setup.sh
#   ./setup.sh --recreate
set -euo pipefail
umask 077

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/signing.sh"

device_name="${UR_ACCEPT_APPLE_SIMULATOR:-urnetwork-acceptance}"
development_team="6BGU69Q742"
recreate=0
for arg in "$@"; do
  case "$arg" in
    --recreate) recreate=1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

root="${URNETWORK_ROOT:-$(cd "$(dirname "$0")/../../.." && pwd)}"
tools_dir="${UR_ACCEPT_APPLE_TOOLS:-$root/build/all/apple/.acceptance-tools}"
case "$tools_dir" in
  /*) ;;
  *) tools_dir="$root/$tools_dir" ;;
esac
command -v timeout >/dev/null 2>&1 || { echo "ERROR: GNU timeout is required" >&2; exit 1; }
for command_name in xcodebuild xcrun go make node security openssl pbcopy pbpaste osascript; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "ERROR: $command_name is required" >&2; exit 1; }
done
case "$(uname -m)" in arm64|aarch64) host_arch=arm64 ;; x86_64|amd64) host_arch=amd64 ;; *) host_arch=unsupported ;; esac
[ -x "$root/warp/warpctl/build/darwin/$host_arch/warpctl" ] || {
  echo "ERROR: local darwin/$host_arch warpctl is missing; build warp/warpctl first" >&2
  exit 1
}

echo ">>> provisioning pinned Apple SDK build tools"
mkdir -p \
  "$tools_dir/go-bin" \
  "$tools_dir/go-cache" \
  "$tools_dir/go-mod-cache" \
  "$tools_dir/go-path"
(
  cd "$root/sdk/build"
  export GOBIN="$tools_dir/go-bin"
  export GOCACHE="$tools_dir/go-cache"
  export GOMODCACHE="$tools_dir/go-mod-cache"
  export GOPATH="$tools_dir/go-path"
  export PATH="$tools_dir/go-bin:$PATH"
  timeout 1800 make init_tools
)
for tool in gomobile gobind checksec; do
  [ -x "$tools_dir/go-bin/$tool" ] || {
    echo "ERROR: acceptance tool was not installed: $tools_dir/go-bin/$tool" >&2
    exit 1
  }
done

echo ">>> Xcode"
xcodebuild -version
xcrun --find simctl >/dev/null

existing_udid="$(xcrun simctl list devices available | sed -n "s/^[[:space:]]*${device_name} (\([^)]*\)).*/\1/p" | sed -n '1p')"
if [ -n "$existing_udid" ] && [ "$recreate" -eq 1 ]; then
  echo ">>> deleting simulator $device_name ($existing_udid)"
  timeout 30 xcrun simctl shutdown "$existing_udid" >/dev/null 2>&1 || true
  timeout 30 xcrun simctl delete "$existing_udid"
  existing_udid=""
fi

if [ -z "$existing_udid" ]; then
  runtime="$(xcrun simctl list runtimes available | awk '/^iOS / { runtime=$NF } END { print runtime }')"
  if [ -z "$runtime" ]; then
    echo ">>> installing the current iOS simulator runtime"
    timeout 7200 xcodebuild -downloadPlatform iOS
    runtime="$(xcrun simctl list runtimes available | awk '/^iOS / { runtime=$NF } END { print runtime }')"
  fi
  [ -n "$runtime" ] || { echo "ERROR: no iOS simulator runtime is available" >&2; exit 1; }
  device_type="$(xcrun simctl list devicetypes | awk '/^iPhone 17 / { print $NF; exit }' | tr -d '()')"
  [ -n "$device_type" ] || device_type="$(xcrun simctl list devicetypes | awk '/^iPhone / { print $NF; exit }' | tr -d '()')"
  [ -n "$device_type" ] || { echo "ERROR: no iPhone simulator device type is installed" >&2; exit 1; }
  echo ">>> creating $device_name ($device_type, $runtime)"
  existing_udid="$(xcrun simctl create "$device_name" "$device_type" "$runtime")"
else
  echo ">>> simulator $device_name already exists ($existing_udid)"
fi

was_booted=0
xcrun simctl list devices | grep -F "$existing_udid" | grep -q Booted && was_booted=1
cleanup() {
  exit_status=$?
  local simulator_state=""
  if [ "$was_booted" -ne 1 ]; then
    timeout 30 xcrun simctl shutdown "$existing_udid" >/dev/null 2>&1 || true
    if ! simulator_state="$(timeout 15 xcrun simctl list devices)"; then
      echo "ERROR: could not verify simulator $existing_udid" >&2
      exit_status=1
    elif printf '%s\n' "$simulator_state" | grep -F "$existing_udid" | grep -q Booted; then
      echo "ERROR: could not stop simulator $existing_udid" >&2
      exit_status=1
    fi
  fi
  exit "$exit_status"
}
trap cleanup EXIT

echo ">>> booting simulator smoke test"
xcrun simctl boot "$existing_udid" >/dev/null 2>&1 || true
timeout 180 xcrun simctl bootstatus "$existing_udid" -b
timeout 30 xcrun simctl spawn "$existing_udid" launchctl print system >/dev/null

echo ">>> code-signing identities"
signing_identities="$(security find-identity -v -p codesigning)"
printf '%s\n' "$signing_identities"
if ! development_identity="$(apple_development_identity_for_team "$development_team")"; then
  echo "ERROR: no Apple development identity for team $development_team is available; the macOS tunnel acceptance test must use that exact team." >&2
  exit 1
fi
echo ">>> private-key signing authorization"
if ! apple_verify_signing_identity_access "$development_identity"; then
  echo "ERROR: the Apple development private key for team $development_team is not authorized for unattended codesign use." >&2
  echo "       Run this setup interactively, enter the login-keychain password in the SecurityAgent prompt, and choose Always Allow." >&2
  exit 1
fi

cat <<EOF
>>> SMOKE TEST PASSED
Simulator: $device_name ($existing_udid)
Signing team: $development_team

macOS acceptance prerequisite:
  The signed app must be allowed to add/use its Network Extension VPN
  configuration. On a new runner, approve the one-time system prompt while
  the test window is visible. The runner cannot safely supply administrator
  credentials or bypass that OS consent.
EOF
