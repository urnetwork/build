#!/usr/bin/env bash
# Reject known stale/missing Windows prerequisites without booting or changing
# the base image. Provisioning remains an explicit operator action.
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$here/lib.sh"
BUILD_HOME="${BUILD_HOME:-$(dirname "$(dirname "$here")")}"
win_init
expected_go_version="$(win_sdk_go_version "$BUILD_HOME/sdk/cgo/go.mod")" \
  || win_die "cannot determine the SDK Go version"
win_assert_image_ready "$expected_go_version" \
  || win_die "Windows image readiness failed; no VM or provisioning was started"
echo ">>> Windows image provisioning receipt matches the current contract (Go $expected_go_version); guest checks remain required"
