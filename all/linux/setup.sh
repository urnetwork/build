#!/usr/bin/env bash
# One-time setup + smoke test for the Linux build environment — the Linux
# analog of windows/setup.sh. Builds the builder container (one per arch, from
# ./Dockerfile) and verifies it has the C++/GTK4 toolchain plus the deb /
# install-tarball / AppImage packaging tools, WITHOUT running a full build. Run
# once on the build host before a release to confirm build.sh will work.
#
#   ./setup.sh                        # smoke-test the native arch (arm64)
#   ./setup.sh --arches "amd64 arm64" # smoke-test both (amd64 runs under emulation)
#
# Shares the Dockerfile with build.sh, so a green smoke test means build.sh runs
# in the same working container.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail
umask 077

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
image_base="urnetwork-linux-builder"
# Two images per arch: daemon (Ubuntu 22.04, the declared glibc floor) and gui
# (Ubuntu 24.04, the oldest with GTK4). See Dockerfile.daemon's header.
roles="${ROLES:-daemon gui}"

# Default to the arch that runs native on the Apple-Silicon host (fast). amd64
# builds/tests under Docker's qemu emulation, so it's opt-in for the smoke test.
ARCHES="${ARCHES:-arm64}"

usage() {
  cat <<EOF
Usage: setup.sh [--arches "amd64 arm64"]

Builds the Linux builder container(s) and smoke-tests the toolchain.

Options:
  --arches "LIST"   space-separated arches to test (default "arm64"; amd64 is
                    emulated on Apple Silicon and slower)
  -h, --help        show this help

Requires: Docker Desktop running.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --arches)   ARCHES="$2"; shift 2 ;;
    --arches=*) ARCHES="${1#*=}"; shift ;;
    --arch)     ARCHES="$2"; shift 2 ;;
    --arch=*)   ARCHES="${1#*=}"; shift ;;
    -h|--help)  usage; exit 0 ;;
    *)          echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

# --- preflight ---------------------------------------------------------------
echo ">>> preflight: checking docker"
for command_name in docker timeout node go git make rsync zip zig; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "ERROR: $command_name is required by the Linux acceptance build" >&2
    exit 1
  }
done
docker info >/dev/null 2>&1 || { echo "ERROR: docker daemon not running — start Docker Desktop" >&2; exit 1; }

# --- build + smoke-test each arch --------------------------------------------
rc=0
for arch in ${ARCHES}; do
  for role in ${roles}; do
    echo ">>> building the Linux ${role} builder image for ${arch} (deps baked in; layer-cached)"
    timeout --signal=TERM --kill-after=60s 3600 \
      docker build --platform "linux/${arch}" \
      -f "${here}/Dockerfile.${role}" \
      -t "${image_base}-${role}:${arch}" "${here}"

    echo ">>> smoke-testing the ${arch}/${role} container"
    # Mount the check script ro and run it with bash (mount perms may drop +x).
    if timeout --signal=TERM --kill-after=30s 600 \
         docker run --rm --platform "linux/${arch}" \
         -v "${here}/smoke-test.sh:/smoke-test.sh:ro" \
         -e ROLE="${role}" \
         "${image_base}-${role}:${arch}" bash /smoke-test.sh; then
      echo ">>> ${arch}/${role}: SMOKE TEST PASSED"
    else
      echo ">>> ${arch}/${role}: SMOKE TEST FAILED" >&2
      rc=1
    fi
    if [ "$role" = daemon ]; then
      echo ">>> smoke-testing the acceptance tunnel privileges for ${arch}"
      if timeout --signal=TERM --kill-after=30s 120 \
           docker run --rm --platform "linux/${arch}" \
           --cap-add NET_ADMIN --device /dev/net/tun \
           "${image_base}-${role}:${arch}" \
           bash -euc 'test -c /dev/net/tun; ip tuntap add dev uraccept0 mode tun; ip link show dev uraccept0 >/dev/null; ip link delete uraccept0'; then
        echo ">>> ${arch}/${role}: NET_ADMIN + /dev/net/tun PASSED"
      else
        echo ">>> ${arch}/${role}: NET_ADMIN + /dev/net/tun FAILED" >&2
        rc=1
      fi
    fi
  done
done

echo
if [ "${rc}" -eq 0 ]; then
  echo ">>> SMOKE TEST PASSED — the Linux build environment is ready for build.sh."
else
  echo "ERROR: one or more arches failed the smoke test — see the output above." >&2
  exit 1
fi
