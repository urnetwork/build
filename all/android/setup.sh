#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
#
# Provision the arm64 Android emulator used by android/test-main.sh.
#
# Usage:
#   ./setup.sh              install tools/image, create AVD, and smoke-test it
#   ./setup.sh --recreate   replace the existing urnetwork-acceptance AVD
#   ./setup.sh --headless   do not show the emulator window during smoke test
set -euo pipefail
umask 077

avd_name="${UR_ACCEPT_ANDROID_AVD:-urnetwork-acceptance}"
api="${UR_ACCEPT_ANDROID_API:-36}"
build_tools_version="${UR_ACCEPT_ANDROID_BUILD_TOOLS:-36.0.0}"
ndk_version="${UR_ACCEPT_ANDROID_NDK:-29.0.14206865}"
recreate=0
headless=0

for arg in "$@"; do
  case "$arg" in
    --recreate) recreate=1 ;;
    --headless) headless=1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

here="$(cd "$(dirname "$0")" && pwd)"
root="${URNETWORK_ROOT:-$(cd "$here/../../.." && pwd)}"
android_acceptance_lib="$root/android/test-main-lib.sh"
[ -f "$android_acceptance_lib" ] || {
  echo "ERROR: Android acceptance helper library is missing" >&2
  exit 1
}
# setup and the acceptance runner must classify the same guest CPU, renderer,
# and foreground-window evidence before either reports a usable AVD.
# shellcheck disable=SC1090
source "$android_acceptance_lib"
tools_dir="${UR_ACCEPT_ANDROID_TOOLS:-$root/build/all/android/.acceptance-tools}"
case "$tools_dir" in
  /*) ;;
  *) tools_dir="$root/$tools_dir" ;;
esac
sdk_root="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-$HOME/Library/Android/sdk}}"
sdkmanager="$sdk_root/cmdline-tools/latest/bin/sdkmanager"
avdmanager="$sdk_root/cmdline-tools/latest/bin/avdmanager"
adb="$sdk_root/platform-tools/adb"
emulator="$sdk_root/emulator/emulator"
image="system-images;android-${api};google_apis;arm64-v8a"
network_test_gate="$root/tests/network-intensive-suite-lock.sh"
if [ ! -x "$network_test_gate" ]; then
  echo "Android setup suite gate is missing or not executable: $network_test_gate" >&2
  exit 127
fi
if [ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]; then
  exec "$network_test_gate" main-acceptance android-setup -- \
    "$here/setup.sh" "$@"
fi
if ! "$network_test_gate" --verify-held main-acceptance; then
  echo "Android setup inherited an invalid network-intensive lock" >&2
  exit 70
fi
command -v timeout >/dev/null 2>&1 || { echo "ERROR: GNU timeout is required" >&2; exit 1; }
for command_name in java go make node rsync; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "ERROR: $command_name is required" >&2; exit 1; }
done
[ -x "$root/android/app/gradlew" ] || { echo "ERROR: Android Gradle wrapper is missing" >&2; exit 1; }
case "$(uname -s)" in Darwin) host_os=darwin ;; Linux) host_os=linux ;; *) host_os=unsupported ;; esac
case "$(uname -m)" in arm64|aarch64) host_arch=arm64 ;; x86_64|amd64) host_arch=amd64 ;; *) host_arch=unsupported ;; esac
[ -x "$root/warp/warpctl/build/$host_os/$host_arch/warpctl" ] || {
  echo "ERROR: local $host_os/$host_arch warpctl is missing; build warp/warpctl first" >&2
  exit 1
}

for tool in "$sdkmanager" "$avdmanager"; do
  [ -x "$tool" ] || {
    echo "ERROR: Android command-line tools not found at $tool" >&2
    echo "Install Android Studio command-line tools or set ANDROID_SDK_ROOT." >&2
    exit 1
  }
done

echo ">>> accepting Android SDK licenses"
yes | timeout 300 "$sdkmanager" --licenses >/dev/null || true

echo ">>> installing Android ${api} arm64 acceptance dependencies"
timeout 1800 "$sdkmanager" \
  platform-tools \
  emulator \
  "build-tools;$build_tools_version" \
  "platforms;android-${api}" \
  "ndk;$ndk_version" \
  "$image"
build_tools_dir="$sdk_root/build-tools/$build_tools_version"
for tool in aapt2 apksigner zipalign; do
  [ -x "$build_tools_dir/$tool" ] || {
    echo "ERROR: Android build-tools;$build_tools_version is missing $tool" >&2
    exit 1
  }
done
ndk_objcopy="$(find "$sdk_root/ndk/$ndk_version" -type f -name llvm-objcopy -perm -111 -print -quit)"
[ -n "$ndk_objcopy" ] || {
  echo "ERROR: Android NDK $ndk_version has no llvm-objcopy" >&2
  exit 1
  }

echo ">>> provisioning pinned Android SDK build tools"
mkdir -p \
  "$tools_dir/go-bin" \
  "$tools_dir/go-cache" \
  "$tools_dir/go-mod-cache" \
  "$tools_dir/go-path"
(
  cd "$root/sdk/build"
  export ANDROID_HOME="$sdk_root"
  export ANDROID_NDK_HOME="$sdk_root/ndk/$ndk_version"
  export ANDROID_SDK_ROOT="$sdk_root"
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

if "$emulator" -list-avds | grep -Fxq "$avd_name"; then
  if [ "$recreate" -eq 1 ]; then
    while read -r candidate state _; do
      case "$candidate" in emulator-*) ;; *) continue ;; esac
      [ "$state" = device ] || continue
      running_name="$(timeout 10 "$adb" -s "$candidate" emu avd name 2>/dev/null | sed -n '1p' | tr -d '\r')"
      if [ "$running_name" = "$avd_name" ]; then
        echo ">>> stopping running $avd_name on $candidate"
        timeout 15 "$adb" -s "$candidate" emu kill >/dev/null 2>&1 || true
        timeout 60 "$adb" -s "$candidate" wait-for-disconnect >/dev/null 2>&1 || true
      fi
    done < <(timeout 15 "$adb" devices)
    echo ">>> deleting the existing $avd_name AVD"
    "$avdmanager" delete avd --name "$avd_name"
  else
    echo ">>> AVD $avd_name already exists"
  fi
fi

if ! "$emulator" -list-avds | grep -Fxq "$avd_name"; then
  echo ">>> creating $avd_name from $image"
  printf 'no\n' | "$avdmanager" create avd \
    --force \
    --name "$avd_name" \
    --package "$image" \
    --device "pixel_6"
fi

if [ "$avd_name" = urnetwork-acceptance ]; then
  legacy_avd_state="$(android_acceptance_retire_legacy_reserved_avd \
    "$adb" "$avd_name")" || {
    echo "ERROR: could not safely retire or classify the reserved acceptance AVD" >&2
    exit 1
  }
  if [ "$legacy_avd_state" = retired ]; then
    echo ">>> retired a default-ID legacy acceptance AVD instance"
  fi
fi
running_avd_status=0
android_acceptance_no_running_avd "$adb" "$avd_name" || running_avd_status=$?
case "$running_avd_status" in
  0) ;;
  1)
    echo "ERROR: AVD $avd_name is already running outside this setup invocation" >&2
    echo "Stop it explicitly or rerun setup with --recreate." >&2
    exit 1
    ;;
  *)
    echo "ERROR: could not prove that AVD $avd_name has no pre-existing emulator instance" >&2
    exit 1
    ;;
esac

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-android-setup.XXXXXX")"
emulator_pid=""
emulator_owner_token="setup-$$-$RANDOM"
serial=""
started_emulator=0

available_console_port() {
  local port
  for port in $(seq 5554 2 5682); do
    if ! /usr/bin/nc -z 127.0.0.1 "$port" >/dev/null 2>&1 &&
       ! /usr/bin/nc -z 127.0.0.1 "$((port + 1))" >/dev/null 2>&1; then
      printf '%s\n' "$port"
      return 0
    fi
  done
  return 1
}

cleanup() {
  exit_status=$?
  local cleanup_grace=0
  if [ "$started_emulator" -eq 1 ] && [ -n "$serial" ]; then
    if android_acceptance_runner_owns_emulator \
        "$adb" "$serial" "$avd_name" "$emulator_pid" \
        "$emulator_owner_token"; then
      cleanup_grace=150
      timeout 15 "$adb" -s "$serial" emu kill >/dev/null 2>&1 || true
    fi
    if ! android_acceptance_stop_emulator_child \
        "$emulator_pid" "$cleanup_grace" 50; then
      echo "ERROR: acceptance emulator required forced cleanup" >&2
      exit_status=1
    fi
  fi
  if ! rm -rf "$run_dir"; then
    exit_status=1
  fi
  exit "$exit_status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

console_port="$(available_console_port)" || {
  echo "ERROR: no free Android emulator console port" >&2
  exit 1
}
serial="emulator-$console_port"
args=(-avd "$avd_name" -read-only -gpu host -no-snapshot -no-boot-anim -netdelay none -netspeed full -port "$console_port")
[ "$headless" -eq 1 ] && args+=(-no-window)
echo ">>> booting a runner-owned $avd_name for smoke test"
run_android_acceptance_shared_avd_emulator \
  "$emulator" "$run_dir/emulator.log" "$emulator_owner_token" \
  "${args[@]}" &
emulator_pid=$!
started_emulator=1

android_acceptance_wait_for_runner_owned_emulator \
  "$adb" "$serial" "$avd_name" "$emulator_pid" \
  "$emulator_owner_token" 360 || {
  echo "ERROR: setup emulator did not prove ownership by this invocation" >&2
  exit 1
}
for _ in $(seq 1 180); do
  [ "$(timeout 10 "$adb" -s "$serial" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ] && break
  sleep 2
done
[ "$(timeout 10 "$adb" -s "$serial" shell getprop sys.boot_completed | tr -d '\r')" = 1 ] || {
  tail -100 "$run_dir/emulator.log" >&2
  echo "ERROR: emulator did not boot" >&2
  exit 1
}
actual_avd="$(timeout 10 "$adb" -s "$serial" emu avd name 2>/dev/null | sed -n '1p' | tr -d '\r')"
[ "$actual_avd" = "$avd_name" ] || { echo "ERROR: $serial is $actual_avd, expected $avd_name" >&2; exit 1; }

abi="$(timeout 10 "$adb" -s "$serial" shell getprop ro.product.cpu.abi | tr -d '\r')"
[ "$abi" = arm64-v8a ] || { echo "ERROR: expected arm64-v8a, got $abi" >&2; exit 1; }
network_ready=0
for _ in $(seq 1 24); do
  if timeout 10 "$adb" -s "$serial" shell ping -c 1 -W 3 api.bringyour.com >/dev/null 2>&1; then
    network_ready=1
    break
  fi
  sleep 5
done
[ "$network_ready" -eq 1 ] || {
  echo "ERROR: emulator has no DNS/network route to api.bringyour.com" >&2
  exit 1
}

renderer_evidence="$run_dir/emulator.log"
setup_preflight="$run_dir/setup-smoke-preflight.txt"
if ! android_acceptance_preflight_device \
    "$adb" "$serial" setup-avd setup-smoke "$setup_preflight" "$renderer_evidence"; then
  echo "ERROR: acceptance AVD failed its CPU, host-renderer, or focused-dialog preflight" >&2
  sed -n '1,20p' "$setup_preflight" >&2
  exit 1
fi

echo ">>> SMOKE TEST PASSED"
echo "AVD: $avd_name"
echo "Build tools: $build_tools_version"
echo "ABI: $abi (required by the github, Solana, and F-Droid targets)"
