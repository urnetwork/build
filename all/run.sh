#!/usr/bin/env zsh

# Expect the following env vars to be set:
# WARP_HOME
# APPLE_API_KEY
# APPLE_API_ISSUER
# GITHUB_API_KEY
# (optional) BUILD_OUT
# (optional) SLACK_WEBHOOK


builder_message () {
    echo -n "$1"
    if [ "$SLACK_WEBHOOK" ]; then
        data="{\"text\":$(echo -n $1 | jq -Rsa .), \"blocks\":[{\"type\":\"section\", \"text\":{\"type\":\"mrkdwn\", \"text\":$(echo -n $1 | jq -Rsa .)}}]}"
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

git_main () {
    git diff --quiet && git diff --cached --quiet && git checkout main && git pull --recurse-submodules
}

github_create_release () {
    GITHUB_UPLOAD=`curl -s -L \
        -X POST \
        -H "Accept: application/vnd.github+json" \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        https://api.github.com/repos/urnetwork/build/releases \
        -d "{\"tag_name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"name\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"body\":\"v${WARP_VERSION}-${WARP_VERSION_CODE}\",\"draft\":false,\"prerelease\":false,\"generate_release_notes\":false}"`
    error_trap 'github create release'
    GITHUB_UPLOAD_ID=`echo "$GITHUB_UPLOAD" | jq .id`
    GITHUB_UPLOAD_URL="https://uploads.github.com/repos/urnetwork/build/releases/$GITHUB_UPLOAD_ID/assets"
    echo "github upload to $GITHUB_UPLOAD_URL"
}

github_release_upload () {
    curl -s -o /dev/null -L -X POST \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        -H "Content-Type: application/octet-stream" \
        -H "Authorization: Bearer $GITHUB_API_KEY" \
        "$GITHUB_UPLOAD_URL?name=$1" \
        --data-binary "@$2"
    error_trap "github release upload $1"
}

export BUILD_HOME=`realpath ..`
export BUILD_ENV=main
export BUILD_SED=gsed
export BRINGYOUR_HOME=`realpath ..`
export STAGE_SECONDS=1


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


warpctl stage version next release --message="$HOST build all"
error_trap 'warpctl stage version'

export WARP_VERSION=`warpctl ls version`
error_trap 'warpctl version'
export WARP_VERSION_CODE=`warpctl ls version-code`
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
pwsdk.maven.password=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
" > app/local.properties &&
    git add app/local.properties -f)
error_trap 'android edit settings'

# put a temporary changelog in place
(cd $BUILD_HOME && echo "Continuous build" > metadata/en-US/changelogs/${WARP_VERSION_CODE}.txt)
error_trap 'android changelog'


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
        mv "$temp" v${GO_MOD_VERSION}
    fi
}

(cd $BUILD_HOME/connect/protocol && 
    go_mod_edit_module github.com/urnetwork/connect/protocol &&
    go_mod_fork)
(cd $BUILD_HOME/connect &&
    go_mod_edit_module github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/connect/protocol &&
    go_edit_require_subpackages github.com/urnetwork/connect &&
    go mod edit -dropretract '[v0.0.1, v0.1.13]' &&
    go_mod_fork 'api|protocol')
(cd $BUILD_HOME/sdk/build &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/connect/protocol &&
    go_mod_edit_require github.com/urnetwork/sdk)
(cd $BUILD_HOME/sdk &&
    go_mod_edit_module github.com/urnetwork/sdk &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/connect/protocol &&
    go_edit_require_subpackages github.com/urnetwork/sdk &&
    go_mod_fork 'build')
(cd $BUILD_HOME/server &&
    go_mod_edit_module github.com/urnetwork/server &&
    go_mod_edit_require github.com/urnetwork/connect &&
    go_mod_edit_require github.com/urnetwork/connect/protocol &&
    go_edit_require_subpackages github.com/urnetwork/server &&
    go_mod_fork)


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

git_tag_submodule () {
    git push --delete origin refs/tags/$1/v${WARP_VERSION}-${WARP_VERSION_CODE} &&
    git tag -a $1/v${WARP_VERSION}-${WARP_VERSION_CODE} -m "$1/${WARP_VERSION}-${WARP_VERSION_CODE}" &&
    git push origin refs/tags/$1/v${WARP_VERSION}-${WARP_VERSION_CODE}
}


(cd $BUILD_HOME/connect && 
    git_commit &&
    git_tag &&
    git_tag_submodule protocol)
error_trap 'connect push branch'
(cd $BUILD_HOME/sdk &&
    git_commit &&
    git_tag)
error_trap 'sdk push branch'
(cd $BUILD_HOME/android && 
    git_commit &&
    git_tag)
error_trap 'android push branch'
(cd $BUILD_HOME/apple && 
    git_commit &&
    git_tag)
error_trap 'apple push branch'
(cd $BUILD_HOME/server &&
    git_commit &&
    git_tag)
error_trap 'server push branch'
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


# Upload releases to testing channels


github_create_release


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


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=iOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=iOS &&
    xcrun altool --validate-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --upload-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
error_trap 'ios deploy'

github_release_upload "URnetwork-${WARP_VERSION}-${WARP_VERSION_CODE}.ipa" "$BUILD_HOME/apple/app/build/URnetwork.ipa"

builder_message "ios \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -allowProvisioningUpdates -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=macOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=macOS &&
    xcrun altool --validate-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --upload-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
error_trap 'macos deploy'

github_release_upload "URnetwork-${WARP_VERSION}-${WARP_VERSION_CODE}.pkg" "$BUILD_HOME/apple/app/build/URnetwork.pkg"

builder_message "macos \`${WARP_VERSION}-${WARP_VERSION_CODE}\` available - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd $BUILD_HOME/android/app &&
    ./gradlew clean assemblePlayRelease assembleSolana_dappRelease)
error_trap 'android build'

github_release_upload \
    "com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.apk" \
    "$BUILD_HOME/android/app/app/build/outputs/apk/play/release/com.bringyour.network-${WARP_VERSION}-${WARP_VERSION_CODE}-play-release.apk"

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


# Github / F-Droid
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

# this should be manually edited and the <version>-ungoogle tag updated before submitting an fdroiddata merge

(cd $BUILD_HOME && 
    git add . && 
    git commit -m "$HOST build ungoogle" && 
    git push)
error_trap 'push ungoogle branch'
(cd $BUILD_HOME && 
    git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && 
    git push origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'push ungoogle tag'

(cd $BUILD_HOME/android/app &&
    ./gradlew clean assembleGithubRelease)
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
if [ $BUILD_ENV = 'main' ]; then
    (cd $BUILD_HOME && warpctl build community connect${GO_MOD_SUFFIX}/provider/Makefile)
    error_trap 'warpctl build community provider'
fi


builder_message "${BUILD_ENV}[0%] services: \`\`\`$(warpctl ls versions $BUILD_ENV)\`\`\`"


warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "${BUILD_ENV}[25%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older --timeout=0
    builder_message "community[25%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[25%] services: \`\`\`$(warpctl ls versions $BUILD_ENV)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "${BUILD_ENV}[50%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older --timeout=0
    builder_message "community[50%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[50%] services: \`\`\`$(warpctl ls versions $BUILD_ENV)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "${BUILD_ENV}[75%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older --timeout=0
    builder_message "community[75%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[75%] services: \`\`\`$(warpctl ls versions $BUILD_ENV)\`\`\`"


sleep $STAGE_SECONDS


warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] lb \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] config-updater \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] taskworker \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] api \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] connect \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "${BUILD_ENV}[100%] web \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older --timeout=0
    builder_message "community[100%] provider \`${WARP_VERSION}-${WARP_VERSION_CODE}\` deployed (only older)"
fi

builder_message "${BUILD_ENV}[100%] services: \`\`\`$(warpctl ls versions $BUILD_ENV)\`\`\`"


builder_message "Build all \`${WARP_VERSION}-${WARP_VERSION_CODE}\` ... done! Enjoy :) - https://github.com/urnetwork/build/releases/tag/v${WARP_VERSION}-${WARP_VERSION_CODE}"
