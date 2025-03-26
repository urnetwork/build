#!/usr/bin/env bash

# Expect the following env vars to be set:
# APPLE_API_KEY
# APPLE_API_ISSUER
# (optional) BUILD_OUT
# (optional) SLACK_WEBHOOK


builder_message () {
    echo $1
    if [ $SLACK_WEBHOOK ]; then
        curl -X POST -H 'Content-type: application/json' --data "{\"text\":$(echo $1 | jq -Rsa .)}" $SLACK_WEBHOOK
    fi
}

error_trap () {
    code=$?
    if [ $code != 0 ]; then
        echo "error($code): $1" >&2
        builder_message "error($code): $1"
        exit $code
    fi
}

export BUILD_HOME=`realpath ..`
export BUILD_ENV=main
export BUILD_SED=gsed
export BRINGYOUR_HOME=`realpath ../..`

warpctl stage version next release --message="$HOST build all"
error_trap 'warpctl stage version'

export WARP_VERSION=`warpctl ls version`
error_trap 'warpctl version'
export WARP_VERSION_CODE=`warpctl ls version-code`
error_trap 'warpctl version code'

builder_message "Build all ${WARP_VERSION}-${WARP_VERSION_CODE}"

# FIXME
# (cd $BUILD_HOME && git stash -u && git checkout main && git pull --recurse-submodules)
# error_trap 'pull'

(cd $BUILD_HOME/connect && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull connect'
(cd $BUILD_HOME/sdk && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull sdk'
(cd $BUILD_HOME/android && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull android'
(cd $BUILD_HOME/apple && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull apple'
(cd $BUILD_HOME/server && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull server'
(cd $BUILD_HOME/web && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull web'
(cd $BUILD_HOME/warp && git stash -u && git checkout main && git pull --recurse-submodules)
error_trap 'pull warp'


# (cd $BUIL[D_HOME/connect && ./test.sh)
# error_trap 'connect tests'
# # FIXME
# # (cd $BUILD_HOME/connect/provider && ./test.sh)
# # error_trap 'connect provider tests'
# (cd $BUILD_HOME/sdk && ./test.sh)
# error_trap 'sdk tests'
# (cd $BUILD_HOME/server && ./test.sh)
# error_trap 'server tests'
# (cd $BUILD_HOME/server/connect && ./test.sh)
# error_trap] 'server connect tests'


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


(cd $BUILD_HOME/connect && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'connect push branch'
(cd $BUILD_HOME/sdk && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'sdk push branch'
(cd $BUILD_HOME/android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'android push branch'
(cd $BUILD_HOME/apple && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'apple push branch'
(cd $BUILD_HOME/server && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'server push branch'
(cd $BUILD_HOME/web && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'web push branch'
(cd $BUILD_HOME/warp && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'warp push branch'


(cd $BUILD_HOME && git add . && git commit -m "$HOST build all" && git push)
error_trap 'push branch'
(cd $BUILD_HOME && git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE} -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'push tag'


(cd $BUILD_HOME/sdk && make)
error_trap 'build sdk'


# Upload releases to testing channels

(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=iOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=iOS &&
    xcrun altool --validate-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --upload-app --file build/URnetwork.ipa -t ios --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
error_trap 'apple ios deploy'
builder_message "apple ios ${WARP_VERSION}-${WARP_VERSION_CODE} available"

(cd $BUILD_HOME/apple/app &&
    xcodebuild -scheme URnetwork clean &&
    xcodebuild archive -workspace app.xcodeproj/project.xcworkspace -config Release -scheme URnetwork -archivePath build.xcarchive -destination generic/platform=macOS &&
    xcodebuild archive -allowProvisioningUpdates -exportArchive -exportOptionsPlist ExportOptions.plist -archivePath build.xcarchive -exportPath build -destination generic/platform=macOS &&
    xcrun altool --validate-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER &&
    xcrun altool --upload-app --file build/URnetwork.pkg -t macos --apiKey $APPLE_API_KEY --apiIssuer $APPLE_API_ISSUER)
error_trap 'apple macos deploy'
builder_message "apple macos ${WARP_VERSION}-${WARP_VERSION_CODE} available"

(cd $BUILD_HOME/android/app &&
    ./gradlew clean &&
    ./gradlew assembleRelease)
error_trap 'android build'

if [ $BUILD_OUT ]; then
    (mkdir -p $BUILD_OUT/apk &&
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} $BUILD_OUT/apk \;)
    error_trap 'android local copy'
    builder_message "android ${WARP_VERSION}-${WARP_VERSION_CODE} available"
fi


# FIXME apple archive and upload to internal testflight
# FIXME android github release and upload to github release
# FIXME android play release to play internal testing





# Github / F-Droid
(cd $BUILD_HOME/android && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
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
(cd $BUILD_HOME/android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android ungoogle push branch'

# this should be manually edited and the <version>-ungoogle tag updated before submitting an fdroiddata merge

(cd $BUILD_HOME && git add . && git commit -m "$HOST build ungoogle" && git push)
error_trap 'push ungoogle branch'
(cd $BUILD_HOME && git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'push ungoogle tag'

(cd $BUILD_HOME/android/app &&
    ./gradlew clean &&
    ./gradlew assembleGithubRelease)
error_trap 'android github build'

if [ $BUILD_OUT ]; then
    (mkdir -p $BUILD_OUT/apk-github && 
        find $BUILD_HOME/android/app/app/build/outputs/apk -iname '*.apk' -exec cp {} $BUILD_OUT/apk-github \;)
    error_trap 'android github local copy'
    builder_message "android github ${WARP_VERSION}-${WARP_VERSION_CODE} available"
fi


# Upload releases to testing channels
# FIXME android github release and upload to github release

builder_message "$BUILD_ENV services: $(warpctl ls versions $BUILD_ENV)"

exit


# Warp services
(cd $BUILD_HOME && warpctl build $BUILD_ENV server/taskworker/Makefile)
error_trap 'warpctl build taskworker'
(cd $BUILD_HOME && warpctl build $BUILD_ENV server/api/Makefile)
error_trap 'warpctl build api'
(cd $BUILD_HOME && warpctl build $BUILD_ENV server/connect/Makefile)
error_trap 'warpctl build connect'
(cd $BUILD_HOME && warpctl build $BUILD_ENV web/Makefile)
error_trap 'warpctl build web'
(cd $BUILD_HOME && warpctl build $BUILD_ENV warp/config-updater/Makefile)
error_trap 'warpctl build config-updater'
# (cd $BUILD_HOME && warpctl build $BUILD_ENV warp/lb/Makefile)
# error_trap 'warpctl build lb'
if [ $BUILD_ENV = 'main' ]; then
    (cd $BUILD_HOME && warpctl build community connect/provider/Makefile)
    error_trap 'warpctl build community provider'
fi

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "$BUILD_ENV[25%] taskworker ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "$BUILD_ENV[25%] api ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "$BUILD_ENV[25%] connect ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "$BUILD_ENV[25%] web ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
# warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
builder_message "$BUILD_ENV[25%] config-updater ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
builder_message "$BUILD_ENV services: $(warpctl ls versions $BUILD_ENV)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
    builder_message "community[25%] provider ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
fi


sleep 7200

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "$BUILD_ENV[50%] taskworker ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "$BUILD_ENV[50%] api ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "$BUILD_ENV[50%] connect ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "$BUILD_ENV[50%] web ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
# warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
builder_message "$BUILD_ENV[50%] config-updater ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
builder_message "$BUILD_ENV services: $(warpctl ls versions $BUILD_ENV)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
fi

sleep 7200

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "$BUILD_ENV[75%] taskworker ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "$BUILD_ENV[75%] api ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "$BUILD_ENV[75%] connect ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "$BUILD_ENV[75%] web ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
# warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
builder_message "$BUILD_ENV[75%] config-updater ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
builder_message "$BUILD_ENV services: $(warpctl ls versions $BUILD_ENV)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
fi

sleep 7200

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "$BUILD_ENV[100%] taskworker ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "$BUILD_ENV[100%] api ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "$BUILD_ENV[100%] connect ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "$BUILD_ENV[100%] web ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
# warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
builder_message "$BUILD_ENV[100%] config-updater ${WARP_VERSION}-${WARP_VERSION_CODE} deployed (only older)"
builder_message "$BUILD_ENV services: $(warpctl ls versions $BUILD_ENV)"
if [ $BUILD_ENV = 'main' ]; then
    warpctl deploy community provider ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
fi

