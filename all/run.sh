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


sdkmanager 'ndk;28.0.13004108'
error_trap 'android ndk'
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/28.0.13004108
if [[ ! `go version` =~ 'go version go1.24.2' ]]; then
    builder_message 'go 1.24.2 required'
    exit 1
fi
if [[ ! `java -version 2>&1` =~ 'openjdk version "22.0.2"' ]]; then
    builder_message 'java 22.0.2 required'
    exit 1
fi

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
    # (cd $BUILD_HOME/connect/provider && ./test.sh)
    # error_trap 'connect provider tests'
    (cd $BUILD_HOME/sdk && ./test.sh)
    error_trap 'sdk tests'
    (cd $BUILD_HOME/server && ./test.sh)
    error_trap 'server tests'
    (cd $BUILD_HOME/server/connect && ./test.sh)
    error_trap 'server connect tests'
fi


(cd $BUILD_HOME/warp/warpctl && make)
error_trap 'build warpctl'
export BUILD_WARPCTL="$BUILD_HOME/warp/warpctl/build/darwin/arm64/warpctl"

$BUILD_WARPCTL stage version next release --message="$HOST build all"
error_trap 'warpctl stage version'

export WARP_VERSION=`$BUILD_WARPCTL ls version`
error_trap 'warpctl version'
export WARP_VERSION_CODE=`$BUILD_WARPCTL ls version-code`
error_trap 'warpctl version code'
GO_MOD_VERSION=`echo $WARP_VERSION | $BUILD_SED 's/\([^\.]*\).*/\1/'`
if [ $GO_MOD_VERSION = 0 ] || [ $GO_MOD_VERSION = 1 ]; then
    GO_MOD_SUFFIX=''
else
    GO_MOD_SUFFIX="/v${GO_MOD_VERSION}"
fi

builder_message "Build all \`${WARP_VERSION}-${WARP_VERSION_CODE}\`"


(cd $BUILD_HOME/connect && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'connect prepare version branch'
(cd $BUILD_HOME/sdk && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'sdk prepare version branch'
(cd $BUILD_HOME/android && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'android prepare version branch'
(cd $BUILD_HOME/apple && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'apple prepare version branch'
(cd $BUILD_HOME/server && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'server prepare version branch'
(cd $BUILD_HOME/web && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'web prepare branch'
(cd $BUILD_HOME/docs && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'docs prepare branch'
(cd $BUILD_HOME/warp && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'warp prepare branch'


# apple branch, edit xcodeproject

(cd $BUILD_HOME/apple &&
    $BUILD_SED -i "s|\(MARKETING_VERSION *= *\).*;|\1${WARP_VERSION};|g" app/app.xcodeproj/project.pbxproj &&
    $BUILD_SED -i "s|\(CURRENT_PROJECT_VERSION *= *\).*;|\1${WARP_VERSION_CODE};|g" app/app.xcodeproj/project.pbxproj)
error_trap 'apple edit settings'

(cd $BUILD_HOME/android &&
    echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
pwsdk.maven.username=urnetwork-ops
pwsdk.maven.password=\"github_\\
pat_\\
11BQVGHTQ0rIdh8RxR4RPf_\\
uhLZtvZ4DaSheSH67doTow5cDYKoE\\
Qg064LrhQzwavQ3IPIHBLXABPAsqHK\"
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
    go mod edit -require=$1${GO_MOD_SUFFIX}@v${WARP_VERSION}-${WARP_VERSION_CODE} &&
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
        git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" &&
        git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE}
    fi
}

git_tag () {
    git push --delete origin refs/tags/v${WARP_VERSION}-${WARP_VERSION_CODE} &&
    git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE} -m "${WARP_VERSION}-${WARP_VERSION_CODE}" &&
    git push origin refs/tags/v${WARP_VERSION}-${WARP_VERSION_CODE}
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

(cd $BUILD_HOME/sdk &&
    go_mod_edit_module github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
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
    git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" &&
    git push &&
    git_tag)
error_trap 'push branch'


# Build release

github_create_draft_release () {
    GITHUB_RELEASE=`$BUILD_CURL \
        -X POST \
        -H 'Accept: application/vnd.github+json' \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        -H 'X-GitHub-Api-Version: 2022-11-28' \
        https://api.github.com/repos/urnetwork/build/releases \
        -d "{\"tag_name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"body\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"draft\":true,\"prerelease\":false,\"generate_release_notes\":false}"`
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

        VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${WARP_VERSION}-${WARP_VERSION_CODE}/$1)|\`$SHA256\`|[ok](https://www.virustotal.com/gui/file/$SHA256)|")
    else
        VIRUSTOTAL_ARTIFACTS+=("|[$1](https://github.com/urnetwork/build/releases/download/v${WARP_VERSION}-${WARP_VERSION_CODE}/$1)|\`$SHA256\`|not submitted|")
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
    RELEASE_BODY="v${WARP_VERSION}-${WARP_VERSION_CODE}

\"$(shuf -n 1 release-color.txt) $(shuf -n 1 release-texture.txt) $(shuf -n 1 release-mineral.txt)\"

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
        -d "{\"tag_name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"body\":$(echo -n "$RELEASE_BODY" | jq -Rsa .),\"draft\":false,\"prerelease\":false,\"generate_release_notes\":false}"`
    error_trap 'github patch release'
}


github_create_draft_release


(cd $BUILD_HOME/sdk/build && make)
error_trap 'build sdk'

github_release_upload "URnetworkSdk-${WARP_VERSION}-${WARP_VERSION_CODE}.aar" "$BUILD_HOME/sdk/build/android/URnetworkSdk.aar"
github_release_upload "URnetworkSdk-sources-${WARP_VERSION}-${WARP_VERSION_CODE}.jar" "$BUILD_HOME/sdk/build/android/URnetworkSdk-sources.jar"
github_release_upload "URnetworkSdk-${WARP_VERSION}-${WARP_VERSION_CODE}.xcframework.zip" "$BUILD_HOME/sdk/build/apple/URnetworkSdk.xcframework.zip"

builder_message "sdk \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd $BUILD_HOME/connect${GO_MOD_SUFFIX}/provider && make)
error_trap 'build provider'

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

github_release_upload "URnetwork-${WARP_VERSION}-${WARP_VERSION_CODE}.ipa" "$BUILD_HOME/apple/app/build/URnetwork.ipa"

builder_message "ios \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=macOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=macOS &&
    xcrun altool --show-progress --validate-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --show-progress --upload-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
# failure to deploy to apple connect means we can't create an macOS release, but other platforms can still release
# typically this is because we've already submitting a release for this build version
warn_trap 'macos deploy'

github_release_upload "URnetwork-${WARP_VERSION}-${WARP_VERSION_CODE}.pkg" "$BUILD_HOME/apple/app/build/URnetwork.pkg"

builder_message "macos \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"

(cd $BUILD_HOME/android/app &&
    ./gradlew clean assemblePlayRelease bundlePlayRelease assembleSolana_dappRelease)
error_trap 'android build'

github_release_upload \
    "com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/play/release/com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.apk"

github_release_upload \
    "com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.aab" \
    "$BUILD_HOME/android/app/app/build/outputs/bundle/playRelease/com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.aab"

github_release_upload \
    "com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-solana_dapp-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/solana_dapp/release/com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-solana_dapp-release.apk"

if [ "$BUILD_OUT" ]; then
    (mkdir -p "$BUILD_OUT/apk" &&
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} "$BUILD_OUT/apk" \;)
    error_trap 'android local copy'
fi

# FIXME apple archive and upload to internal testflight
# FIXME android play release to play internal testing

builder_message "android \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


# Github / Ungoogle
# note for F-Droid, the -ungoogle tag should be aliased to -fdroid to trigger their build
(cd $BUILD_HOME/android &&
    git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android prepare ungoogle version branch'
(cd $BUILD_HOME/android &&
    echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
" > app/local.properties &&
    git add app/local.properties -f &&
    $BUILD_SED -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/app/build.gradle &&
    $BUILD_SED -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/settings.gradle)
error_trap 'android edit ungoogle settings'
(cd $BUILD_HOME/android && 
    git add . && 
    git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && 
    git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android ungoogle push branch'

(cd $BUILD_HOME && 
    git add . && 
    git commit -m "$HOST build ungoogle" && 
    git push)
error_trap 'push ungoogle branch'
(cd $BUILD_HOME && 
    git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && 
    git push origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'push ungoogle tag'

# build in the fdroid server context
# ideally this should not be required, but there are some small differences in the android artifacts apparently due to build environment (macos/arm versus linux/amd perhaps)
# FIXME - there is currently a bug on apple m4 that causes an "invalid instruction" error when using the docker apple virtualization framework
#         see https://github.com/golang/go/issues/71434
#         The docker vmm framework is a work around, but it is very slow for amd64.
#         Run on an m1 or intel mac for now.
(cd $BUILD_HOME &&
    docker run --rm -u vagrant \
        --entrypoint /urnetwork/build/fdroid/build.sh \
        -v $WARP_HOME/release:/urnetwork/release:z \
        -v $BUILD_HOME:/urnetwork/build:Z \
        registry.gitlab.com/fdroid/fdroidserver:buildserver)
error_trap 'android github build'

github_release_upload \
    "com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-github-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/github/release/com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-github-release.apk"

if [ "$BUILD_OUT" ]; then
    (mkdir -p "$BUILD_OUT/apk-github" && 
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} "$BUILD_OUT/apk-github" \;)
    error_trap 'android github local copy'
fi

# Upload releases to testing channels

builder_message "android github \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


github_create_release
builder_message "release \`${WARP_VERSION}-${WARP_VERSION_CODE}\` complete - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


# Warp services
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV warp/config-updater/Makefile)
error_trap 'warpctl build config-updater'
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV warp/lb/Makefile)
error_trap 'warpctl build lb'
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV server${GO_MOD_SUFFIX}/taskworker/Makefile)
error_trap 'warpctl build taskworker'
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV server${GO_MOD_SUFFIX}/api/Makefile)
error_trap 'warpctl build api'
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV server${GO_MOD_SUFFIX}/connect/Makefile)
error_trap 'warpctl build connect'
(cd $BUILD_HOME && $BUILD_WARPCTL build $BUILD_ENV web/Makefile)
error_trap 'warpctl build web'
if [ $BUILD_ENV = 'main' ]; then
    (cd $BUILD_HOME && $BUILD_WARPCTL build community connect${GO_MOD_SUFFIX}/provider/Makefile)
    error_trap 'warpctl build community provider'
fi


builder_message "${BUILD_ENV}[0%] services: \`\`\`$($BUILD_WARPCTL ls versions $BUILD_ENV --sample)\`\`\`"


$BUILD_WARPCTL deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

$BUILD_WARPCTL deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    $BUILD_WARPCTL deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older --timeout=0
    builder_message "community[25%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[25%] services: \`\`\`$($BUILD_WARPCTL ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


$BUILD_WARPCTL deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

$BUILD_WARPCTL deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    $BUILD_WARPCTL deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older --timeout=0
    builder_message "community[50%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[50%] services: \`\`\`$($BUILD_WARPCTL ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


$BUILD_WARPCTL deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

$BUILD_WARPCTL deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    $BUILD_WARPCTL deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older --timeout=0
    builder_message "community[75%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[75%] services: \`\`\`$($BUILD_WARPCTL ls versions $BUILD_ENV --sample)\`\`\`"


sleep $STAGE_SECONDS


$BUILD_WARPCTL deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

$BUILD_WARPCTL deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
$BUILD_WARPCTL deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    $BUILD_WARPCTL deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older --timeout=0
    builder_message "community[100%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[100%] services: \`\`\`$($BUILD_WARPCTL ls versions $BUILD_ENV --sample)\`\`\`"


builder_message "Build all \`${WARP_VERSION}-${WARP_VERSION_CODE}\` ... done! Enjoy :) - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"
