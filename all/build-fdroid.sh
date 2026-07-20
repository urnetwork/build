#!/usr/bin/env bash
# Build the URnetwork Android github/ungoogle flavor APKs from the LOCAL working
# tree, inside the F-Droid buildserver container. The fdroid server context
# matters: building on the host produces small artifact differences (macos/arm
# versus linux/amd perhaps), and F-Droid's reproducible-build verification needs
# the container-built APKs.
#
# One script, two phases:
#   host phase       (default) stages local repos if configured, then
#                    docker-runs the buildserver image with THIS script (from
#                    the mounted build tree) as the entrypoint, passing
#                    `container-build`
#   container phase  (`container-build` arg) installs the toolchain (go, jdk,
#                    ndk, debug key) and runs gradle `buildSdk assembleGithub`
#
# This is the fdroid build part of run.sh, extracted so it can also run
# standalone. It uses the local branches AS-IS — no pulls, no checkouts, no
# version edits — and assumes run.sh (or the operator) already configured the
# android repo on its v<version>-ungoogle branch (ungoogled gradle settings +
# app/local.properties with warp.version/warp.version_code).
#
# APKs land in $BUILD_HOME/android/app/app/build/outputs/apk/github/release/.
#
# NOTE - the fdroiddata recipe (metadata/com.bringyour.network.yml) seds the
#        go toolchain version out of this file in its prebuild, to build the
#        same go from source. Its sed path must track this file's location
#        (formerly fdroid/build.sh). See the go check in container_build.
#
# FIXME - there is currently a bug on apple m4 that causes an "invalid
#         instruction" error when using the docker apple virtualization
#         framework, see https://github.com/golang/go/issues/71434
#         The docker vmm framework is a work around, but it is very slow for
#         amd64. Run on an m1 or intel mac for now.
#
# Inputs (env, host phase):
#   WARP_HOME   warp home; $WARP_HOME/release (release signing config) is
#               mounted into the container
#   BUILD_HOME  (optional) build home, mounted into the container as the build
#               tree (default: this script's parent dir)
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail


# ----------------------------------------------------------------------------
# container phase — runs INSIDE the fdroid buildserver container (user vagrant,
# debian amd64), with the build tree mounted at /urnetwork/build and the
# release signing config at /urnetwork/release
# ----------------------------------------------------------------------------
container_build() {
    echo ">>> root init"
    sudo apt-get update
    sudo apt-get install -y gcc libc-dev make
    echo "deb https://deb.debian.org/debian trixie main" | sudo tee /etc/apt/sources.list.d/trixie.list
    sudo apt-get update
    sudo apt-get install -y -t trixie openjdk-21-jdk-headless
    sudo update-alternatives --auto java
    curl -L https://go.dev/dl/go1.26.5.linux-amd64.tar.gz | sudo tar -xz -C /usr/local/

    echo ">>> android ndk install"
    export ANDROID_HOME=/opt/android-sdk
    local ANDROID_NDK_VERSION=29.0.14206865
    sdkmanager "ndk;$ANDROID_NDK_VERSION" 'platforms;android-36'
    export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"

    echo ">>> android debug key init"
    mkdir "$HOME/.android"
    keytool -genkey -v \
        -keystore "$HOME/.android/debug.keystore" \
        -alias androiddebugkey \
        -dname "CN=ur.network, OU=URnetwork, O=URnetwork, L=San Francisco, S=California, C=US" \
        -storepass android \
        -keypass android \
        -keyalg RSA \
        -validity 14000

    # The quoted go-version check below is load-bearing beyond this script:
    # the fdroiddata recipe seds the go<major.minor.patch> token out of this
    # file to build that exact go from source. When bumping go, keep the check
    # in this exact quoted form, and keep it the only such quoted full version
    # string in this file.
    local go_version java_version
    go_version=$(go version 2> /dev/null || true)
    if [ "$go_version" == "" ]; then
        echo "go check: go will use /usr/local/go ($(/usr/local/go/bin/go version))"
    elif [[ "$go_version" =~ "go version go1.26.5" ]]; then
        echo "go check: go will use system go ($go_version)"
    else
        echo "go check: system go must either be 1.26.5 or not installed"
        exit 1
    fi
    java_version=$(java -version 2>&1 || true)
    if [[ ! "$java_version" =~ 'openjdk version "21.0.' ]]; then
        echo "java check: 21.0.x required ($java_version)"
        exit 1
    fi

    export WARP_HOME=/urnetwork
    export BRINGYOUR_HOME=/urnetwork/build
    export GRADLE_OPTS="-Dorg.gradle.daemon=false -Dorg.gradle.workers.max=1 -Dkotlin.compiler.execution.strategy=in-process"

    cd "$BRINGYOUR_HOME/android/app"
    ./gradlew clean buildSdk assembleGithub
}

if [ "${1:-}" == "container-build" ]; then
    container_build
    exit 0
fi


# ----------------------------------------------------------------------------
# host phase
# ----------------------------------------------------------------------------
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_HOME="${BUILD_HOME:-$(dirname "$here")}"
: "${WARP_HOME:?set WARP_HOME}"

# Optionally stage local working-tree repos over the build root so this builds
# LOCAL (possibly uncommitted) changes. No-op unless SRC_HOME / SRC_<REPO> is set
# (release builds via run.sh stage BUILD_HOME themselves and pass no SRC_*).
# The gradle `buildSdk` task builds the aar from sdk (whose go.mod replaces sdk,
# connect, AND glog), so all three must be staged together, plus the android app
# repo. NOTE: the android repo must already carry the ungoogle gradle settings +
# app/local.properties (warp.version/warp.version_code); staging copies your local
# android working tree AS-IS, so configure it before building.
# shellcheck source=stage-local-repos.sh
source "$here/stage-local-repos.sh"
stage_local_repos sdk connect glog android

echo ">>> building the android github flavor in the fdroid buildserver container (android on branch $(git -C "$BUILD_HOME/android" branch --show-current))"
docker pull registry.gitlab.com/fdroid/fdroidserver:buildserver
docker run --oom-kill-disable --memory="8192m" --rm -u vagrant \
    --entrypoint /urnetwork/build/all/build-fdroid.sh \
    -v "$WARP_HOME/release:/urnetwork/release:z" \
    -v "$BUILD_HOME:/urnetwork/build:Z" \
    registry.gitlab.com/fdroid/fdroidserver:buildserver \
    container-build
