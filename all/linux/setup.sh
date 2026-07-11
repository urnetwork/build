#!/usr/bin/env bash
# One-time setup + smoke test for the Linux snap build environment — the Linux
# analog of windows/setup.sh. Builds the snapcraft builder container (one per
# arch, from ./Dockerfile) and verifies it has snapcraft + the C++/GTK4 toolchain
# the snap needs, WITHOUT running a full snap build. Run once on the build host
# before a release to confirm build.sh will work.
#
#   ./setup.sh                        # smoke-test the native arch (arm64)
#   ./setup.sh --arches "amd64 arm64" # smoke-test both (amd64 runs under emulation)
#
# Shares the Dockerfile with build.sh, so a green smoke test means build.sh runs
# in the same working container.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
image_base="urnetwork-snap-builder"

# Default to the arch that runs native on the Apple-Silicon host (fast). amd64
# builds/tests under Docker's qemu emulation, so it's opt-in for the smoke test.
ARCHES="${ARCHES:-arm64}"

usage() {
  cat <<EOF
Usage: setup.sh [--arches "amd64 arm64"]

Builds the snap builder container(s) and smoke-tests the toolchain.

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
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found — install Docker Desktop" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "ERROR: docker daemon not running — start Docker Desktop" >&2; exit 1; }

# --- build + smoke-test each arch --------------------------------------------
rc=0
for arch in ${ARCHES}; do
  echo ">>> building the snap builder image for ${arch} (deps baked in; layer-cached)"
  docker build --platform "linux/${arch}" -t "${image_base}:${arch}" "${here}"

  echo ">>> smoke-testing the ${arch} container"
  # Override the rock's snapcraft entrypoint to run our checks; mount the script ro.
  if docker run --rm --platform "linux/${arch}" --entrypoint bash \
       -v "${here}/smoke-test.sh:/smoke-test.sh:ro" \
       "${image_base}:${arch}" /smoke-test.sh; then
    echo ">>> ${arch}: SMOKE TEST PASSED"
  else
    echo ">>> ${arch}: SMOKE TEST FAILED" >&2
    rc=1
  fi
done

echo
if [ "${rc}" -eq 0 ]; then
  echo ">>> SMOKE TEST PASSED — the Linux build environment is ready for build.sh."
else
  echo "ERROR: one or more arches failed the smoke test — see the output above." >&2
  exit 1
fi
