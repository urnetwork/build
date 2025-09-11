#!/usr/bin/env zsh

# Expect the following env vars to be set:
# WARP_HOME
# APPLE_API_KEY
# APPLE_API_ISSUER
# GITHUB_API_KEY
# (optional) BUILD_OUT
# (optional) SLACK_WEBHOOK


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
export BUILD_CURL=(curl -s -L)
export BRINGYOUR_HOME=`realpath ..`
if [ ! "$STAGE_SECONDS" ]; then
    export STAGE_SECONDS=60
fi


git_main () {
    git diff --quiet && git diff --cached --quiet && git checkout main && git pull --recurse-submodules
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
    (cd $BUILD_HOME && rm -rf server)
    (cd $BUILD_HOME && rm -rf web)
    (cd $BUILD_HOME && rm -rf docs)
    (cd $BUILD_HOME && rm -rf warp)
    (cd $BUILD_HOME && 
        git stash -u && 
        git reset --hard && 
        git submodule update --init)
    error_trap 'reset'
fi


BUILD_PRE_COMMIT=`cd $BUILD_HOME && git log -1 --format=%H`
(cd $BUILD_HOME && git_main)
error_trap 'pull'
BUILD_COMMIT=`cd $BUILD_HOME && git log -1 --format=%H`
if [ "$BUILD_PRE_COMMIT" != "$BUILD_COMMIT" ]; then
    builder_message "Build repo updated. Must restart to use the latest script."
    exit 1
fi


ANDROID_NDK_VERSION=28.2.13676358
sdkmanager "ndk;$ANDROID_NDK_VERSION"
error_trap 'android ndk'
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"
if [[ ! `go version` =~ 'go version go1.25.0' ]]; then
    builder_message 'go 1.25.0 required'
    exit 1
fi
if [[ ! `java -version 2>&1` =~ 'openjdk version "21.0.8"' ]]; then
    builder_message 'java 21.0.8 required'
    exit 1
fi


(cd $BUILD_HOME/connect && git_main)
error_trap 'pull connect'
(cd $BUILD_HOME/sdk && git_main)
error_trap 'pull sdk'
(cd $BUILD_HOME/android && git_main)
error_trap 'pull android'
(cd $BUILD_HOME/apple && git_main)
error_trap 'pull apple'
(cd $BUILD_HOME/server && git_main)
error_trap 'pull server'
(cd $BUILD_HOME/web && git_main)
error_trap 'pull web'
(cd $BUILD_HOME/docs && git_main)
error_trap 'pull docs'
(cd $BUILD_HOME/warp && git_main)
error_trap 'pull warp'


if [ "$BUILD_TEST" ]; then
    (cd $BUILD_HOME/connect && ./test.sh)
    error_trap 'connect tests'
    # FIXME
    (cd $BUILD_HOME/connect/provider && ./test.sh)
    error_trap 'connect provider tests'
    (cd $BUILD_HOME/sdk && ./test.sh)
    error_trap 'sdk tests'
    (cd $BUILD_HOME/server && ./test.sh)
    error_trap 'server tests'
    # (cd $BUILD_HOME/server/connect && ./test.sh)
    # error_trap 'server connect tests'
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


export WARP_VERSION_BASE=`warpctl ls version`
error_trap 'warpctl version'
export WARP_VERSION_CODE=`warpctl ls version-code`
error_trap 'warpctl version code'
GO_MOD_VERSION=`echo $WARP_VERSION_BASE | $BUILD_SED 's/\([^\.]*\).*/\1/'`
if [ $GO_MOD_VERSION = 0 ] || [ $GO_MOD_VERSION = 1 ]; then
    GO_MOD_SUFFIX=''
else
    GO_MOD_SUFFIX="/v${GO_MOD_VERSION}"
fi
export WARP_VERSION="${WARP_VERSION_BASE}+${WARP_VERSION_CODE}"
export EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}"


# rebuild warpctl with the `WARP_*` env vars so we have the binary properly versioned
(cd $BUILD_HOME/warp/warpctl && make)
error_trap 'build warpctl'


builder_message "Build all \`${EXTERNAL_WARP_VERSION}\`"


(cd $BUILD_HOME/connect && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'connect prepare version branch'
(cd $BUILD_HOME/sdk && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'sdk prepare version branch'
(cd $BUILD_HOME/android && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'android prepare version branch'
(cd $BUILD_HOME/apple && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'apple prepare version branch'
(cd $BUILD_HOME/server && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'server prepare version branch'
(cd $BUILD_HOME/web && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'web prepare branch'
(cd $BUILD_HOME/docs && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'docs prepare branch'
(cd $BUILD_HOME/warp && git checkout -b v${EXTERNAL_WARP_VERSION})
error_trap 'warp prepare branch'


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


# metadata
# copy the pending change log into place if it exists
if [ -e "$BUILD_HOME/metadata/en-US/changelogs/pending.txt" ]; then
    cp \
        "$BUILD_HOME/metadata/en-US/changelogs/pending.txt" \
        "$BUILD_HOME/metadata/en-US/changelogs/${WARP_VERSION_CODE}.txt"
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

go_mod_fork () {
    if [ $GO_MOD_VERSION != 0 ] && [ $GO_MOD_VERSION != 1 ]; then
        temp=`mktemp -d` &&
        for f in *; do
            if [ ! -e "$f/go.mod" ] && [ ! -e "$f/v${GO_MOD_VERSION}/go.mod" ] && [[ ! "$f" =~ "^($1)\$" ]]; then
                mv "$f" "$temp"
            fi
        done &&
        $BUILD_SED -i '/^retract/d' "$temp/go.mod" &&
        mv "$temp" v${GO_MOD_VERSION} &&
        # the go go.sum needs to be updated
        (cd v${GO_MOD_VERSION} && go mod tidy && go get -t ./...)
    fi
}


git_commit () {
    git add . &&
    if ! (git diff --quiet && git diff --cached --quiet); then
        git commit -m "${EXTERNAL_WARP_VERSION}" &&
        git push -u origin v${EXTERNAL_WARP_VERSION}
    fi
}

git_tag () {
    git push --delete origin refs/tags/v${EXTERNAL_WARP_VERSION} &&
    git tag -a v${EXTERNAL_WARP_VERSION} -m "${EXTERNAL_WARP_VERSION}" &&
    git push origin refs/tags/v${EXTERNAL_WARP_VERSION}
}


(cd $BUILD_HOME/connect &&
    go_mod_edit_module github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go mod edit -dropretract '[v0.0.1, v0.1.13]' &&
    go_mod_fork 'api')
error_trap 'connect edit'

(cd $BUILD_HOME/connect && 
    git_commit &&
    git_tag)
error_trap 'connect push branch'


(cd $BUILD_HOME/sdk/build &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/sdk)
error_trap 'sdk build edit'

# TODO `-ldflags "-X sdk.Version=...` doesn't appear to work with gomobile
# TODO we hardcode the sdk.Version for now
(cd $BUILD_HOME/sdk &&
    go_mod_edit_module github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    $BUILD_SED -i "s/Version string = \"\"/Version string = \"${WARP_VERSION}\"/g" sdk.go &&
    go_mod_fork 'build')
error_trap 'sdk edit'

(cd $BUILD_HOME/sdk &&
    git_commit &&
    git_tag)
error_trap 'sdk push branch'


(cd $BUILD_HOME/server &&
    go_mod_edit_module github.com/urnetwork/server &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/server &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go_mod_fork)
error_trap 'server edit'

(cd $BUILD_HOME/server &&
    git_commit &&
    git_tag)
error_trap 'server push branch'


(cd $BUILD_HOME/android && 
    git_commit &&
    git_tag)
error_trap 'android push branch'


(cd $BUILD_HOME/apple && 
    git_commit &&
    git_tag)
error_trap 'apple push branch'


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


(cd $BUILD_HOME &&
    git add . &&
    git commit -m "${EXTERNAL_WARP_VERSION}" &&
    git push &&
    git_tag)
# version code variants for the github flavor
(cd $BUILD_HOME &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+2))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    git_tag)
(cd $BUILD_HOME &&
    WARP_VERSION_CODE=$(($WARP_VERSION_CODE+3))
    EXTERNAL_WARP_VERSION="${WARP_VERSION_BASE}-${WARP_VERSION_CODE}" &&
    git_tag)
error_trap 'push branch'


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

virustotal () {
    SHA256=`shasum -a 256 "$2" | awk '{ print $1 }'`

    if [ "$VIRUSTOTAL_API_KEY" ]; then
        VIRUSTOTAL_PREPARE_UPLOAD=`$BUILD_CURL \
            -H 'Accept: application/json' \
            -H "x-apikey: $VIRUSTOTAL_API_KEY" \
            https://www.virustotal.com/api/v3/files/upload_url`
        error_trap "virustotal prepare upload $1"
        VIRUSTOTAL_UPLOAD_URL=`echo "$VIRUSTOTAL_PREPARE_UPLOAD" | jq -r .data`
        VIRUSTOTAL_UPLOAD=`$BUILD_CURL \
            -X POST \
            -H 'Accept: application/json' \
            -H 'Content-Type: multipart/form-data' \
            -H "x-apikey: $VIRUSTOTAL_API_KEY" \
            "$VIRUSTOTAL_UPLOAD_URL" \
            -F "file=@$2"`
        error_trap "virustotal upload $1"
        VIRUSTOTAL_ID=`echo "$VIRUSTOTAL_UPLOAD" | jq -r .data.id`

        virustotal_verify "$1" "$VIRUSTOTAL_ID"

        VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${EXTERNAL_WARP_VERSION}/$1)|\`$SHA256\`|[ok](https://www.virustotal.com/gui/file/$SHA256)|")
    else
        VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${EXTERNAL_WARP_VERSION}/$1)|\`$SHA256\`|not submitted|")
    fi
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
            fi
        fi
        echo "virustotal analysis $1 waiting for result ($VIRUSTOTAL_ANALYSIS_STATUS) ..."
        sleep 10
    done
    builder_message "virustotal analysis $1 did not complete"
    exit 1
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

    GITHUB_RELEASE=`$BUILD_CURL \
        -X PATCH \
        -H 'Accept: application/vnd.github+json' \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        "https://api.github.com/repos/urnetwork/build/releases/$GITHUB_RELEASE_ID" \
        -d "{\"tag_name\":\"v${EXTERNAL_WARP_VERSION}\",\"name\":\"v${EXTERNAL_WARP_VERSION}\",\"body\":$(echo -n "$RELEASE_BODY" | jq -Rsa .),\"draft\":false,\"prerelease\":$PRE_RELEASE,\"generate_release_notes\":false}"`
    error_trap 'github patch release'
}


github_create_draft_release


(cd $BUILD_HOME/sdk/build && make)
error_trap 'build sdk'

github_release_upload "URnetworkSdk-${EXTERNAL_WARP_VERSION}.aar" "$BUILD_HOME/sdk/build/android/URnetworkSdk.aar"
github_release_upload "URnetworkSdk-sources-${EXTERNAL_WARP_VERSION}.jar" "$BUILD_HOME/sdk/build/android/URnetworkSdk-sources.jar"
github_release_upload "URnetworkSdk-${EXTERNAL_WARP_VERSION}.xcframework.zip" "$BUILD_HOME/sdk/build/apple/URnetworkSdk.xcframework.zip"

builder_message "sdk \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/connect${GO_MOD_SUFFIX}/provider && make)
error_trap 'build provider'

github_release_upload "urnetwork-provider-${EXTERNAL_WARP_VERSION}.tar.gz" "$BUILD_HOME/connect${GO_MOD_SUFFIX}/provider/build/provider.tar.gz"

builder_message "provider \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/server${GO_MOD_SUFFIX}/bringyourctl && make)
error_trap 'build bringyourctl'


# the latest macos or xcode messes up the package with the error
#    ITMS-90048: This bundle is invalid - Your archive contains paths that are not allowed: [._Symbols]
# see https://github.com/flutter/flutter/issues/166367
bug_fix_clean_ipa () {
    unzip -l "$1" | grep ._Symbols && zip -d "$1" ._Symbols/ || echo "No ._Symbols found. Nothing to clean up."
}


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=iOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=iOS &&
    bug_fix_clean_ipa build/URnetwork.ipa &&
    xcrun altool --show-progress --validate-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --show-progress --upload-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
# failure to deploy to apple connect means we can't create an iOS release, but other platforms can still release
# typically this is because we've already submitting a release for this build version
warn_trap 'ios deploy'

github_release_upload "URnetwork-${EXTERNAL_WARP_VERSION}.ipa" "$BUILD_HOME/apple/app/build/URnetwork.ipa"

builder_message "ios \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=macOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=macOS &&
    xcrun altool --show-progress --validate-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --show-progress --upload-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
# failure to deploy to apple connect means we can't create an macOS release, but other platforms can still release
# typically this is because we've already submitting a release for this build version
warn_trap 'macos deploy'

github_release_upload "URnetwork-${EXTERNAL_WARP_VERSION}.pkg" "$BUILD_HOME/apple/app/build/URnetwork.pkg"

builder_message "macos \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


(cd $BUILD_HOME/android/app &&
    ./gradlew clean assemblePlayRelease bundlePlayRelease assembleSolana_dappRelease)
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

# build in the fdroid server context
# ideally this should not be required, but there are some small differences in the android artifacts apparently due to build environment (macos/arm versus linux/amd perhaps)
# FIXME - there is currently a bug on apple m4 that causes an "invalid instruction" error when using the docker apple virtualization framework
#         see https://github.com/golang/go/issues/71434
#         The docker vmm framework is a work around, but it is very slow for amd64.
#         Run on an m1 or intel mac for now.
(cd $BUILD_HOME &&
    docker run --oom-kill-disable --memory="8192m" --rm -u vagrant \
        --entrypoint /urnetwork/build/fdroid/build.sh \
        -v $WARP_HOME/release:/urnetwork/release:z \
        -v $BUILD_HOME:/urnetwork/build:Z \
        registry.gitlab.com/fdroid/fdroidserver:buildserver)
error_trap 'android github build'

github_release_upload \
    "com.bringyour.network-${EXTERNAL_WARP_VERSION}-github-release.apk" \
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

# Upload releases to testing channels

builder_message "android github \`${EXTERNAL_WARP_VERSION}\` available - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"


github_create_release
builder_message "release \`${EXTERNAL_WARP_VERSION}\` complete - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"



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
(cd $BUILD_HOME && warpctl build $BUILD_ENV warp/lb/Makefile)
error_trap 'warpctl build lb'
(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/taskworker/Makefile)
error_trap 'warpctl build taskworker'
(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/api/Makefile)
error_trap 'warpctl build api'
(cd $BUILD_HOME && warpctl build $BUILD_ENV server${GO_MOD_SUFFIX}/connect/Makefile)
error_trap 'warpctl build connect'
(cd $BUILD_HOME && warpctl build $BUILD_ENV web/Makefile)
error_trap 'warpctl build web'
(cd $BUILD_HOME && warpctl build $BUILD_ENV web/app/Makefile)
error_trap 'warpctl build web/app'
if [ $BUILD_ENV = 'main' ]; then
    (cd $BUILD_HOME && warpctl build community connect${GO_MOD_SUFFIX}/provider/Makefile)
    error_trap 'warpctl build community provider'
fi


builder_message "${BUILD_ENV}[0%] services: \`\`\`$(warpctl ls versions $BUILD_ENV --sample)\`\`\`"


warpctl deploy $BUILD_ENV lb ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] lb \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] config-updater \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] taskworker \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] api \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] connect \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] web \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV app ${WARP_VERSION} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] app \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION} --percent=25 --only-older --timeout=0
    builder_message "community[25%] provider \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[25%] services: \`\`\`$(warpctl ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] lb \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] config-updater \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] taskworker \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] api \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] connect \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] web \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV app ${WARP_VERSION} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] app \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION} --percent=50 --only-older --timeout=0
    builder_message "community[50%] provider \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[50%] services: \`\`\`$(warpctl ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] lb \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] config-updater \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] taskworker \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] api \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] connect \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] web \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV app ${WARP_VERSION} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] app \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION} --percent=75 --only-older --timeout=0
    builder_message "community[75%] provider \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[75%] services: \`\`\`$(warpctl ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] lb \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] config-updater \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] taskworker \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] api \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] connect \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] web \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
warpctl deploy $BUILD_ENV app ${WARP_VERSION} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] app \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION} --percent=100 --only-older --timeout=0 --set-latest
    builder_message "community[100%] provider \`${EXTERNAL_WARP_VERSION}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[100%] services: \`\`\`$(warpctl ls versions $BUILD_ENV --sample)\`\`\`"


builder_message "Build all \`${EXTERNAL_WARP_VERSION}\` ... done! Enjoy :) - https://github.com/urnetwork/build/releases/tag/v${EXTERNAL_WARP_VERSION}"
