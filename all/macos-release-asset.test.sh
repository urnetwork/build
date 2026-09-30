#!/usr/bin/env bash
# Proves the release never attaches the Mac App Store export to the GitHub
# release. The macOS URnetwork.pkg is exported with the app-store-connect
# ExportOptions.plist: it is signed for the Mac App Store only and does not
# launch when installed directly, so publishing it as a download ships a
# broken app. It must still be built, validated and uploaded to App Store
# Connect.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
run_sh="$here/run.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# the store export is still produced and uploaded to App Store Connect
grep -Eq 'xcrun altool .*--upload-app --file build/URnetwork\.pkg -t macos' "$run_sh" ||
  fail "macOS pkg is no longer uploaded to App Store Connect"

# no GitHub release upload of anything the apple app build exported as a pkg
if grep -En '^[[:space:]]*github_release_upload[[:space:]].*apple/app/build/[^"]*\.pkg' "$run_sh"; then
  fail "the App Store macOS pkg is attached to the GitHub release"
fi

echo "macos-release-asset: OK"
