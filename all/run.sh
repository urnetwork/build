#!/usr/bin/env zsh

# Expect the following env vars to be set:
# WARP_HOME
# APPLE_API_KEY
# APPLE_API_ISSUER
# GITHUB_API_KEY
# (optional) BUILD_OUT
# (optional) SLACK_WEBHOOK
# (optional) WARP_SKIP_DEPLOY set to skip deployment
# (optional) CONNECT_IP_UPDATE set (non-empty) to regenerate the connect IP
#            tables (security + blocker) from the live feeds before the tests
#            run, then push them to connect main (commit message stamped with
#            the release version) before the version branches are cut
# (optional) BUILD_APPLE_IDENTITY set (non-empty) to create a per-run apple
#            signing keychain from ~/.identity.p12 + ~/.p12-pw (see the
#            keychain note below and REMOTEBUILD.md option 1)
#
# The Windows build image is built once, out of band, by build/all/windows/setup.sh
# (which takes the Windows 11 ARM64 + virtio-win ISOs). run.sh only boots that image
# and rsyncs the build home into it, so it needs no ISO env vars here.
#
# The apple stages need a signing keychain that is unlocked *in this build's own
# login session*: keychain unlock state is scoped to the audit session that ran
# the unlock, so an unlock from any other ssh session is invisible here, CodeSign
# fails with `errSecInternalComponent`, the ios/macos archives are skipped, and
# the release then hard-fails uploading the missing .ipa. See REMOTEBUILD.md for
# the full analysis and the ranked options.
# With BUILD_APPLE_IDENTITY set, this script creates a throwaway per-run build
# keychain (REMOTEBUILD.md option 1): it imports ~/.identity.p12 (passphrase in
# ~/.p12-pw, both chmod 600, created by the one-time export in REMOTEBUILD.md),
# grants non-interactive key access to the apple tools, prepends the keychain to
# the user search list, smoke-tests codesign up front, and deletes the keychain +
# restores the search list on exit. The login keychain is never touched.
# Without BUILD_APPLE_IDENTITY (e.g. a GUI dev machine), the login keychain must
# already be unlocked in this session with non-interactive key access granted
# (REMOTEBUILD.md option 2 -- the builder's old ~/urnetwork/build.sh unlock +
# keep-alive pattern). One-time per key in that mode:
#   security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$PW" \
#       ~/Library/Keychains/login.keychain-db
# Verify signing works in the build session (should print no error):
#   echo x > /tmp/t && codesign -f -s "Apple Development: Product Builder (F7RQ5ZZ798)" /tmp/t

# note: if docker fails to build, try 1. removing all images then 2. pruning the build images:
#       docker rmi `docker images -a -q`
#       docker system prune


# Check the complete host toolchain before this script pulls repositories,
# creates a signing keychain, installs an NDK, or performs any other mutation.
# Keep this list in sync with the host-side commands used here and by the
# build scripts this runner invokes. Tools installed by this runner itself
# (currently osv-scanner and warpctl), or inside a purpose-built container
# (including flatpak/flatpak-builder), do not belong in this list.
required_build_tools=(
    bash
    codesign
    curl
    docker
    ffprobe
    git
    go
    gsed
    hdiutil
    java
    jq
    make
    nc
    node
    npm
    npx
    openssl
    pandoc
    pip
    productbuild
    productsign
    python
    python3
    qemu-img
    qemu-system-aarch64
    realpath
    rsync
    scp
    sdkmanager
    security
    shasum
    ssh
    ssh-keygen
    tar
    timeout
    unzip
    xcodebuild
    xcrun
    zip
    zig
)
if [ "$BUILD_TEST" ]; then
    # Only the optional acceptance-test phase uses these host tools.
    required_build_tools+=(lsof pkill sudo)
fi

missing_build_tools=()
for build_tool in "${required_build_tools[@]}"; do
    if ! command -v "$build_tool" >/dev/null 2>&1; then
        missing_build_tools+=("$build_tool")
    fi
done
if [ -z "$NVM_DIR" ] || [ ! -r "$NVM_DIR/nvm.sh" ]; then
    missing_build_tools+=("nvm (set NVM_DIR to a readable nvm installation)")
fi
if (( ${#missing_build_tools[@]} )); then
    echo "Missing required build tool(s):" >&2
    printf '  - %s\n' "${missing_build_tools[@]}" >&2
    echo "Install every missing tool before running build/all/run.sh." >&2
    exit 1
fi

if [[ ! `go version` =~ 'go version go1.26.7' ]]; then
    echo 'go 1.26.7 required' >&2
    exit 1
fi
if [[ ! `java -version 2>&1` =~ 'openjdk version "21.0.' ]]; then
    echo 'java 21.0.x required' >&2
    exit 1
fi
if [[ ! `node --version 2>&1` =~ 'v24.14.1' ]]; then
    echo 'node 24.14.1 required' >&2
    exit 1
fi
if [[ ! `npm --version 2>&1` =~ '11.11.0' ]]; then
    echo 'npm 11.11.0 required' >&2
    exit 1
fi
if ! docker info >/dev/null 2>&1; then
    echo 'Docker is required and its daemon must be running.' >&2
    exit 1
fi
if ! xcodebuild -version >/dev/null 2>&1; then
    echo 'A working Xcode command-line toolchain is required.' >&2
    exit 1
fi


builder_message () {
    echo -n "$1\n"
    if [ "$SLACK_WEBHOOK" ]; then
        data="{\"text\":$(echo -n $1 | jq -Rsa .), \"blocks\":[{\"type\":\"section\", \"text\":{\"type\":\"mrkdwn\", \"text\":$(echo -n "$1" | jq -Rsa .)}}]}"
        curl -s -o /dev/null -X POST -H 'Content-type: application/json' --data "$data" $SLACK_WEBHOOK
    fi
}

error_trap () {
    code=$?
    if [ $code != 0 ]; then
        builder_message "error($code): $1"
        exit $code
    fi
}

warn_trap () {
    code=$?
    if [ $code != 0 ]; then
        builder_message "warning($code): $1. Build will continue."
    fi
}


export BUILD_HOME=`realpath ..`
export BUILD_ENV=main
export BUILD_SED=gsed
# -S/--fail-with-body: stay quiet on success, but on HTTP >= 400 fail with a
# non-zero exit (so error_trap fires) while still printing the response body and
# the curl error line. Without this, API errors (e.g. an expired GITHUB_API_KEY
# returning 401) are swallowed and the release silently fails to upload/publish.
export BUILD_CURL=(curl -s -S -L --fail-with-body)
export BRINGYOUR_HOME=`realpath ..`
if [ ! "$STAGE_SECONDS" ]; then
    export STAGE_SECONDS=60
fi


# Per-run apple signing keychain (BUILD_APPLE_IDENTITY; see the header note and
# REMOTEBUILD.md option 1). Runs before any long work so a signing problem fails
# the build in seconds, not an hour in. The BUILD_TEST block below re-arms this
# cleanup when it swaps the EXIT trap.
cleanup_apple_identity () {
    if [ "$APPLE_IDENTITY_KC" ]; then
        echo "$APPLE_IDENTITY_ORIG_KEYCHAINS" | xargs security list-keychains -d user -s
        security delete-keychain "$APPLE_IDENTITY_KC" 2>/dev/null
        APPLE_IDENTITY_KC=""
    fi
}
APPLE_IDENTITY_KC=""
if [ "$BUILD_APPLE_IDENTITY" ]; then
    if [ ! -f ~/.identity.p12 ] || [ ! -f ~/.p12-pw ]; then
        builder_message 'BUILD_APPLE_IDENTITY is set but ~/.identity.p12 / ~/.p12-pw is missing (see REMOTEBUILD.md)'
        exit 1
    fi
    # sweep a stale keychain left by a killed run (this also drops it from the
    # search list), then capture the search list to restore on exit
    security delete-keychain "$HOME/Library/Keychains/build.keychain-db" 2>/dev/null
    APPLE_IDENTITY_ORIG_KEYCHAINS=`security list-keychains -d user`
    APPLE_IDENTITY_KC="$HOME/Library/Keychains/build.keychain-db"
    # throwaway keychain, throwaway password -- nothing stored
    APPLE_IDENTITY_KC_PW=`openssl rand -base64 24`
    trap cleanup_apple_identity EXIT
    security create-keychain -p "$APPLE_IDENTITY_KC_PW" "$APPLE_IDENTITY_KC" &&
        security set-keychain-settings -lut 21600 "$APPLE_IDENTITY_KC" &&
        security unlock-keychain -p "$APPLE_IDENTITY_KC_PW" "$APPLE_IDENTITY_KC"
    error_trap 'apple identity keychain create'
    # import every identity on the box (~/.identity.p12 plus optional extras
    # like ~/.identity-dist.p12 / ~/.identity-installer.p12 — all share the
    # ~/.p12-pw passphrase; see all/make-apple-dist-identity.sh). The archive
    # signs with Apple Development; the store export additionally needs Apple
    # Distribution (and Mac Installer Distribution for the macOS .pkg) when
    # cloud signing is unavailable.
    for identity_p12 in "$HOME"/.identity*.p12; do
        security import "$identity_p12" -P "$(cat ~/.p12-pw)" -f pkcs12 \
            -T /usr/bin/codesign -T /usr/bin/security \
            -T /usr/bin/productbuild -T /usr/bin/productsign \
            -k "$APPLE_IDENTITY_KC"
        error_trap "apple identity keychain import $(basename "$identity_p12")"
    done
    # set-key-partition-list silently no-ops when the key is not in the targeted
    # keychain, so prove the import landed first (macOS 26 gotcha, REMOTEBUILD.md)
    security find-identity -v -p codesigning "$APPLE_IDENTITY_KC" | grep -q F7RQ5ZZ798
    error_trap 'apple identity missing from build keychain'
    security set-key-partition-list -S apple-tool:,apple: -s -k "$APPLE_IDENTITY_KC_PW" "$APPLE_IDENTITY_KC" > /dev/null
    error_trap 'apple identity partition list'
    # record what landed, so a missing distribution identity is obvious in the log
    security find-identity -v "$APPLE_IDENTITY_KC"
    echo "$APPLE_IDENTITY_ORIG_KEYCHAINS" | xargs security list-keychains -d user -s "$APPLE_IDENTITY_KC"
    error_trap 'apple identity keychain search list'
    # prove signing works in THIS session before spending an hour building
    echo x > /tmp/apple-identity-smoke &&
        codesign -f -s "Apple Development: Product Builder (F7RQ5ZZ798)" /tmp/apple-identity-smoke
    error_trap 'apple identity codesign smoke test'
    rm -f /tmp/apple-identity-smoke
fi


git_main () {
    branch_name=main
    if [ $1 ]; then
        branch_name=$1
    fi
    git diff --quiet && git diff --cached --quiet && git checkout $branch_name && git pull --recurse-submodules
}

(cd $WARP_HOME/config && git_main)
error_trap 'pull warp config'
(cd $WARP_HOME/vault && git_main)
error_trap 'pull warp vault'
(cd $WARP_HOME/release && git_main)
error_trap 'pull warp release'


if [ "$BUILD_RESET" ]; then
    (cd $BUILD_HOME && rm -rf connect)
    (cd $BUILD_HOME && rm -rf sdk)
    (cd $BUILD_HOME && rm -rf android)
    (cd $BUILD_HOME && rm -rf apple)
    (cd $BUILD_HOME && rm -rf windows)
    (cd $BUILD_HOME && rm -rf linux)
    (cd $BUILD_HOME && rm -rf sn)
    (cd $BUILD_HOME && rm -rf server)
    (cd $BUILD_HOME && rm -rf web)
    (cd $BUILD_HOME && rm -rf docs)
    (cd $BUILD_HOME && rm -rf warp)
    (cd $BUILD_HOME && rm -rf glog)
    (cd $BUILD_HOME && rm -rf proxy)
    (cd $BUILD_HOME && rm -rf operator-proxy)
    (cd $BUILD_HOME && rm -rf userwireguard)
    (cd $BUILD_HOME && rm -rf goidenticons)
    (cd $BUILD_HOME && rm -rf extension)
    (cd $BUILD_HOME && rm -rf localizations)
    (cd $BUILD_HOME && 
        git stash -u && 
        git reset --hard && 
        git submodule update --init)
    error_trap 'reset'
fi


BUILD_PRE_COMMIT=`cd $BUILD_HOME && git log -1 --format=%H`
# rebase-pull (not plain pull) for the build repo itself: a failed prior run
# can strand its machine-generated version-stamp commit on local main (the
# stamp push raced another push or the network), and a plain pull then fails
# on the divergence — every later run would fail here until someone
# intervened. Rebasing carries the stray stamp forward; the next successful
# stamp push includes it.
(cd $BUILD_HOME &&
    git diff --quiet && git diff --cached --quiet &&
    git checkout main &&
    git pull --rebase &&
    git submodule update --init --recursive)
error_trap 'pull'
BUILD_COMMIT=`cd $BUILD_HOME && git log -1 --format=%H`
if [ "$BUILD_PRE_COMMIT" != "$BUILD_COMMIT" ]; then
    builder_message "Build repo updated. Must restart to use the latest script."
    exit 1
fi


ANDROID_NDK_VERSION=29.0.14206865
sdkmanager "ndk;$ANDROID_NDK_VERSION"
error_trap 'android ndk'
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"


(cd $BUILD_HOME/connect && git_main)
error_trap 'pull connect'
(cd $BUILD_HOME/sdk && git_main)
error_trap 'pull sdk'
(cd $BUILD_HOME/android && git_main)
error_trap 'pull android'
(cd $BUILD_HOME/apple && git_main)
error_trap 'pull apple'
(cd $BUILD_HOME/windows && git_main)
error_trap 'pull windows'
(cd $BUILD_HOME/linux && git_main)
error_trap 'pull linux'
(cd $BUILD_HOME/sn && git_main)
error_trap 'pull sn'
(cd $BUILD_HOME/server && git_main)
error_trap 'pull server'
(cd $BUILD_HOME/web && git_main)
error_trap 'pull web'
(cd $BUILD_HOME/docs && git_main)
error_trap 'pull docs'
(cd $BUILD_HOME/warp && git_main)
error_trap 'pull warp'
(cd $BUILD_HOME/glog && git_main master)
error_trap 'pull glog'
(cd $BUILD_HOME/proxy && git_main)
error_trap 'pull proxy'
(cd $BUILD_HOME/operator-proxy && git_main)
error_trap 'pull operator-proxy'
(cd $BUILD_HOME/userwireguard && git_main master)
error_trap 'pull userwireguard'
(cd $BUILD_HOME/goidenticons && git_main)
error_trap 'pull goidenticons'
(cd $BUILD_HOME/extension && git_main)
error_trap 'pull extension'
(cd $BUILD_HOME/localizations && git_main)
error_trap 'pull localizations'
(cd $WARP_HOME/mmm && git_main)
error_trap 'pull mmm'

# refresh the generated connect IP tables from the live threat feeds:
# security/main.go -> ip_security_cfaa_block.go, blocker/main.go -> ip_blocker_block.go.
# Runs while every repo is still on main so the tests below exercise the update.
# The commit + push to connect main happens after the release version is staged
# (the commit message carries it) and before the version branches are cut — see
# the matching CONNECT_IP_UPDATE block below.
if [ "$CONNECT_IP_UPDATE" ]; then
    builder_message "updating the generated connect ip tables (security + blocker)"
    (cd $BUILD_HOME/connect &&
        go run ./security &&
        go run ./blocker)
    error_trap 'connect ip update'
fi

# Refresh the committed ur.io changelog sources while mmm is still on main.
# This is release synchronization, not site compilation: doing it here makes
# the generated source part of repository history before the web build consumes
# it. GITHUB_API_KEY raises the GitHub API limit for the release walk. Keep the
# generator strict so this runner can report a failed refresh, but do not lose a
# release over GitHub's API limit: the generator writes only after completing
# the walk, so its committed changelog.js + changelog-version.js remain a valid
# stand-in on failure and the later web build can safely consume them.
builder_message "updating the generated ur.io changelog"
(cd $WARP_HOME/mmm/ur.io &&
    CHANGELOG_STRICT=1 \
    GITHUB_TOKEN="$GITHUB_API_KEY" \
    node react/scripts/generate-changelog.mjs)
warn_trap 'ur.io changelog update failed; using the committed changelog as a stand-in'

# The install page's desktop downloads: the newest complete GitHub release
# (Windows MSI, Ubuntu daemon deb + AppImage) as of this run, i.e. the previous
# release, since this run's own desktop bundles are attached later. Same
# strictness and fallback as the changelog: the generator writes only on
# success, so the committed releases.js remains a valid stand-in.
builder_message "updating the generated ur.io desktop releases"
(cd $WARP_HOME/mmm/ur.io &&
    RELEASES_STRICT=1 \
    GITHUB_TOKEN="$GITHUB_API_KEY" \
    node react/scripts/generate-releases.mjs)
warn_trap 'ur.io releases update failed; using the committed releases as a stand-in'

# regenerate every app's strings from the shared localization store:
# localizations/keys/*.yaml -> android res/values*, apple Localizable.xcstrings,
# windows .resw, linux .po. Runs while every repo is still on main (localizations
# included, pulled above) so the tests below exercise the freshly generated
# strings. Unlike the connect IP tables this is not flag-gated — the store is the
# single source of truth, so the generated files must match it on every build.
# The codegen is byte-stable, so an unchanged store produces no commit below.
# The commit + push to each app repo happens after the release version is staged
# (the commit message carries it) and before the version branches are cut — see
# the matching localizations block below.
builder_message "generating app localizations from the shared store"
(cd $BUILD_HOME/localizations &&
    npm ci --silent &&
    URNETWORK_ROOT="$BUILD_HOME" npm run gen)
error_trap 'localizations codegen'

# Dependency vulnerability gate. osv-scanner reports what the OSV database knows
# about every module in the dependency graph; this fails the build on CRITICAL or
# HIGH findings and reports the rest.
#
# Severity resolution is deliberate, not incidental: the Go vulnerability database
# (GO-* records) carries NO severity field, so the scanner's own max_severity is
# empty for pure-Go findings and a naive "fail if any vulnerability" gate would
# block every build on known-LOW advisories. Each finding's GHSA alias does carry
# a rating, so severities come from the OSV API by alias. A finding whose severity
# cannot be resolved is reported and does NOT fail the build — an unknown rating is
# not evidence of a high one, and a network hiccup must not become a release veto.
# Set BUILD_SKIP_VULN_SCAN to skip (intended for offline/dev runs only).
scan_vulnerabilities () {
    scan_dir="$1"
    scan_out="$BUILD_HOME/.osv-$scan_dir.json"

    # rc 1 means "vulnerabilities found", which is not a scanner failure; any
    # other non-zero rc is (bad flags, unreadable tree, network down)
    osv-scanner scan source -r "$BUILD_HOME/$scan_dir" \
        --data-source native \
        --format json \
        --output-file "$scan_out" >/dev/null 2>&1
    scan_code=$?
    if [ $scan_code != 0 ] && [ $scan_code != 1 ]; then
        builder_message "error($scan_code): osv-scanner failed to scan $scan_dir"
        exit $scan_code
    fi

    # id<TAB>package<TAB>aliases, one line per finding
    findings=$(jq -r '
        .results[]?.packages[]? |
        .package.name as $pkg |
        .vulnerabilities[]? |
        [.id, $pkg, ((.aliases // []) | join(","))] | @tsv
    ' "$scan_out" 2>/dev/null | sort -u)

    if [ -z "$findings" ]; then
        builder_message "$scan_dir: no known dependency vulnerabilities."
        return 0
    fi

    # the resolved severities are accumulated in a file: the read loop below
    # runs in a subshell under a pipe, so variables set inside it would not
    # survive to the checks after it
    severities_file="$BUILD_HOME/.osv-$scan_dir.severities"
    rm -f "$severities_file"
    echo "$findings" | while IFS=$'\t' read -r vuln_id pkg vuln_aliases; do
        severity=""
        # the finding's own id first (npm/other ecosystems report GHSA ids
        # directly and those records carry the rating), then its aliases (a
        # GO-* record has no severity of its own but aliases a GHSA that does)
        for lookup_id in "$vuln_id" ${(s:,:)vuln_aliases}; do
            case "$lookup_id" in
                GHSA-*)
                    severity=$(curl -s --max-time 20 "https://api.osv.dev/v1/vulns/$lookup_id" |
                        jq -r '.database_specific.severity // empty' 2>/dev/null)
                    ;;
            esac
            if [ "$severity" ]; then
                break
            fi
        done
        if [ -z "$severity" ]; then
            severity="UNKNOWN"
        fi
        echo "$severity\t$vuln_id\t$pkg" >> "$severities_file"
    done

    blocking=$(grep -E '^(CRITICAL|HIGH)\t' "$severities_file" 2>/dev/null)
    reported=$(sort "$severities_file" 2>/dev/null)
    rm -f "$severities_file"

    builder_message "$scan_dir dependency vulnerabilities:\n$reported"

    if [ "$blocking" ]; then
        builder_message "error: $scan_dir has critical/high dependency vulnerabilities:\n$blocking"
        exit 1
    fi
    builder_message "$scan_dir: no critical or high dependency vulnerabilities."
}

if [ ! "$BUILD_SKIP_VULN_SCAN" ]; then
    builder_message "scanning dependencies for known vulnerabilities"
    if ! which osv-scanner > /dev/null; then
        builder_message "installing osv-scanner"
        go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest
        error_trap 'install osv-scanner'
        export PATH="$(go env GOPATH)/bin:$PATH"
    fi
    for scan_dir in warp sdk server; do
        scan_vulnerabilities "$scan_dir"
    done
    builder_message "dependency vulnerability scan passed."
fi

if [ "$BUILD_TEST" ]; then
    builder_message "Build all test candidate"

    # Bring up the local test environment (postgres + redis on a dedicated
    # loopback IP) for the duration of the tests. server/local/run-local.sh blocks
    # in the foreground and restores /etc/hosts + tears down the containers on
    # exit, so we background it, wait for the DBs to go healthy, run the tests,
    # then stop it. NOTE: the build user needs passwordless sudo — run-local.sh
    # edits /etc/hosts and adds a loopback alias.
    builder_message "starting local test environment"
    "$BUILD_HOME/server/local/run-local.sh" &
    RUN_LOCAL_PID=$!
    stop_local_env () {
        if [ "$RUN_LOCAL_PID" ]; then
            # Kill run-local's blocking child (`compose logs -f`) so its own trap
            # runs the teardown (compose down + restore /etc/hosts + drop alias),
            # and TERM the script itself; then wait for the teardown to finish.
            pkill -TERM -P "$RUN_LOCAL_PID" 2>/dev/null
            kill -TERM "$RUN_LOCAL_PID" 2>/dev/null
            wait "$RUN_LOCAL_PID" 2>/dev/null
            RUN_LOCAL_PID=""
        fi
    }
    # Safety net: stop the env even if a test fails and error_trap exits the build.
    # (Setting EXIT replaces the apple identity cleanup trap, so chain it here.)
    trap 'stop_local_env; cleanup_apple_identity' EXIT

    # Wait for the containers run-local.sh starts to become healthy (names are
    # fixed in that script). Bail if run-local dies early.
    local_env_up=""
    for i in {1..180}; do
        kill -0 "$RUN_LOCAL_PID" 2>/dev/null || break
        pg_health=`docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' urnetwork-local-pg 2>/dev/null`
        redis_health=`docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' urnetwork-local-redis 2>/dev/null`
        if [ "$pg_health" = healthy ] && [ "$redis_health" = healthy ]; then
            local_env_up=1
            break
        fi
        sleep 1
    done
    if [ ! "$local_env_up" ]; then
        builder_message "error: local test environment did not come up"
        exit 1
    fi
    builder_message "local test environment ready"

    for m in `find $BUILD_HOME -type d -d 1 -exec basename {} \;`; do
        if [[ -e "$BUILD_HOME/$m/test.sh" ]]; then
            (cd $BUILD_HOME/$m && ./test.sh)
            error_trap "$m tests"
            builder_message "$m tests passed."
        fi
    done

    # Tests done — stop the local environment and re-arm the apple identity
    # cleanup as the EXIT trap (a no-op when BUILD_APPLE_IDENTITY is unset).
    builder_message "stopping local test environment"
    stop_local_env
    trap cleanup_apple_identity EXIT

    builder_message "Build all test candidate passed. A version number can now be assigned."
fi


(cd $BUILD_HOME/warp/warpctl && make)
error_trap 'build bootstrap warpctl'
export PATH="$BUILD_HOME/warp/warpctl/build/darwin/arm64:$PATH"
if [[ ! `which warpctl` = "$BUILD_HOME/warp/warpctl/build/darwin/arm64/warpctl" ]]; then
    builder_message "Build warpctl is not first on the PATH ($(which warpctl))."
    exit 1
fi

warpctl stage version next release --message="$HOST build all"
error_trap 'warpctl stage version'


# note on versions
# WARP_VERSION follows the format <version>+<version_code>
#              This is because the version can contain contain a pre-release tag,
#              and the version code is semantically the build tag.
# However, most systems choke on the "+" character.
# EXTERNAL_WARP_VERSION converts the "+<version_code>" to "-<version_code>"
#                       to deploy to external systems, git and docker container repo.
# Within the binaries we still use the "+".


WARP_VERSION_BASE=$(warpctl ls version)
error_trap 'warpctl version'
export WARP_VERSION_BASE
WARP_VERSION_CODE=$(warpctl ls version-code)
error_trap 'warpctl version code'
export WARP_VERSION_CODE
GO_MOD_VERSION=`echo $WARP_VERSION_BASE | $BUILD_SED 's/\([^\.]*\).*/\1/'`
if [ $GO_MOD_VERSION = 0 ] || [ $GO_MOD_VERSION = 1 ]; then
    GO_MOD_SUFFIX=''
else
    GO_MOD_SUFFIX="/v${GO_MOD_VERSION}"
fi
export WARP_VERSION="${WARP_VERSION_BASE}+${WARP_VERSION_CODE}"
export EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}"
# Browser extension stores (Chrome Web Store and addons.mozilla.org) require a
# plain dotted version of 1-4 integers with no leading zeros and no suffix. They
# reject the "-<version_code>".
export EXTENSION_VERSION="$WARP_VERSION_BASE"


# rebuild warpctl with the `WARP_*` env vars so we have the binary properly versioned
(cd $BUILD_HOME/warp/warpctl && make)
error_trap 'build warpctl'


builder_message "Build all \`${EXTERNAL_WARP_VERSION}\`"


# push the connect IP table update (generated before the tests above) to connect
# main, stamped with the release version, before the version branches below are
# cut from it. Stage only the generated tables, never a blanket add.
if [ "$CONNECT_IP_UPDATE" ]; then
    (cd $BUILD_HOME/connect &&
        git add ip_security_cfaa_block.go ip_blocker_block.go &&
        if ! git diff --cached --quiet; then
            git commit -m "${EXTERNAL_WARP_VERSION} ip security and blocker update" &&
            git push
        fi)
    error_trap 'connect ip update push'
fi


# Push the changelog and the desktop releases generated before the tests to
# mmm main, stamped with the release version. Stage only their source files;
# the web build derives the public markdown and llms assets from these
# committed inputs.
(cd $WARP_HOME/mmm &&
    git add \
        ur.io/react/src/data/changelog.js \
        ur.io/react/src/data/changelog-version.js \
        ur.io/react/src/data/releases.js &&
    if ! git diff --cached --quiet; then
        git commit -m "${EXTERNAL_WARP_VERSION} ur.io changelog and releases update" &&
        git push
    fi)
error_trap 'ur.io changelog update push'


# push the regenerated localizations (generated before the tests above) to each
# app repo's main, stamped with the release version, before the version branches
# below are cut from them. Stage only the generated paths — never a blanket add:
# these are app repos, and `git add .` here would sweep any unrelated working-tree
# change into the release commit. Guarding on the *staged* diff also means a build
# where nothing changed pushes nothing.
(cd $BUILD_HOME/android &&
    git add 'app/app/src/main/res/values*/strings.xml' &&
    if ! git diff --cached --quiet; then
        git commit -m "${EXTERNAL_WARP_VERSION} localizations update" &&
        git push
    fi)
error_trap 'android localizations push'
(cd $BUILD_HOME/apple &&
    git add app/network/Shared/Resources/Localizable.xcstrings &&
    if ! git diff --cached --quiet; then
        git commit -m "${EXTERNAL_WARP_VERSION} localizations update" &&
        git push
    fi)
error_trap 'apple localizations push'
(cd $BUILD_HOME/windows &&
    git add app/src/App/Strings &&
    if ! git diff --cached --quiet; then
        git commit -m "${EXTERNAL_WARP_VERSION} localizations update" &&
        git push
    fi)
error_trap 'windows localizations push'
(cd $BUILD_HOME/linux &&
    git add app/po &&
    if ! git diff --cached --quiet; then
        git commit -m "${EXTERNAL_WARP_VERSION} localizations update" &&
        git push
    fi)
error_trap 'linux localizations push'


(cd $BUILD_HOME/connect && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'connect prepare version branch'
(cd $BUILD_HOME/sdk && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'sdk prepare version branch'
(cd $BUILD_HOME/android && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'android prepare version branch'
(cd $BUILD_HOME/apple && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'apple prepare version branch'
(cd $BUILD_HOME/windows && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'windows prepare version branch'
(cd $BUILD_HOME/linux && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'linux prepare version branch'
(cd $BUILD_HOME/sn && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'sn prepare version branch'
(cd $BUILD_HOME/server && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'server prepare version branch'
(cd $BUILD_HOME/web && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'web prepare branch'
(cd $BUILD_HOME/docs && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'docs prepare branch'
(cd $BUILD_HOME/warp && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'warp prepare branch'
(cd $BUILD_HOME/glog && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'glog prepare branch'
(cd $BUILD_HOME/proxy && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'proxy prepare branch'
(cd $BUILD_HOME/operator-proxy && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'operator-proxy prepare branch'
(cd $BUILD_HOME/userwireguard && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'userwireguard prepare branch'
(cd $BUILD_HOME/goidenticons && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'goidenticons prepare branch'
(cd $BUILD_HOME/extension && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'extension prepare branch'
(cd $BUILD_HOME/localizations && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'localizations prepare branch'


# apple branch, edit xcodeproject

(cd $BUILD_HOME/apple &&
    $BUILD_SED -i "s|\(MARKETING_VERSION *= *\).*;|\1${WARP_VERSION_BASE};|g" app/app.xcodeproj/project.pbxproj &&
    $BUILD_SED -i "s|\(CURRENT_PROJECT_VERSION *= *\).*;|\1${WARP_VERSION_CODE};|g" app/app.xcodeproj/project.pbxproj)
error_trap 'apple edit settings'

(cd $BUILD_HOME/android &&
    echo -n "
warp.version=$WARP_VERSION_BASE
warp.version_code=$WARP_VERSION_CODE
" > app/local.properties &&
    git add app/local.properties -f)
error_trap 'android edit settings'


# metadata -- THE STORE CHANGELOG
#
# What this replaced: a straight `cp pending.txt <version code>.txt`. pending.txt
# is a static one-liner that nobody has edited in months, so every changelog this
# repo has ever published is byte-identical -- 1025763520, 1025613560, 1025339670
# and 1025223880 all read "- Bug and performance fixes.". all/changelog.py builds
# the note out of the actual commits between the last release's submodule pins and
# the ones this release is about to tag. `--to worktree` because at this point in
# the run the new release is not committed or tagged yet: every submodule is
# sitting on the main HEAD it was pulled to, which is exactly what will be pinned.
#
# 500 CHARACTERS, and the generator does the trimming, not the store. fdroidserver
# sets char_limits['whatsNew'] = 500 and update.py slices `text[:limit]` with no
# warning and no error, so an over-long note is published cut mid-sentence and
# nothing anywhere says so. Play's cap is the same 500 and it rejects rather than
# truncates. The generator packs whole bullets inside that budget; nothing here
# can produce a half-sentence.
#
# pending.txt keeps its job. If somebody wrote a headline for this release it goes
# above the generated bullets (--lede); if it still holds the shipped placeholder
# text the generator ignores it and says so on stderr.
#
# warn_trap, not error_trap: a changelog is not worth losing a release over. The
# generator renders both artifacts in full before it opens either output file, so
# a failure of any kind -- no python3, GitHub unreachable, a rate limit -- leaves
# the tree untouched and the `-s` guard below falls back to exactly the old
# behaviour. GITHUB_API_KEY is already exported above for the ur.io changelog and
# raises the API limit here too; every repo walked is public, so it is optional.
#
# THE ABI-SPLIT FILENAMES are the other half of this, and they are why F-Droid
# shows no changelog for this app at all today. fdroiddata's
# com.bringyour.network.yml carries `VercodeOperation: ['%c + 2', '%c + 3']`, so
# F-Droid builds and indexes version codes base+2 and base+3, while this block was
# writing only <base>.txt. fdroidserver matches changelog files by EXACT filename
# -- update.py's `f == str(CurrentVersionCode) + '.txt'` and its
# `int(base) in [b["versionCode"] for b in builds]` -- so 42 of the 45 builds in
# that recipe have no changelog and f-droid.org renders none. Writing the same
# text under each offset fixes that with no fdroiddata change needed. The offsets
# are read from FDROID_VERSION_CODE_OFFSETS rather than hardcoded a third time,
# because fdroiddata owns them and a third ABI would silently break this again.
# default.txt is fdroidserver's documented fallback (update.py: used only when no
# version-specific entry was set, so it can never clobber a real one) and costs one
# more cp: if the offsets ever drift, F-Droid degrades to a present-but-generic
# note instead of a blank one.
FDROID_VERSION_CODE_OFFSETS="${FDROID_VERSION_CODE_OFFSETS:-0 2 3}"
BUILD_CHANGELOG_STORE="${TMPDIR:-/tmp}/urnetwork-changelog-${EXTERNAL_WARP_VERSION}.txt"
# exported: github_finalize_release() below reads it, long after this block
export BUILD_CHANGELOG_FULL="${TMPDIR:-/tmp}/urnetwork-changelog-${EXTERNAL_WARP_VERSION}.md"
# IN THE REPO, not in TMPDIR, unlike the two above. The android note has a home
# already (metadata/en-US/changelogs, which F-Droid reads off this repository);
# the apple, windows and linux notes had nowhere to be, and a note that exists
# only in a Slack message during the run is a note nobody can find on the day
# they submit. `git add .` below carries the whole tree into the release commit
# and the release tag, so every published version can be traced to the exact
# text that was offered for it.
# One directory, one file per audience, every file prefixed with the version code
# so the directory is a browsable per-release archive that sorts chronologically --
# the same key metadata/en-US/changelogs/<versionCode>.txt already uses. Exported:
# github_finalize_release() links the _Full.md and inlines the _Simple.txt.
export BUILD_NOTES_DIR="$BUILD_HOME/changelogs"
export BUILD_NOTES_PREFIX="$WARP_VERSION_CODE"
rm -f "$BUILD_CHANGELOG_STORE" "$BUILD_CHANGELOG_FULL"
builder_message "generating the changelog for \`${EXTERNAL_WARP_VERSION}\`"
python3 "$BUILD_HOME/all/changelog.py" \
    --repo "$BUILD_HOME" \
    --to worktree \
    --to-label "v${EXTERNAL_WARP_VERSION}" \
    --lede "$BUILD_HOME/metadata/en-US/changelogs/pending.txt" \
    --store-out "$BUILD_CHANGELOG_STORE" \
    --notes-dir "$BUILD_NOTES_DIR" \
    --notes-prefix "$BUILD_NOTES_PREFIX" \
    --full-out "$BUILD_CHANGELOG_FULL"
warn_trap 'generate changelog'
if [ ! -s "$BUILD_CHANGELOG_STORE" ] && [ -e "$BUILD_HOME/metadata/en-US/changelogs/pending.txt" ]; then
    builder_message "warning: changelog generation produced no store note; falling back to metadata/en-US/changelogs/pending.txt. Build will continue."
    cp "$BUILD_HOME/metadata/en-US/changelogs/pending.txt" "$BUILD_CHANGELOG_STORE"
fi
if [ -s "$BUILD_CHANGELOG_STORE" ]; then
    # ${=...} so zsh word-splits the offset list; zsh does not split on
    # whitespace by default the way bash does.
    for changelog_offset in ${=FDROID_VERSION_CODE_OFFSETS}; do
        cp \
            "$BUILD_CHANGELOG_STORE" \
            "$BUILD_HOME/metadata/en-US/changelogs/$((WARP_VERSION_CODE+changelog_offset)).txt"
    done
    cp \
        "$BUILD_CHANGELOG_STORE" \
        "$BUILD_HOME/metadata/en-US/changelogs/default.txt"
    # $( ) rather than backticks: this nests a command substitution inside a
    # double-quoted string that already contains backticks (the Slack code
    # fence), and backticks cannot nest there. `wc -m` counts CHARACTERS, which
    # is the unit both stores enforce -- `wc -c` would under-report the moment a
    # commit subject carries an em dash or a non-ASCII name.
    builder_message "store changelog ($(wc -m < "$BUILD_CHANGELOG_STORE" | tr -d ' ')/500 chars):
\`\`\`
$(cat "$BUILD_CHANGELOG_STORE")
\`\`\`"
fi


# metadata -- THE OTHER THREE STOREFRONTS
#
# The block above handles android, and android alone: 500 characters of plain
# text, written under the offset filenames F-Droid matches by. The other three
# stores are not that. Their limits are 4000 (App Store Connect), 1500 (Partner
# Center) and none at all (AppStream, which is not even text -- it is markup
# inside urnetwork/linux's metainfo XML, and appstreamcli gates that repo's build
# on it). changelog.py --notes-dir writes one file per storefront, each in
# its own format at its own limit; see AUDIENCES in all/changelog.py for the
# citation behind every one of those numbers.
#
# Each file is that app's own changes first, then the shared sdk/connect changes
# after a boundary marker, so whoever submits can delete the tail in one motion
# if that store's reviewers do not need it. changelogs/README.md says which
# field each file is pasted into.
#
# NOT UNDER metadata/. That tree belongs to F-Droid: fdroidserver globs
# `build/[A-Za-z]*/metadata/[a-z][a-z]*` and then os.walk()s it, matching
# changelog files by bare filename at any depth, so an apple or windows note
# dropped in there would either overwrite the real en-US note or invent a locale
# in the F-Droid index. changelogs/ is a sibling and is invisible to that glob.
#
# NO trap on this block. The generator already ran under warn_trap above; what
# is left is cat/wc/cp over files it either produced or did not, and every one
# of them is guarded. A missing note is reported and skipped.
if [ -d "$BUILD_NOTES_DIR" ]; then
    for store_note in \
        "Android.txt:Play Console -> release notes (pasted by hand), 500 chars. The unattended F-Droid copy is under metadata/en-US/changelogs/" \
        "Apple.txt:App Store Connect -> What's New in This Version (iOS and macOS), 4000 chars" \
        "Windows.txt:Partner Center -> Store listings -> What's new in this version, 1500 chars" \
        "Linux.xml:urnetwork/linux metainfo <release><description>, AppStream XML"
    do
        # ${x%%:*} / ${x#*:} rather than `cut`: no subprocess, and the field is
        # a path that must not be word-split.
        store_note_path="$BUILD_NOTES_DIR/${BUILD_NOTES_PREFIX}_${store_note%%:*}"
        if [ ! -s "$store_note_path" ]; then
            builder_message "warning: no store note at ${store_note%%:*}. Build will continue."
            continue
        fi
        builder_message "store note ${store_note%%:*} ($(wc -m < "$store_note_path" | tr -d ' ') chars) -- ${store_note#*:}:
\`\`\`
$(cat "$store_note_path")
\`\`\`"
    done

    # THE LINUX FRAGMENT HAS TO TRAVEL, and it has to travel in the repository
    # rather than as a build flag. The metainfo is installed by exactly one
    # channel -- the Flatpak -- and Flathub builds that manifest on ITS OWN
    # infrastructure, where nothing in this pipeline runs. A meson option passed
    # from all/linux/build-arch.sh would therefore be absent on the one channel
    # that ships the file; packaging/flatpak/com.bringyour.network.yml already
    # documents that trap for -Dapp_version. So the fragment is copied INTO the
    # linux checkout, where git_commit() (which does `git add .` on the version
    # branch) carries it to urnetwork/linux the same way app/po already travels.
    #
    # ONLY IF THAT REPO HAS OPTED IN. The handshake is the destination file
    # already existing: urnetwork/linux commits it once, together with the meson
    # fs.read() that substitutes it into the metainfo template, and from then on
    # every release refreshes it. Until then this says so and changes nothing --
    # dropping an unreferenced file into another repository's tree is not this
    # script's call to make.
    linux_release_description="$BUILD_HOME/linux/app/packaging/release-description.xml"
    if [ -s "$BUILD_NOTES_DIR/${BUILD_NOTES_PREFIX}_Linux.xml" ]; then
        if [ -e "$linux_release_description" ]; then
            cp "$BUILD_NOTES_DIR/${BUILD_NOTES_PREFIX}_Linux.xml" "$linux_release_description"
            builder_message "linux release description staged into app/packaging/release-description.xml"
        else
            builder_message "note: urnetwork/linux has no app/packaging/release-description.xml, so the generated AppStream release description was not staged there. It is in changelogs/${BUILD_NOTES_PREFIX}_Linux.xml in this repo, ready for whoever wires the meson side. Build will continue."
        fi
    fi
fi


go_mod_edit_module () {
    go mod edit -module=$1${GO_MOD_SUFFIX} &&
    go_edit_require $1
}

go_mod_edit_require () {
    go mod edit -dropreplace=$1 &&
    go mod edit -droprequire=$1 &&
    go mod edit -require=$1${GO_MOD_SUFFIX}@v${EXTERNAL_WARP_VERSION} &&
    go_edit_require $1
}

go_edit_require () {
    find . \( -iname '*.go' -o -iname 'Makefile' \) -type f -exec $BUILD_SED -i "s|\"$1\"|\"$1${GO_MOD_SUFFIX}\"|g" {} \;
}

go_edit_require_subpackages () {
    find . \( -iname '*.go' -o -iname 'Makefile' \) -type f -exec $BUILD_SED -i "/\/v[0-9]\+/! s|\"$1\([^\"]*\)\"|\"$1${GO_MOD_SUFFIX}\1\"|g" {} \;
}

go_mod_fork_prepare () {
    if [ $GO_MOD_VERSION != 0 ] && [ $GO_MOD_VERSION != 1 ]; then
        local temp
        temp=`mktemp -d` || return $?
        for f in *; do
            if [ ! -e "$f/go.mod" ] && [ ! -e "$f/v${GO_MOD_VERSION}/go.mod" ]; then
                fork_dir=1
                for t in "$@"; do
                    if [[ "$f" =~ "^($t)\$" ]]; then
                        fork_dir=
                    fi
                done
                if [[ "$fork_dir" ]]; then
                    mv "$f" "$temp" || return $?
                fi
            fi
        done
        $BUILD_SED -i '/^retract/d' "$temp/go.mod" || return $?
        mv "$temp" v${GO_MOD_VERSION} || return $?
        # Arguments naming a top-level tree leave that tree beside the versioned
        # module (the original behavior above). Arguments containing a slash are
        # paths within a moved tree; carve those back out after the fork. This is
        # for repository data that belongs in the release tag but not in the Go
        # module zip.
        for t in "$@"; do
            if [[ "$t" == */* ]] && [ -e "v${GO_MOD_VERSION}/$t" ]; then
                mkdir -p "${t%/*}" || return $?
                mv "v${GO_MOD_VERSION}/$t" "$t" || return $?
            fi
        done
    fi
}

go_mod_fork_tidy () {
    if [ $GO_MOD_VERSION != 0 ] && [ $GO_MOD_VERSION != 1 ]; then
        # the go go.sum needs to be updated
        (cd v${GO_MOD_VERSION} && go mod tidy && go get -t ./...)
    fi
}

go_mod_fork () {
    go_mod_fork_prepare "$@" &&
    go_mod_fork_tidy
}

go_mod_fork_update () {
    if [ $GO_MOD_VERSION != 0 ] && [ $GO_MOD_VERSION != 1 ]; then
        # the go go.sum needs to be updated for the forked mods
        for f in *; do
            if [ -e "$f/go.mod" ] && [[ "$f" =~ "^($1)\$" ]]; then
                (cd $f && go mod tidy && go get -t ./...)
            fi
        done
    fi
}


npm_edit_module () {
    jq --arg p "$1" --arg v "$EXTERNAL_WARP_VERSION" '.dependencies.[$p] = $v' package.json > package.json.2 && mv package.json.2 package.json
    jq --arg p "$1" --arg v "$EXTERNAL_WARP_VERSION" '.packages.[""].dependencies.[$p] = $v' package-lock.json > package-lock.json.2 && mv package-lock.json.2 package-lock.json
    jq --arg p "$1" 'del(.packages.["node_modules/" + $p])' package-lock.json > package-lock.json.2 && mv package-lock.json.2 package-lock.json
}

# Set this fork's own package version. Defaults to EXTERNAL_WARP_VERSION (a valid
# npm pre-release version). Pass an explicit version for targets like the browser
# extension whose manifest must use the bare, store-compatible EXTENSION_VERSION.
npm_fork_version () {
    local v="${1:-$EXTERNAL_WARP_VERSION}"
    jq --arg v "$v" '.version = $v' package.json > package.json.2 && mv package.json.2 package.json
    jq --arg v "$v" '.version = $v' package-lock.json > package-lock.json.2 && mv package-lock.json.2 package-lock.json
    jq --arg v "$v" '.packages.[""].version = $v' package-lock.json > package-lock.json.2 && mv package-lock.json.2 package-lock.json
    # update package-lock.json
    npm install
}

npm_fork () {
    npm_fork_version "$EXTERNAL_WARP_VERSION"
}

npm_fork_update () {
    for f in *; do
        if [ -e "$f/package.json" ] && [[ "$f" =~ "^($1)\$" ]]; then
            jq --arg v "$EXTERNAL_WARP_VERSION" '.version = $v' $f/package.json > $f/package.json.2 && mv $f/package.json.2 $f/package.json
            jq --arg v "$EXTERNAL_WARP_VERSION" '.version = $v' $f/package-lock.json > $f/package-lock.json.2 && mv $f/package-lock.json.2 $f/package-lock.json
            jq --arg v "$EXTERNAL_WARP_VERSION" '.packages.[""].version = $v' $f/package-lock.json > $f/package-lock.json.2 && mv $f/package-lock.json.2 $f/package-lock.json
            # update package-lock.json
            (cd $f && npm install)
        fi
    done
}

# note npm publishing requires a build unlike go publishing, which just requires the git tag
npm_publish () {
    if [ -e "Makefile" ]; then
        make || return $?
    else
        npm ci && npm run build --if-present || return $?
    fi
    npm publish --tag nightly
}


git_commit () {
    git tag -d v${EXTERNAL_WARP_VERSION}
    git add . &&
    if ! (git diff --quiet && git diff --cached --quiet); then
        git commit -m "${EXTERNAL_WARP_VERSION}" &&
        git push -u origin v${EXTERNAL_WARP_VERSION}
    fi
}

# Create and push the annotated release tag for the current version.
#
# A version is published at most once, so a tag that already exists on origin means
# something is wrong (a re-run, or a version-code collision). Fail loudly instead of
# silently overwriting it -- this also avoids fighting a published immutable-release
# tag lock.
#
# Pass "recreate" to deliberately move the tag to the current commit. This is only
# used where the same version is re-tagged within a single run (the sdk re-tags
# after its fork/lock files are regenerated). The caller must drop the local tag
# first, which git_commit already does via `git tag -d`.
git_tag () {
    local tag="v${EXTERNAL_WARP_VERSION}"
    if [ "$1" = "recreate" ]; then
        git push --delete origin "refs/tags/$tag" &&
        git tag -a "$tag" -m "${EXTERNAL_WARP_VERSION}" &&
        git push origin "refs/tags/$tag"
    elif git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then
        builder_message "error: tag $tag already exists on origin; refusing to overwrite (a version is published only once). Pass 'recreate' to move it intentionally."
        return 1
    else
        git tag -a "$tag" -m "${EXTERNAL_WARP_VERSION}" &&
        git push origin "refs/tags/$tag"
    fi
}


(cd $BUILD_HOME/glog &&
    go_mod_edit_module github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/glog)
error_trap 'glog edit'


(cd $BUILD_HOME/glog && 
    git_commit &&
    git_tag)
error_trap 'glog push branch'


(cd $BUILD_HOME/connect &&
    go_mod_edit_module github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go mod edit -dropretract '[v0.0.1, v0.1.13]' &&
    go_mod_fork 'api')
error_trap 'connect edit'

(cd $BUILD_HOME/connect && 
    git_commit &&
    git_tag)
error_trap 'connect push branch'


(cd $BUILD_HOME/userwireguard &&
    go_mod_edit_module github.com/urnetwork/userwireguard &&
    go_edit_require_subpackages github.com/urnetwork/userwireguard)
error_trap 'userwireguard edit'

(cd $BUILD_HOME/userwireguard &&
    git_commit &&
    git_tag)
error_trap 'userwireguard push branch'


(cd $BUILD_HOME/goidenticons &&
    go_mod_edit_module github.com/urnetwork/goidenticons &&
    go_edit_require_subpackages github.com/urnetwork/goidenticons)
error_trap 'goidenticons edit'

(cd $BUILD_HOME/goidenticons &&
    git_commit &&
    git_tag)
error_trap 'goidenticons push branch'


(cd $BUILD_HOME/proxy &&
    go_mod_edit_module github.com/urnetwork/proxy &&
    go_edit_require_subpackages github.com/urnetwork/proxy &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/userwireguard &&
    go_edit_require_subpackages github.com/urnetwork/userwireguard &&
    go_mod_fork)
error_trap 'proxy edit'

(cd $BUILD_HOME/proxy && 
    git_commit &&
    git_tag)
error_trap 'proxy push branch'


(cd $BUILD_HOME/operator-proxy &&
    go_mod_edit_module github.com/urnetwork/operator-proxy &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/operator-proxy &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go_mod_fork)
error_trap 'operator-proxy edit'

(cd $BUILD_HOME/operator-proxy &&
    git_commit &&
    git_tag)
error_trap 'operator-proxy push branch'


(cd $BUILD_HOME/sdk/build &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/goidenticons &&
    go_mod_edit_require github.com/urnetwork/sdk)
error_trap 'sdk build edit'

(cd $BUILD_HOME/sdk/cgo &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/goidenticons &&
    go_mod_edit_require github.com/urnetwork/sdk)
error_trap 'sdk cgo edit'

(cd $BUILD_HOME/sdk/js &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/sdk)
error_trap 'sdk js edit'

# NOTE the sdk is a gomobile/cgo-bound *library*, so there is no `main` to carry
# the version the way the sn cli tools do. sdk.Version is a `const`, which the
# linker's -X cannot patch (it only rewrites writable `var`s) -- so instead of a
# no-op -X we inject the version straight into the source below. This is
# deliberate and fork-proof: baking it into the source sidesteps the module
# rename (github.com/urnetwork/sdk -> .../sdk/v<N>) that would silently break a
# hardcoded -X <pkgpath>.Version after the fork. Every sdk build path (android
# aar, apple xcframework, cgo desktop) picks the version up from this one edit.
(cd $BUILD_HOME/sdk &&
    go_mod_edit_module github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/goidenticons &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/goidenticons &&
    $BUILD_SED -i "s/Version string = \"\"/Version string = \"${WARP_VERSION}\"/g" sdk.go &&
    go_mod_fork 'build' 'cgo' 'js')
error_trap 'sdk edit'

(cd $BUILD_HOME/sdk &&
    git_commit &&
    git_tag &&
    go_mod_fork_update 'build' &&
    go_mod_fork_update 'cgo' &&
    go_mod_fork_update 'js' &&
    npm_fork_update 'js' &&
    git_commit &&
    # re-tag the same version now that the fork/lock files above were regenerated;
    # "recreate" intentionally moves the existing tag instead of failing on the duplicate
    git_tag recreate)
error_trap 'sdk push branch'

(cd $BUILD_HOME/sdk/js &&
    npm_publish)
error_trap 'js-sdk publish'


(cd $BUILD_HOME/sn &&
    go_mod_edit_module github.com/urfoundation/sn &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urnetwork/goidenticons &&
    go_mod_edit_require github.com/urnetwork/operator-proxy &&
    go_mod_edit_require github.com/urnetwork/server &&
    go_mod_edit_require github.com/urnetwork/proxy &&
    go_mod_edit_require github.com/urnetwork/userwireguard &&
    go_edit_require_subpackages github.com/urfoundation/sn &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_edit_require_subpackages github.com/urnetwork/goidenticons &&
    go_edit_require_subpackages github.com/urnetwork/operator-proxy &&
    go_edit_require_subpackages github.com/urnetwork/server &&
    go_edit_require_subpackages github.com/urnetwork/proxy &&
    go_edit_require_subpackages github.com/urnetwork/userwireguard &&
    # sim-testnet now imports server, while server imports sn. Publish a
    # bootstrap sn tag before tidying so server can resolve this same-version
    # module cycle; sn is tidied and re-tagged after server is available below.
    go_mod_fork_prepare)
error_trap 'sn edit'

(cd $BUILD_HOME/sn &&
    git_commit &&
    git_tag)
error_trap 'sn bootstrap push branch'


(cd $BUILD_HOME/server &&
    go_mod_edit_module github.com/urnetwork/server &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/glog &&
    go_mod_edit_require github.com/urnetwork/goidenticons &&
    go_mod_edit_require github.com/urnetwork/operator-proxy &&
    go_mod_edit_require github.com/urnetwork/proxy &&
    go_mod_edit_require github.com/urnetwork/userwireguard &&
    go_mod_edit_require github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urfoundation/sn &&
    go_edit_require_subpackages github.com/urnetwork/server &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/glog &&
    go_edit_require_subpackages github.com/urnetwork/goidenticons &&
    go_edit_require_subpackages github.com/urnetwork/operator-proxy &&
    go_edit_require_subpackages github.com/urnetwork/proxy &&
    go_edit_require_subpackages github.com/urnetwork/userwireguard &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_edit_require_subpackages github.com/urfoundation/sn &&
    # The committed sim-latency baseline is an immutable 500+ MiB evidence
    # dataset, not a server package or runtime asset. Keep it beside the
    # versioned module on the release branch so it remains in the Git tag for
    # audit/reproduction without entering the Go module's source archive.
    # Otherwise server/v20xx exceeds Go's 500 MiB module-zip limit and cannot be
    # resolved by sn (or any other module consumer).
    go_mod_fork 'connect/sim-latency/baseline')
error_trap 'server edit'

(cd $BUILD_HOME/server &&
    git_commit &&
    git_tag)
error_trap 'server push branch'


# Complete the half of the sn/server module cycle deferred above. Moving the sn
# tag is intentional and mirrors the sdk's post-generation re-tag: server now
# exists at this version, so tidy can resolve the full sim-testnet dependency
# graph and the final tag contains the regenerated go.mod/go.sum.
(cd $BUILD_HOME/sn &&
    go_mod_fork_tidy &&
    git_commit &&
    git_tag recreate)
error_trap 'sn finalize branch'


(cd $BUILD_HOME/android && 
    git_commit &&
    git_tag)
error_trap 'android push branch'


(cd $BUILD_HOME/apple &&
    git_commit &&
    git_tag)
error_trap 'apple push branch'


(cd $BUILD_HOME/windows &&
    git_commit &&
    git_tag)
error_trap 'windows push branch'


(cd $BUILD_HOME/linux &&
    git_commit &&
    git_tag)
error_trap 'linux push branch'


(cd $BUILD_HOME/web &&
    git_commit &&
    git_tag)
error_trap 'web push branch'


(cd $BUILD_HOME/docs && 
    git_commit &&
    git_tag)
error_trap 'docs push branch'


(cd $BUILD_HOME/warp && 
    git_commit &&
    git_tag)
error_trap 'warp push branch'


(cd $BUILD_HOME/localizations &&
    npm_fork &&
    git_commit &&
    git_tag)
error_trap 'localizations edit'

(cd $BUILD_HOME/localizations &&
    npm_publish)
error_trap 'localizations push branch'


# give npm a bit of time to ingest the latest packages before we link against them
sleep 30


(cd $BUILD_HOME/extension &&
    npm_edit_module @urnetwork/localizations &&
    npm_edit_module @urnetwork/sdk-js &&
    npm_fork_version "$EXTENSION_VERSION")
error_trap 'extension edit'

(cd $BUILD_HOME/extension && 
    git_commit &&
    git_tag)
error_trap 'extension push branch'


(cd $BUILD_HOME &&
    git add . &&
    git commit -m "${EXTERNAL_WARP_VERSION}" &&
    (git push || (git pull --rebase && git push)) &&
    git_tag)
error_trap 'push branch'
# version code variants for the github flavor
(cd $BUILD_HOME &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+2))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    git_tag)
error_trap 'push +2 branch'
(cd $BUILD_HOME &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+3))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    git_tag)
error_trap 'push +3 branch'


# Build release

github_create_draft_release () {
    if [ "$1" ]; then
        PRE_RELEASE=true
    else
        PRE_RELEASE=false
    fi
    GITHUB_RELEASE=`$BUILD_CURL \
        -X POST \
        -H 'Accept: application/vnd.github+json' \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        https://api.github.com/repos/urnetwork/build/releases \
        -d "{\"tag_name\":\"v${EXTERNAL_WARP_VERSION}\",\"name\":\"v${EXTERNAL_WARP_VERSION}\",\"body\":\"v${EXTERNAL_WARP_VERSION}\",\"draft\":true,\"prerelease\":$PRE_RELEASE,\"generate_release_notes\":false}"`
    error_trap 'github create release'
    GITHUB_RELEASE_ID=`echo "$GITHUB_RELEASE" | jq -r .id`
    GITHUB_UPLOAD_URL="https://uploads.github.com/repos/urnetwork/build/releases/$GITHUB_RELEASE_ID/assets"
    echo "github upload to $GITHUB_UPLOAD_URL"
    VIRUSTOTAL_ARTIFACTS=()
}

github_release_upload () {
    virustotal "$1" "$2"

    GITHUB_UPLOAD=`$BUILD_CURL \
        -X POST \
        -H 'Accept: application/vnd.github+json' \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        -H 'Content-Type: application/octet-stream' \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        "$GITHUB_UPLOAD_URL?name=$1" \
        --data-binary "@$2"`
    error_trap "github release upload $1"
}

# virustotal analysis is best-effort context for the release notes: a
# malicious/suspicious VERDICT is fatal (virustotal_verify exits), but failing
# to GET a verdict — api quota (429 QuotaExceededError), transport errors, or
# analysis latency — must not block the release. Once the quota trips, later
# artifacts skip the scan entirely instead of each burning the retry window.
VIRUSTOTAL_QUOTA_EXCEEDED=""
virustotal () {
    SHA256=`shasum -a 256 "$2" | awk '{ print $1 }'`

    if [ "$VIRUSTOTAL_API_KEY" ] && [ ! "$VIRUSTOTAL_QUOTA_EXCEEDED" ]; then
        VIRUSTOTAL_PREPARE_UPLOAD=`$BUILD_CURL \
            -H 'Accept: application/json' \
            -H "x-apikey: $VIRUSTOTAL_API_KEY" \
            https://www.virustotal.com/api/v3/files/upload_url`
        VIRUSTOTAL_UPLOAD_URL=`echo "$VIRUSTOTAL_PREPARE_UPLOAD" | jq -r .data`
        if [ ! "$VIRUSTOTAL_UPLOAD_URL" ] || [ "$VIRUSTOTAL_UPLOAD_URL" = "null" ]; then
            virustotal_unavailable "$1" "prepare upload failed ($VIRUSTOTAL_PREPARE_UPLOAD)"
            return
        fi
        VIRUSTOTAL_UPLOAD=`$BUILD_CURL \
            -X POST \
            -H 'Accept: application/json' \
            -H 'Content-Type: multipart/form-data' \
            -H "x-apikey: $VIRUSTOTAL_API_KEY" \
            "$VIRUSTOTAL_UPLOAD_URL" \
            -F "file=@$2"`
        VIRUSTOTAL_ID=`echo "$VIRUSTOTAL_UPLOAD" | jq -r .data.id`
        # FIXME if the same file is uploaded multiple times, the VIRUSTOTAL_ID will need to be pulled from a different field (fix)
        if [ ! "$VIRUSTOTAL_ID" ] || [ "$VIRUSTOTAL_ID" = "null" ]; then
            virustotal_unavailable "$1" "upload failed ($VIRUSTOTAL_UPLOAD)"
            return
        fi
        echo "virustotal analysis https://www.virustotal.com/gui/file/$SHA256"
        if virustotal_verify "$1" "$VIRUSTOTAL_ID"; then
            VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${EXTERNAL_WARP_VERSION}/$1)|\`$SHA256\`|[ok](https://www.virustotal.com/gui/file/$SHA256)|")
        else
            virustotal_unavailable "$1" "no verdict"
        fi
    else
        VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${EXTERNAL_WARP_VERSION}/$1)|\`$SHA256\`|not submitted|")
    fi
}

virustotal_unavailable () {
    builder_message "warning: virustotal $2 for $1; continuing without a verdict. Build will continue."
    VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${EXTERNAL_WARP_VERSION}/$1)|\`$SHA256\`|not analyzed|")
}

virustotal_verify () {
    for i in {0..90}; do
        VIRUSTOTAL_ANALYSIS=`$BUILD_CURL \
            -H 'Accept: application/json' \
            -H "x-apikey: $VIRUSTOTAL_API_KEY" \
            "https://www.virustotal.com/api/v3/analyses/$2"`
        VIRUSTOTAL_ANALYSIS_STATUS=`echo "$VIRUSTOTAL_ANALYSIS" | jq -r .data.attributes.status`
        if [ "$VIRUSTOTAL_ANALYSIS_STATUS" = "completed" ]; then
            VIRUSTOTAL_ANALYSIS_STATS=`echo "$VIRUSTOTAL_ANALYSIS" | jq -r .data.attributes.stats`
            if [ "$VIRUSTOTAL_ANALYSIS_STATS" != "" ]; then
                if [ `echo "$VIRUSTOTAL_ANALYSIS_STATS" | jq '[.malicious, .suspicious] | add'` = 0 ]; then
                    echo "virustotal analysis $1 ok ($VIRUSTOTAL_ANALYSIS_STATS)"
                    return
                else
                    builder_message "virustotal analysis $1 failed: \`\`\`$VIRUSTOTAL_ANALYSIS_STATS\`\`\`"
                    exit 1
                fi
            else
                echo "virustotal analysis $1 complete, waiting for stats ($VIRUSTOTAL_ANALYSIS_STATUS) ..."
            fi
        elif [ "$VIRUSTOTAL_ANALYSIS_STATUS" = "null" ]; then
            echo "virustotal analysis $1 ($2) unknown result ($VIRUSTOTAL_ANALYSIS) ..."
            # quota exhaustion never resolves within this run; stop retrying
            # here and skip the scan for every later artifact
            if echo "$VIRUSTOTAL_ANALYSIS" | grep -q QuotaExceededError; then
                VIRUSTOTAL_QUOTA_EXCEEDED=1
                return 1
            fi
        else
            echo "virustotal analysis $1 waiting for result ($VIRUSTOTAL_ANALYSIS_STATUS) ..."
        fi
        sleep 10
    done
    # no verdict is a warning (the caller records "not analyzed"); only a
    # malicious/suspicious verdict above is fatal
    return 1
}

github_create_release () {
    if [ "$1" ]; then
        PRE_RELEASE=true
        HEADER="This is an architecture-specific release of v${EXTERNAL_WARP_VERSION%?}0"
    else
        PRE_RELEASE=false
        HEADER="\"$(shuf -n 1 $BUILD_HOME/all/release-color.txt) $(shuf -n 1 $BUILD_HOME/all/release-texture.txt) $(shuf -n 1 $BUILD_HOME/all/release-mineral.txt)\""
    fi

    RELEASE_BODY="v${EXTERNAL_WARP_VERSION}

${HEADER}

|Asset|SHA256|VirusTotal analysis|
|--------|------|------------------|"
    for a in $VIRUSTOTAL_ARTIFACTS; do
        RELEASE_BODY="$RELEASE_BODY
$a"
    done

    # THE FULL CHANGELOG, and this is the only place it goes.
    #
    # Every commit in every submodule since the previous release, grouped by
    # component, with bodies and links -- the thing that cannot fit in the
    # 500-character store note. The GitHub release body is where it belongs: it
    # is the artifact Obtainium users and anyone following releases already read,
    # and it is public, unlike mmm/ur.io.
    #
    # BASE RELEASE ONLY ($1 is set for the +2/+3 architecture-specific releases).
    # Those are the same release re-tagged per ABI -- their body already says so
    # -- and repeating tens of kilobytes three times is noise that would also
    # triple the exposure to the size limit below.
    #
    # THAT LIMIT IS A RELEASE-LOSING ONE, which is why it is checked rather than
    # assumed. The API answers 422 "body is too long (maximum is 125000
    # characters)", $BUILD_CURL carries --fail-with-body so any HTTP >= 400 exits
    # non-zero, and the error_trap below then exits the run -- AFTER every artifact
    # has already been uploaded. changelog.py budgets itself to 120000 by default;
    # this re-checks against what the header and the VirusTotal table actually
    # took, and drops the section with a warning rather than risking the release.
    #
    # Set BUILD_RELEASE_BODY_CHANGELOG= to publish without it (see README): the
    # ur.io site changelog is generated by walking these release bodies from a
    # repo this one cannot see, so this is the kill switch if that walk ever
    # objects to the longer body.
    if [ ! "$1" ] && [ "${BUILD_RELEASE_BODY_CHANGELOG:-1}" ] && [ -s "$BUILD_CHANGELOG_FULL" ]; then
        RELEASE_CHANGELOG=`cat "$BUILD_CHANGELOG_FULL"`
        # THE LINK FIRST, and it resolves because of ordering, not luck: the
        # `git add . && git commit && git_tag` on $BUILD_HOME above ran before
        # github_create_draft_release(), so v${EXTERNAL_WARP_VERSION} already
        # exists and already contains changelogs/. Pinned to the tag rather than
        # to a branch so the link keeps showing THIS release's changelog forever.
        RELEASE_BODY="$RELEASE_BODY

---

Full changelog: [changelogs/${BUILD_NOTES_PREFIX}_Full.md](https://github.com/urnetwork/build/blob/v${EXTERNAL_WARP_VERSION}/changelogs/${BUILD_NOTES_PREFIX}_Full.md) -- every commit in every component since the previous release. Per-store notes for Play, App Store Connect, Partner Center and AppStream are in [that same directory](https://github.com/urnetwork/build/tree/v${EXTERNAL_WARP_VERSION}/changelogs)."
        # THE SHORT VIEW INLINE, so the body still says something on its own.
        RELEASE_NOTE_SIMPLE="$BUILD_NOTES_DIR/${BUILD_NOTES_PREFIX}_Simple.txt"
        if [ -s "$RELEASE_NOTE_SIMPLE" ]; then
            RELEASE_BODY="$RELEASE_BODY

$(cat "$RELEASE_NOTE_SIMPLE")"
        fi
        # THE FULL TEXT STAYS, FOLDED. ur.io's site changelog is generated by
        # walking these release bodies (`node react/scripts/generate-changelog.mjs`,
        # near the top of this script) from a repo this one cannot see, so
        # replacing the text with a link would quietly starve a public website of
        # its content. <details> collapses it in the GitHub UI -- uncluttered to
        # read, still present to scrape. Drop the fold once someone has confirmed
        # what that generator actually reads; BUILD_RELEASE_BODY_CHANGELOG= turns
        # this whole section off in the meantime.
        #
        # THE SIZE CHECK IS STILL A RELEASE-LOSING ONE. The API answers 422 above
        # 125000 characters and $BUILD_CURL --fail-with-body would exit the run
        # AFTER every artifact has been uploaded. Now that the link carries the
        # changelog on its own, an over-budget body drops only the fold, and the
        # reader still gets there in one click -- which is what makes dropping it
        # safe rather than lossy.
        if [ $((${#RELEASE_BODY} + ${#RELEASE_CHANGELOG} + 128)) -le 124000 ]; then
            RELEASE_BODY="$RELEASE_BODY

<details><summary>Full changelog</summary>

$RELEASE_CHANGELOG

</details>"
        else
            builder_message "note: the full changelog (${#RELEASE_CHANGELOG} chars) does not fit the release body next to the artifact table, so the body links to it instead of embedding it. Build will continue."
        fi
    fi

    GITHUB_RELEASE=`$BUILD_CURL \
        -X PATCH \
        -H 'Accept: application/vnd.github+json' \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        "https://api.github.com/repos/urnetwork/build/releases/$GITHUB_RELEASE_ID" \
        -d "{\"tag_name\":\"v${EXTERNAL_WARP_VERSION}\",\"name\":\"v${EXTERNAL_WARP_VERSION}\",\"body\":$(echo -n "$RELEASE_BODY" | jq -Rsa .),\"draft\":false,\"prerelease\":$PRE_RELEASE,\"generate_release_notes\":false}"`
    error_trap 'github patch release'
}


# Build an sn cli command (sn/cli/<pkg>) for the given GOOS/GOARCH targets and
# package the binaries as <binary>.tar.gz under the command's build/ dir. The
# output binary is named <binary>. Version is stamped into `main.Version`: each
# thin cli/ main owns the Version var and hands it to its library at startup, so
# the linker path is immune to the release module fork (main is always "main").
#
# The sn miner is the former connect/provider, so it is built and shipped as the
# old `provider` binary/asset name that a bunch of downstream code still expects
# (the provider install/uninstall scripts, router/edgeos docs).
#
#   sn_cli_release <cli-pkg> <binary> <goos/goarch>...
sn_cli_release () {
    local pkg="$1" binary="$2"
    shift 2
    local src="$BUILD_HOME/sn${GO_MOD_SUFFIX}/cli/$pkg"
    local out="$src/build"
    rm -rf "$out" || return 1
    local osarch os arch gomips ext
    for osarch in "$@"; do
        os="${osarch%/*}"
        arch="${osarch#*/}"
        gomips=""
        case "$arch" in mips*) gomips="GOMIPS=softfloat" ;; esac
        # `go build -o <file>` writes the name verbatim -- it only appends the
        # platform exe suffix when it derives the name itself (`-o <dir>/`). The
        # binary is renamed here (cli/miner -> provider), so the suffix has to be
        # added by hand or windows ships an extensionless, unrunnable file.
        ext=""
        case "$os" in windows) ext=".exe" ;; esac
        (cd "$src" &&
            env GOEXPERIMENT=greenteagc CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" $gomips \
                go build -trimpath \
                -ldflags "-w -s -X main.Version=${WARP_VERSION}" \
                -o "$out/$os/$arch/$binary$ext" .) || return 1
    done
    (cd "$out" && COPYFILE_DISABLE=1 tar -czf "$binary.tar.gz" */)
}


github_create_draft_release


(cd $BUILD_HOME/sdk/build && make)
error_trap 'build sdk'

github_release_upload "URnetworkSdk-${EXTERNAL_WARP_VERSION}.aar" "$BUILD_HOME/sdk/build/android/URnetworkSdk.aar"
github_release_upload "URnetworkSdk-sources-${EXTERNAL_WARP_VERSION}.jar" "$BUILD_HOME/sdk/build/android/URnetworkSdk-sources.jar"
github_release_upload "URnetworkSdk-${EXTERNAL_WARP_VERSION}.xcframework.zip" "$BUILD_HOME/sdk/build/apple/URnetworkSdk.xcframework.zip"
github_release_upload "URnetworkSdkJs-${EXTERNAL_WARP_VERSION}.zip" "$BUILD_HOME/sdk/js/build/URnetworkSdkJs.zip"

builder_message "sdk \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


# The sn miner is the former connect/provider. Build all three sn cli tools and
# publish them as release artifacts. A bunch of downstream code still expects the
# miner under the old `provider` name -- the provider install/uninstall scripts
# pull a `urnetwork-provider-*.tar.gz` asset and run its `<os>/<arch>/provider`
# binary -- so ship the miner as `provider` for now. Match the old provider's
# full arch matrix so those installers keep working on every target.
sn_cli_release miner provider \
    linux/arm64 linux/arm linux/amd64 linux/386 \
    linux/mips linux/mipsle linux/mips64 linux/mips64le \
    darwin/arm64 darwin/amd64 windows/arm64 windows/amd64
error_trap 'build provider (sn miner)'

github_release_upload "urnetwork-provider-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/sn${GO_MOD_SUFFIX}/cli/miner/build/provider.tar.gz"

builder_message "provider (sn miner) \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


sn_cli_release validator validator \
    darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
error_trap 'build validator'

github_release_upload "urnetwork-validator-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/sn${GO_MOD_SUFFIX}/cli/validator/build/validator.tar.gz"

builder_message "validator \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


# snclaim signs + broadcasts the miner's on-chain claim actions.
sn_cli_release snclaim snclaim \
    darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
error_trap 'build snclaim'

github_release_upload "urnetwork-snclaim-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/sn${GO_MOD_SUFFIX}/cli/snclaim/build/snclaim.tar.gz"

builder_message "snclaim \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/server${GO_MOD_SUFFIX}/bringyourctl && make)
error_trap 'build bringyourctl'


(cd $BUILD_HOME/proxy${GO_MOD_SUFFIX}/socks && make)
error_trap 'build proxy socks'

github_release_upload "urnetwork-proxy-socks-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/proxy${GO_MOD_SUFFIX}/socks/build/proxy-socks.tar.gz"

builder_message "proxy socks \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"

# (cd $BUILD_HOME/proxy${GO_MOD_SUFFIX}/http && make)
# error_trap 'build proxy http'

# github_release_upload "urnetwork-proxy-http-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/proxy${GO_MOD_SUFFIX}/http/build/proxy-http.tar.gz"

# builder_message "proxy http \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"

# (cd $BUILD_HOME/proxy${GO_MOD_SUFFIX}/wg && make)
# error_trap 'build proxy wg'

# github_release_upload "urnetwork-proxy-wg-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/proxy${GO_MOD_SUFFIX}/wg/build/proxy-wg.tar.gz"

# builder_message "proxy wg \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/extension && make)
error_trap 'build extension'

github_release_upload "crx-@urnetwork-extension-${EXTENSION_VERSION}.zip" "$BUILD_HOME/extension/release/crx-@urnetwork-extension-${EXTENSION_VERSION}.zip"
github_release_upload "crx-@urnetwork-extension-${EXTENSION_VERSION}-firefox.zip" "$BUILD_HOME/extension/release/crx-@urnetwork-extension-${EXTENSION_VERSION}-firefox.zip"

builder_message "extension \`${EXTENSION_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


# the latest macos or xcode messes up the package with the error
#    ITMS-90048: This bundle is invalid - Your archive contains paths that are not allowed: [._Symbols]
# see https://github.com/flutter/flutter/issues/166367
bug_fix_clean_ipa () {
    unzip -l "$1" | grep ._Symbols && zip -d "$1" ._Symbols/ || echo "No ._Symbols found. Nothing to clean up."
}

# Headless provisioning auth (REMOTEBUILD.md): hand -allowProvisioningUpdates the
# App Store Connect API key explicitly (Xcode 13+) so profile downloads never
# depend on Xcode-account GUI state. altool below discovers the same .p8 by key
# id from these standard directories; when none is found the array stays empty
# and xcodebuild behaves exactly as before.
XCODEBUILD_AUTH=()
for d in "$HOME/.private_keys" "$HOME/private_keys" "$HOME/.appstoreconnect/private_keys"; do
    if [ -f "$d/AuthKey_${APPLE_API_KEY}.p8" ]; then
        XCODEBUILD_AUTH=(
            -authenticationKeyPath "$d/AuthKey_${APPLE_API_KEY}.p8"
            -authenticationKeyID "$APPLE_API_KEY"
            -authenticationKeyIssuerID "$APPLE_API_ISSUER"
        )
        break
    fi
done


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates $XCODEBUILD_AUTH -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=iOS &&
    $BUILD_HOME/sdk/build/check_apple_size.sh \
        --sdk $BUILD_HOME/sdk/build/apple/URnetworkSdk.xcframework \
        --extension-sdk $BUILD_HOME/sdk/build/apple/URnetworkExtensionSdk.xcframework \
        --extension build.xcarchive/Products/Applications/URnetwork.app/PlugIns/URnetworkVPN.appex &&
    xcodebuild archive -allowProvisioningUpdates $XCODEBUILD_AUTH -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=iOS &&
    bug_fix_clean_ipa build/URnetwork.ipa &&
    xcrun altool --show-progress --validate-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --show-progress --upload-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
# failure to deploy to apple connect means we can't create an iOS release, but other platforms can still release
# typically this is because we've already submitting a release for this build version
warn_trap 'ios deploy'

# the deploy above is allowed to soft-fail, so the .ipa may not exist (e.g. the
# export could not sign); skip the dependent uploads instead of hard-failing them
if [ -f "$BUILD_HOME/apple/app/build/URnetwork.ipa" ]; then
    github_release_upload "URnetwork-${EXTERNAL_WARP_VERSION}.ipa" "$BUILD_HOME/apple/app/build/URnetwork.ipa"

    builder_message "ios \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"
else
    builder_message "warning: no ios .ipa artifact (deploy soft-failed above); skipping its release upload. Build will continue."
fi


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates $XCODEBUILD_AUTH -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=macOS &&
    xcodebuild archive -allowProvisioningUpdates $XCODEBUILD_AUTH -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=macOS &&
    xcrun altool --show-progress --validate-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --show-progress --upload-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
# failure to deploy to apple connect means we can't create an macOS release, but other platforms can still release
# typically this is because we've already submitting a release for this build version
warn_trap 'macos deploy'

if [ -f "$BUILD_HOME/apple/app/build/URnetwork.pkg" ]; then
    github_release_upload "URnetwork-${EXTERNAL_WARP_VERSION}.pkg" "$BUILD_HOME/apple/app/build/URnetwork.pkg"

    builder_message "macos \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"
else
    builder_message "warning: no macos .pkg artifact (deploy soft-failed above); skipping its release upload. Build will continue."
fi


# =============================================================================
# Desktop apps: Windows Store (MSI) + Linux (deb + install tarball + rpm + AppImage).
# See build/DESKTOP_BUILD.md + build/all/{windows,linux}/README.md.
#
# Each platform builds via its own script (all/build-windows.sh,
# all/build-linux.sh). The Windows cgo SDK builds INSIDE the QEMU/HVF ARM Windows
# VM (sdk/cgo via Go + llvm-mingw); the Linux cgo SDK cross-builds natively on
# this macOS host (sdk/cgo via zig). Then the app
# *bundle* is produced LOCALLY on this host via virtualization — the MSI in a
# QEMU/HVF ARM Windows VM (build/all/windows, image built once by setup.sh);
# the Linux artifacts in Docker containers (build/all/linux; per arch TWO
# images, because the halves cannot share one: the daemon builds on Ubuntu
# 22.04, whose glibc 2.35 IS the floor the .deb declares, while the GTK4 GUI
# needs 24.04, the oldest Ubuntu packaging GTK4 — so 22.04 has no GTK at all.
# Each runs meson + the linux repo's packaging scripts, then verify.sh proves
# the artifacts work: the AppImage extracts, bundles its GTK stack without
# falling through to host libraries, and launches under xvfb; the .deb installs,
# creates its group and purges cleanly; the install.sh tarball round-trips. The
# artifact names are normative in linux/MIGRATION.md). The scripts use the local branches as-is
# (the version branches configured above) and inherit BUILD_HOME + the WARP_*
# versions exported above; they can also be re-run standalone after this
# pipeline, e.g. when a flaky VM/container build needs a retry.
#
# Required: both desktop builds are release gates. On failure, stop before
# uploading that platform's stale or partial artifacts.
#
# SUBMISSION/PUBLISHING is manual for now: this pipeline builds the bundles and
# attaches them to the GitHub release; a human submits the MSI to the Microsoft
# Store (Partner Center). Linux has no store — the deb/tarball/rpm/AppImage
# ship from the release page, and re-hosting the AppImage + .zsync on the
# update endpoint (GitHub Releases can't serve the multi-range requests zsync
# needs — linux/APPIMAGE.md §11f) is likewise a manual follow-up. Publishing
# the .deb to an apt repo and the .rpm to a dnf repo (which additionally wants
# an rpm HEADER signature, not the detached .asc) are manual follow-ups too.
# =============================================================================

DESKTOP_OUT="${BUILD_OUT:-$BUILD_HOME/out}/desktop"

builder_message "building windows app (cgo sdk + MSI in the local QEMU ARM Windows VM)"
OUT_DIR="$DESKTOP_OUT/windows" "$BUILD_HOME/all/build-windows.sh"
error_trap 'windows build'
github_release_upload "URnetworkSdkWindows-${EXTERNAL_WARP_VERSION}.zip" "$BUILD_HOME/sdk/cgo/build/URnetworkSdkWindows.zip"
for msi in "$DESKTOP_OUT/windows/"*.msi(N); do
    github_release_upload "$(basename "$msi")" "$msi"
done
builder_message "windows \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"

builder_message "building linux app (cgo sdk + deb/install-tarball/rpm/arch on ubuntu 22.04 + AppImage on ubuntu 24.04, each verified in-container; then Flatpak in its dependency-complete Linux container)"
OUT_DIR="$DESKTOP_OUT/linux" "$BUILD_HOME/all/build-linux.sh"
error_trap 'linux build'
github_release_upload "URnetworkSdkLinux-${EXTERNAL_WARP_VERSION}.zip" "$BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"

# THE FLATPAK, built in its own Linux container. The release host is macOS, so
# flatpak cannot be a host prerequisite. Dockerfile.flatpak installs flatpak,
# native flatpak-builder, elfutils and their source helpers; the first run also
# installs the GNOME runtime into a persistent Docker volume. The container is
# separate from the daemon/AppImage images because bubblewrap needs nested
# namespaces: only this one receives CAP_SYS_ADMIN/CAP_NET_ADMIN and the
# matching Docker security-policy relaxations. Its source bind mount remains
# read-only.
#
# It runs AFTER build-linux.sh because the manifest vendors the cgo SDK that
# build-linux.sh stages into the tree.
#
# ONE ARCH. flatpak-builder has no cross-compile mode, so this produces a bundle
# for this build machine's architecture (inside the matching Docker platform).
# The other architecture still gets the AppImage and native packages.
#
# This is a release gate now that its toolchain is managed by the build. A real
# Flatpak failure must stop before the uploader silently ships no bundle.
VERSION="${EXTERNAL_WARP_VERSION}" \
LINUX_DIR="$BUILD_HOME/linux" \
OUT_DIR="$DESKTOP_OUT/linux" \
    bash "$BUILD_HOME/all/linux/build-flatpak.sh"
error_trap 'flatpak build'

# Six artifact types per arch (names normative — linux/MIGRATION.md):
# urnetwork-daemon_<v>_<arch>.deb, urnetwork-daemon-<v>-<arch>.install.tar.gz,
# urnetwork-daemon-<v>.<rpmarch>.rpm, urnetwork-daemon-<v>-<pkgarch>.pkg.tar.zst,
# URnetwork-<v>-<arch>.AppImage + .AppImage.zsync, and URnetwork-<v>-<arch>.flatpak
# (one native architecture). (N) nullglobs each pattern so a missing optional
# type uploads nothing rather than a literal '*' — which is what makes the
# .rpm's warn-and-continue behaviour in build-linux.sh work at this level.
#
# A bare *.rpm is safe: nfpm writes binary rpms only, so no .src.rpm can
# appear here. The .sha256/.asc sidecars the packaging scripts write are
# deliberately NOT uploaded, matching the .deb's existing behaviour.
for artifact in "$DESKTOP_OUT/linux/"*.deb(N) \
                "$DESKTOP_OUT/linux/"*.install.tar.gz(N) \
                "$DESKTOP_OUT/linux/"*.rpm(N) \
                "$DESKTOP_OUT/linux/"*.pkg.tar.zst(N) \
                "$DESKTOP_OUT/linux/"*.flatpak(N) \
                "$DESKTOP_OUT/linux/"*.AppImage(N) \
                "$DESKTOP_OUT/linux/"*.AppImage.zsync(N); do
    github_release_upload "$(basename "$artifact")" "$artifact"
done
builder_message "linux \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/android/app &&
    ./gradlew clean assemblePlayRelease bundlePlayRelease assembleSolana_dappRelease assembleEthos_dappRelease)
error_trap 'android build'

github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}-play-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/play/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-play-universal-release.apk"

github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}-play-release.aab" \
    "$BUILD_HOME/android/app/app/build/outputs/bundle/playRelease/com.bringyour.network-${EXTERNAL_WARP_VERSION}-play-release.aab"

github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}-solana_dapp-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/solana_dapp/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-solana_dapp-universal-release.apk"

github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}-ethos_dapp-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/ethos_dapp/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-ethos_dapp-universal-release.apk"

if [ "$BUILD_OUT" ]; then
    (mkdir -p "$BUILD_OUT/apk" &&
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} "$BUILD_OUT/apk" \;)
    error_trap 'android local copy'
fi

# FIXME apple archive and upload to internal testflight
# FIXME android play release to play internal testing

builder_message "android \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


# Github / Ungoogle
# note for F-Droid, the -ungoogle tag should be aliased to -fdroid to trigger their build
#
# REMEMBER WHERE THE RELEASE COMMIT IS, because everything below moves the
# android submodule off it and onto a degoogled branch, and the build repo has
# to be put back afterwards. See the restore at the end of this section for why
# that matters.
ANDROID_RELEASE_COMMIT=`cd $BUILD_HOME/android && git rev-parse HEAD`
error_trap 'android release commit'
(cd $BUILD_HOME/android &&
    git checkout -b v${EXTERNAL_WARP_VERSION}-ungoogle)
error_trap 'android prepare ungoogle version branch'
(cd $BUILD_HOME/android &&
    echo -n "
warp.version=$WARP_VERSION_BASE
warp.version_code=$WARP_VERSION_CODE
" > app/local.properties &&
    git add app/local.properties -f &&
    $BUILD_SED -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/app/build.gradle &&
    $BUILD_SED -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/settings.gradle)
error_trap 'android edit ungoogle settings'
(cd $BUILD_HOME/android && 
    git add . && 
    git commit -m "${EXTERNAL_WARP_VERSION}-ungoogle" && 
    git push -u origin v${EXTERNAL_WARP_VERSION}-ungoogle)
error_trap 'android ungoogle push branch'

(cd $BUILD_HOME && 
    git add . && 
    git commit -m "$HOST build ungoogle" && 
    git push)
error_trap 'push ungoogle branch'
(cd $BUILD_HOME && 
    git tag -a v${EXTERNAL_WARP_VERSION}-ungoogle -m "${EXTERNAL_WARP_VERSION}-ungoogle" && 
    git push origin v${EXTERNAL_WARP_VERSION}-ungoogle)
error_trap 'push ungoogle tag'

# build in the fdroid server context (all/build-fdroid.sh — uses the ungoogle
# branch state configured above; also runs standalone)
# ideally this should not be required, but there are some small differences in the android artifacts apparently due to build environment (macos/arm versus linux/amd perhaps)
"$BUILD_HOME/all/build-fdroid.sh"
error_trap 'android github build'

# upload github as the "official" apk which lexicograhpically sorts to the front, for release systems like Obtanium
github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-universal-release.apk"

# github_release_upload \
#     "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-universal-release.apk" \
#     "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-universal-release.apk"

# github_release_upload \
#     "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-armeabi-v7a-release.apk" \
#     "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-armeabi-v7a-release.apk"

# github_release_upload \
#     "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-arm64-v8a-release.apk" \
#     "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-arm64-v8a-release.apk"

if [ "$BUILD_OUT" ]; then
    (mkdir -p "$BUILD_OUT/apk-github" && 
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} "$BUILD_OUT/apk-github" \;)
    error_trap 'android github local copy'
fi


# PUT THE ANDROID PIN BACK. Everything the degoogled tree was needed for is
# done: the -ungoogle branch and tag are pushed, build-fdroid.sh has run, and
# the apks are uploaded and copied. What is left is a build repo whose main
# records the degoogled android commit as its submodule pin, and that is not a
# state anybody should inherit.
#
# The degoogled build.gradle comments out every play-services dependency, but
# the play and solana_dapp sources still import GoogleSignIn, LocationServices
# and friends. So main pinned there cannot compile those flavors: every CI run
# and every pull request against main fails the android job until the next
# release moves the pin again. The `builder build ungoogle` commit is what
# leaves it there, and it has been doing so every release -- it only became
# visible when the android build+test workflow was added.
#
# The -ungoogle TAG still points at the degoogled tree, which is the point of
# it: F-Droid builds from that tag. This restores only main, to the same commit
# v${EXTERNAL_WARP_VERSION} already records.
#
# THE WORKING TREE IS RESTORED TOO, not just the index. The next run opens with
# `git diff --quiet && git diff --cached --quiet` on $BUILD_HOME (see the pull
# near the top), and a submodule whose checkout disagrees with the recorded pin
# reads as a dirty diff -- which would short-circuit that && chain and abort the
# run at 'pull'. Fixing the pin without the checkout would trade a broken CI for
# a broken build host.
if [ "$ANDROID_RELEASE_COMMIT" ]; then
    (cd $BUILD_HOME/android && git checkout "$ANDROID_RELEASE_COMMIT")
    error_trap 'android restore release commit'
    # --quiet is the no-op guard: when the pin already matches (nothing staged),
    # `git commit` would exit non-zero and error_trap would sink a good release.
    (cd $BUILD_HOME &&
        git add android &&
        (git diff --cached --quiet ||
            (git commit -m "${EXTERNAL_WARP_VERSION} restore android pin" &&
                (git push || (git pull --rebase && git push)))))
    error_trap 'restore android pin'
    builder_message "android pin restored to \`${ANDROID_RELEASE_COMMIT}\` (the \`${EXTERNAL_WARP_VERSION}\` release commit); the degoogled tree stays on the \`v${EXTERNAL_WARP_VERSION}-ungoogle\` tag"
else
    builder_message "warning: ANDROID_RELEASE_COMMIT is empty, so main is still pinned to the degoogled android tree. Every CI run against main will fail the android job until that is corrected."
fi

# Upload releases to testing channels

builder_message "android github \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


github_create_release


# create pre-releases for version code variants
# this is needed for reproducible builds
(BASE_EXTERNAL_WARP_VERSION="$EXTERNAL_WARP_VERSION" &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+2))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    github_create_draft_release true &&
    github_release_upload \
        "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-armeabi-v7a-release.apk" \
        "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${BASE_EXTERNAL_WARP_VERSION}-github-armeabi-v7a-release.apk" &&
    github_create_release true
    echo "[1/2]Monitor the F-Droid build here: https://monitor.f-droid.org/builds/log/com.bringyour.network/$WARP_VERSION_CODE"
)
error_trap 'android github armeabi-v7a reproducible pre-release'

(BASE_EXTERNAL_WARP_VERSION="$EXTERNAL_WARP_VERSION" &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+3))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    github_create_draft_release true &&
    github_release_upload \
        "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-arm64-v8a-release.apk" \
        "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${BASE_EXTERNAL_WARP_VERSION}-github-arm64-v8a-release.apk"
    github_create_release true
    echo "[2/2]Monitor the F-Droid build here: https://monitor.f-droid.org/builds/log/com.bringyour.network/$WARP_VERSION_CODE"
)
error_trap 'android github arm64-v8a reproducible pre-release'


# Warp services
(cd $BUILD_HOME && warpctl build $BUILD_ENV warp/config-updater/Makefile)
error_trap 'warpctl build config-updater'
builder_message "service config-updater \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV warp/grafana/Makefile)
error_trap 'warpctl build grafana'
builder_message "service grafana \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV warp/lb/Makefile)
error_trap 'warpctl build lb'
builder_message "service lb \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/taskworker/Makefile)
error_trap 'warpctl build taskworker'
builder_message "service taskworker \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/api/Makefile)
error_trap 'warpctl build api'
builder_message "service api \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/connect/Makefile)
error_trap 'warpctl build connect'
builder_message "service connect \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/mcp/Makefile)
error_trap 'warpctl build mcp'
builder_message "service mcp \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/cli/proxy/Makefile)
error_trap 'warpctl build proxy'
builder_message "service proxy \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV web/web/Makefile)
error_trap 'warpctl build web'
builder_message "service web \`${EXTERNAL_WARP_VERSION}\` available"

(cd $BUILD_HOME && warpctl build $BUILD_ENV web/app/Makefile)
error_trap 'warpctl build web/app'
builder_message "service web/app \`${EXTERNAL_WARP_VERSION}\` available"

builder_message "release \`${EXTERNAL_WARP_VERSION}\` complete - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


if [ "$WARP_SKIP_DEPLOY" = "" ]; then
    source "$BUILD_HOME/build/all/deploy-rollout.zsh"
    warp_rollout
    error_trap 'warp service rollout'
fi

builder_message "Build all \`${EXTERNAL_WARP_VERSION}\` ... done! Enjoy :) - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"
