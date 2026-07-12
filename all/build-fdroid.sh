#!/usr/bin/env bash
# Build the URnetwork Android github/ungoogle flavor APKs from the LOCAL working
# tree, inside the F-Droid buildserver container (fdroid/build.sh runs gradle
# `buildSdk assembleGithub` in there). The fdroid server context matters:
# building on the host produces small artifact differences (macos/arm versus
# linux/amd perhaps), and F-Droid's reproducible-build verification needs the
# container-built APKs.
#
# This is the fdroid build part of run.sh, extracted so it can also run
# standalone. It uses the local branches AS-IS — no pulls, no checkouts, no
# version edits — and assumes run.sh (or the operator) already configured the
# android repo on its v<version>-ungoogle branch (ungoogled gradle settings +
# app/local.properties with warp.version/warp.version_code).
#
# APKs land in $BUILD_HOME/android/app/app/build/outputs/apk/github/release/.
#
# FIXME - there is currently a bug on apple m4 that causes an "invalid
#         instruction" error when using the docker apple virtualization
#         framework, see https://github.com/golang/go/issues/71434
#         The docker vmm framework is a work around, but it is very slow for
#         amd64. Run on an m1 or intel mac for now.
#
# Inputs (env):
#   WARP_HOME   warp home; $WARP_HOME/release (release signing config) is
#               mounted into the container
#   BUILD_HOME  (optional) build home, mounted into the container as the build
#               tree (default: this script's parent dir)
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_HOME="${BUILD_HOME:-$(dirname "$here")}"
: "${WARP_HOME:?set WARP_HOME}"

echo ">>> building the android github flavor in the fdroid buildserver container (android on branch $(git -C "$BUILD_HOME/android" branch --show-current))"
docker pull registry.gitlab.com/fdroid/fdroidserver:buildserver
docker run --oom-kill-disable --memory="8192m" --rm -u vagrant \
    --entrypoint /urnetwork/build/fdroid/build.sh \
    -v "$WARP_HOME/release:/urnetwork/release:z" \
    -v "$BUILD_HOME:/urnetwork/build:Z" \
    registry.gitlab.com/fdroid/fdroidserver:buildserver
