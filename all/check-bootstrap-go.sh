#!/usr/bin/env bash
# Use the Linux binaries bootstrap make already produced to catch compiler and
# reachable dependency advisories before candidates and release publication.
# Every final service binary retains its own mandatory publication-time scan.
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail
: "${BUILD_HOME:?set BUILD_HOME}"

for arch in amd64 arm64; do
  binary="$BUILD_HOME/warp/warpctl/build/linux/$arch/warpctl"
  if [ ! -f "$binary" ] || [ ! -s "$binary" ]; then
    echo "Bootstrap security check requires the Linux $arch Warpctl binary: $binary" >&2
    exit 1
  fi
  echo ">>> checking bootstrap Linux $arch Go binary for known vulnerabilities"
  govulncheck -mode=binary "$binary"
done
