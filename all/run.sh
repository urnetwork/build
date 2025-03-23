

warpctl stage version next release --message="$HOST build all"

WARP_VERSION=`warpctl ls version`
WARP_VERSION_CODE=`warpctl ls version-code`


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


(cd connect && ./test.sh)
error_trap 'connect tests'
(cd sdk && ./test.sh)
error_trap 'sdk tests'
(cd server && ./test.sh)
error_trap 'server tests'
(cd server/connect && ./test.sh)
error_trap 'server connect tests'


(cd connect && git checkout -b v${WARP_VERSION})
error_trap 'connect prepare version branch'
(cd sdk && git checkout -b v${WARP_VERSION})
error_trap 'sdk prepare version branch'
(cd android && git checkout -b v${WARP_VERSION})
error_trap 'android prepare version branch'
(cd apple && git checkout -b v${WARP_VERSION})
error_trap 'apple prepare version branch'
(cd server && git checkout -b v${WARP_VERSION})
error_trap 'server prepare version branch'
(cd web && git checkout -b v${WARP_VERSION})
error_trap 'web prepare branch'


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

# FIXME commit and push all branches

git add .
git commit -m "$HOST build all"
# FIXME create tag, push tag to origin


# FIXME apple archive and upload to internal testflight
# FIXME android github release and upload to github release
# FIXME android play release to play internal testing



# F-Droid
(cd android && git checkout -b v${WARP_VERSION}-ungoogle)
error_trap 'android prepare ungoogle version branch'
(cd android &&
	echo -n "
warp.version=$WARP_VERSION
warp.version_code=$WARP_VERSION_CODE
" > android/app/local.properties &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' app/build.gradle &&
	sed -i 's|.*/\* *build: *google *\*/.*|/*ungoogled*/|g' gradle.settings)
error_trap 'android edit ungoogle settings'

# this should be manually edited and the <version>-ungoogle tag updated before submitting an fdroiddata merge

# FIXME commit and push all branches
git checkout -b v${WARP_VERSION}-ungoogle
git add .
git commit -m "$HOST build fdroid"
# FIXME create tag, push tag to origin



# Warp services
warpctl build main server/taskworker/Makefile
warpctl build main server/api/Makefile
warpctl build main server/connect/Makefile
warpctl build main web/Makefile


warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=25 --only-older

sleep N

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=50 --only-older

sleep N

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=75 --only-older

sleep N

warpctl deploy main taskworker ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main api ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main connect ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older
warpctl deploy main web ${WARP_VERSION}+${WARP_VERSION_CODE} --percent=100 --only-older

