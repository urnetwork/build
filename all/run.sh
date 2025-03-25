#!/usr/bin/env bash

error_trap () {
	code=$?
	if [ $code != 0 ]; then
		echo "error($code): $1" >&2
		exit $code
	fi
}

BUILD_HOME=..
BUILD_ENV=main

warpctl stage version next release --message="$HOST build all"

WARP_VERSION=`warpctl ls version`
WARP_VERSION_CODE=`warpctl ls version-code`

echo "Build all ${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd $BUILD_HOME/connect && git checkout main && git pull --recurse-submodules)
error_trap 'pull connect'
(cd $BUILD_HOME/sdk && git checkout main && git pull --recurse-submodules)
error_trap 'pull sdk'
(cd $BUILD_HOME/android && git checkout main && git pull --recurse-submodules)
error_trap 'pull android'
(cd $BUILD_HOME/apple && git checkout main && git pull --recurse-submodules)
error_trap 'pull apple'
(cd $BUILD_HOME/server && git checkout main && git pull --recurse-submodules)
error_trap 'pull server'
(cd $BUILD_HOME/web && git checkout main && git pull --recurse-submodules)
error_trap 'pull web'
(cd $BUILD_HOME/warp && git checkout main && git pull --recurse-submodules)
error_trap 'pull warp'


(cd $BUILD_HOME/connect && ./test.sh)
error_trap 'connect tests'
(cd $BUILD_HOME/sdk && ./test.sh)
error_trap 'sdk tests'
(cd $BUILD_HOME/server && ./test.sh)
error_trap 'server tests'
(cd $BUILD_HOME/server/connect && ./test.sh)
error_trap 'server connect tests'


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
	sed -i "s|\(MARKETING_VERSION *= *\).*;|\1${WARP_VERSION};|g" app/app.xcodeproj/project.pbxproj &&
	sed -i "s|\(CURRENT_PROJECT_VERSION *= *\).*;|\1${WARP_VERSION_CODE};|g" app/app.xcodeproj/project.pbxproj)
error_trap 'apple edit settings'

(cd $BUILD_HOME/android &&
	echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
pwsdk.maven.username=urnetwork-ops
pwsdk.maven.password=xxx
" > android/app/local.properties)
error_trap 'android edit settings'

# put a temporary changelog in place
echo "Continuous build" > metadata/en-US/changelogs/${WARP_VERSION_CODE}.txt
error_trap 'android changelog'


(cd $BUILD_HOME/connect && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'connect push branch'
(cd $BUILD_HOME/sdk && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'sdk push branch'
(cd $BUILD_HOME/android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'android push branch'
(cd $BUILD_HOME/apple && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'apple push branch'
(cd $BUILD_HOME/server && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'server push branch'
(cd $BUILD_HOME/web && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'web push branch'
(cd $BUILD_HOME/warp && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'warp push branch'


(git add . && git commit -m "$HOST build all" && git push)
error_trap 'push branch'
(git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE} -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'push tag'



# FIXME apple archive and upload to internal testflight
# FIXME android github release and upload to github release
# FIXME android play release to play internal testing



# F-Droid
(cd $BUILD_HOME/android && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android prepare ungoogle version branch'
(cd $BUILD_HOME/android &&
	echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
" > android/app/local.properties &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/build.gradle &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' gradle.settings)
error_trap 'android edit ungoogle settings'
(cd $BUILD_HOME/android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android ungoogle push branch'

# this should be manually edited and the <version>-ungoogle tag updated before submitting an fdroiddata merge

(git add . && git commit -m "$HOST build ungoogle" && git push)
error_trap 'push ungoogle branch'
(git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'push ungoogle tag'


# Warp services
warpctl build $BUILD_ENV server/taskworker/Makefile
warpctl build $BUILD_ENV server/api/Makefile
warpctl build $BUILD_ENV server/connect/Makefile
warpctl build $BUILD_ENV web/Makefile
warpctl build $BUILD_ENV warp/config-updater/Makefile
warpctl build $BUILD_ENV warp/lb/Makefile


warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older

sleep 60

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older

sleep 60

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older

sleep 60

warpctl deploy $BUILD_ENV taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy $BUILD_ENV config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
