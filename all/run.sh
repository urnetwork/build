#!/usr/bin/env bash

error_trap () {
	code=$?
	if [ $code != 0 ]; then
		echo $1 >&2
		exit $code
	fi
}

warpctl stage version next release --message="$HOST build all"

WARP_VERSION=`warpctl ls version`
WARP_VERSION_CODE=`warpctl ls version-code`

echo "Build all ${WARP_VERSION}-${WARP_VERSION_CODE}"


(cd connect && git checkout main && git pull --recurse-submodules)
error_trap 'pull connect'
(cd sdk && git checkout main && git pull --recurse-submodules)
error_trap 'pull sdk'
(cd android && git checkout main && git pull --recurse-submodules)
error_trap 'pull android'
(cd apple && git checkout main && git pull --recurse-submodules)
error_trap 'pull android'
(cd server && git checkout main && git pull --recurse-submodules)
error_trap 'pull server'
(cd web && git checkout main && git pull --recurse-submodules)
error_trap 'pull web'
(cd warp && git checkout main && git pull --recurse-submodules)
error_trap 'pull warp'


(cd connect && ./test.sh)
error_trap 'connect tests'
(cd sdk && ./test.sh)
error_trap 'sdk tests'
(cd server && ./test.sh)
error_trap 'server tests'
(cd server/connect && ./test.sh)
error_trap 'server connect tests'


(cd connect && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'connect prepare version branch'
(cd sdk && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'sdk prepare version branch'
(cd android && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'android prepare version branch'
(cd apple && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'apple prepare version branch'
(cd server && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'server prepare version branch'
(cd web && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'web prepare branch'
(cd warp && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'warp prepare branch'


# apple branch, edit xcodeproject

(cd apple &&
	sed -i "s|\(MARKETING_VERSION *= *\).*;|\1${WARP_VERSION};|g" app/app.xcodeproj/project.pbxproj &&
	sed -i "s|\(CURRENT_PROJECT_VERSION *= *\).*;|\1${WARP_VERSION_CODE};|g" app/app.xcodeproj/project.pbxproj)
error_trap 'apple edit settings'

(cd android &&
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


(cd connect && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'connect push branch'
(cd sdk && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'sdk push branch'
(cd android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'android push branch'
(cd apple && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'apple push branch'
(cd server && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'server push branch'
(cd web && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'web push branch'
(cd warp && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'warp push branch'


(git add . && git commit -m "$HOST build all" && git push)
error_trap 'push branch'
(git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE} -m "${WARP_VERSION}-${WARP_VERSION_CODE}" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE})
error_trap 'push tag'



# FIXME apple archive and upload to internal testflight
# FIXME android github release and upload to github release
# FIXME android play release to play internal testing



# F-Droid
(cd android && git checkout -b v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android prepare ungoogle version branch'
(cd android &&
	echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
" > android/app/local.properties &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/build.gradle &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' gradle.settings)
error_trap 'android edit ungoogle settings'
(cd android && git add . && git commit -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push -u origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'android ungoogle push branch'

# this should be manually edited and the <version>-ungoogle tag updated before submitting an fdroiddata merge

(git add . && git commit -m "$HOST build ungoogle" && git push)
error_trap 'push ungoogle branch'
(git tag -a v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle -m "${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle" && git push origin v${WARP_VERSION}-${WARP_VERSION_CODE}-ungoogle)
error_trap 'push ungoogle tag'


# Warp services
warpctl build main server/taskworker/Makefile
warpctl build main server/api/Makefile
warpctl build main server/connect/Makefile
warpctl build main web/Makefile
warpctl build main warp/config-updater/Makefile
warpctl build main warp/lb/Makefile


warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older

sleep 60

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older

sleep 60

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older

sleep 60

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main lb ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main config-updater ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
