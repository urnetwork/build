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

root="${URNETWORK_ROOT:-$(cd "$(dirname "$0")/../../.." && pwd)}"
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
  "platforms;android-${api}" \
  "ndk;$ndk_version" \
  "$image"
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

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-android-setup.XXXXXX")"
emulator_pid=""
serial=""
started_emulator=0

find_avd_serial() {
  local candidate state name devices
  devices="$(timeout 15 "$adb" devices)" || return 1
  while read -r candidate state _; do
    case "$candidate" in emulator-*) ;; *) continue ;; esac
    [ "$state" = device ] || continue
    name="$(timeout 10 "$adb" -s "$candidate" emu avd name 2>/dev/null | sed -n '1p' | tr -d '\r')"
    [ "$name" = "$avd_name" ] && { printf '%s\n' "$candidate"; return 0; }
  done <<<"$devices"
  return 1
}

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
  if [ "$started_emulator" -eq 1 ] && [ -n "$serial" ]; then
    timeout 15 "$adb" -s "$serial" emu kill >/dev/null 2>&1 || true
    for _ in $(seq 1 150); do
      kill -0 "$emulator_pid" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$emulator_pid" 2>/dev/null; then
      kill -TERM "$emulator_pid" 2>/dev/null || true
      for _ in $(seq 1 50); do
        kill -0 "$emulator_pid" 2>/dev/null || break
        sleep 0.2
      done
      if kill -0 "$emulator_pid" 2>/dev/null; then
        kill -KILL "$emulator_pid" 2>/dev/null || true
      fi
      echo "ERROR: acceptance emulator required forced cleanup" >&2
      exit_status=1
    fi
    wait "$emulator_pid" 2>/dev/null || true
  fi
  if ! rm -rf "$run_dir"; then
    exit_status=1
  fi
  exit "$exit_status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

serial="$(find_avd_serial || true)"
if [ -z "$serial" ]; then
  console_port="$(available_console_port)" || {
    echo "ERROR: no free Android emulator console port" >&2
    exit 1
  }
  serial="emulator-$console_port"
  args=(-avd "$avd_name" -no-snapshot -no-boot-anim -netdelay none -netspeed full -port "$console_port")
  [ "$headless" -eq 1 ] && args+=(-no-window)
  echo ">>> booting $avd_name for smoke test on $serial"
  "$emulator" "${args[@]}" >"$run_dir/emulator.log" 2>&1 &
  emulator_pid=$!
  started_emulator=1
else
  echo ">>> reusing running $avd_name on $serial for smoke test"
fi

timeout 180 "$adb" -s "$serial" wait-for-device
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

echo ">>> SMOKE TEST PASSED"
echo "AVD: $avd_name"
echo "ABI: $abi (required by the github, Solana, Ethos, and F-Droid targets)"
