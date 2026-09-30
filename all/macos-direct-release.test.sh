#!/usr/bin/env bash
# Proves the macOS direct download is released the only way a non-App-Store
# Mac app can be: the URnetworkDirect scheme exported with the Developer ID
# ExportOptions, notarized and stapled, wrapped in a signed + notarized DMG,
# Gatekeeper-assessed, and only then attached to the GitHub release as
# URnetwork-<version>-macos.dmg. The App Store block before it stays intact
# (its pkg still uploads to App Store Connect and is still never published;
# see macos-release-asset.test.sh). Step fatality and artifact gating are the
# Go component tests' job (component_required_test.go, "macos-direct").
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
run_sh="$here/run.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

zsh -n "$run_sh" || fail "run.sh does not parse"

# one region, after the App Store macOS block, before the desktop apps
store_line=$(grep -En 'xcrun altool .*--upload-app --file build/URnetwork\.pkg -t macos' "$run_sh" | cut -d: -f1 | head -n 1)
region_line=$(grep -En '^# macOS direct download:' "$run_sh" | cut -d: -f1)
desktop_line=$(grep -En '^# Desktop apps:' "$run_sh" | cut -d: -f1)
[ -n "$store_line" ] || fail "the App Store macOS pkg upload is gone"
[ "$(echo "$region_line" | wc -l | tr -d ' ')" = 1 ] || fail "expected exactly one macOS direct download region"
[ "$store_line" -lt "$region_line" ] && [ "$region_line" -lt "$desktop_line" ] ||
  fail "the macOS direct download region is not between the App Store macOS block and the desktop apps"
region=$(sed -n "${region_line},${desktop_line}p" "$run_sh")
sed -n "$((region_line - 1))p" "$run_sh" | grep -q '^# =====' || fail "the region lacks its own separator"

# the ordered chain, every step inside the region
expect() {
  echo "$region" | grep -Eq "$1" || fail "missing step: $2"
}
expect 'xcodebuild -scheme URnetworkDirect clean' "clean of the direct scheme"
expect 'xcodebuild archive .* -scheme URnetworkDirect -archivePath build-direct\.xcarchive -destination generic/platform=macOS' "archive of the direct scheme"
expect 'xcodebuild archive .* -exportArchive -exportOptionsPlist ExportOptions-DeveloperID\.plist -archivePath build-direct\.xcarchive -exportPath build/direct' "Developer ID export"
expect 'require_build_artifacts build/direct/URnetwork\.app/Contents/MacOS/URnetwork' "app executable gate"
expect 'macos_notarize_and_staple build/direct/URnetwork\.app' "app notarization + staple"
expect 'ln -s /Applications build/direct-dmg/Applications' "Applications symlink in the DMG"
expect 'hdiutil create .* -format UDZO "build/\$MACOS_DIRECT_DMG"' "UDZO DMG"
expect 'codesign --force --timestamp --sign "\$MACOS_DIRECT_IDENTITY" "build/\$MACOS_DIRECT_DMG"' "DMG codesign with the Developer ID identity"
expect 'macos_notarize_and_staple "build/\$MACOS_DIRECT_DMG"' "DMG notarization + staple"
expect 'spctl --assess --type open --context context:primary-signature -vv "build/\$MACOS_DIRECT_DMG"' "DMG Gatekeeper assessment"
expect 'spctl --assess --type execute -vv build/direct/URnetwork\.app' "app Gatekeeper assessment"
expect '^github_release_upload "URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.dmg" "\$BUILD_HOME/apple/app/build/\$MACOS_DIRECT_DMG"' "DMG release upload"
expect '^builder_message "macos direct download' "builder message"
echo "$region" | grep -Eq '^MACOS_DIRECT_DMG="URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.dmg"' || fail "DMG name is not URnetwork-<version>-macos.dmg"

# each chain is trapped, in order: identity, build+notarize, dmg, upload
echo "$region" | grep -E '^error_trap ' | tr '\n' '|' |
  grep -q "error_trap 'macos direct: Developer ID Application identity'|error_trap 'macos direct build and notarize'|error_trap 'macos direct dmg'|" ||
  fail "the region's error traps changed"
[ "$(echo "$region" | grep -c '^github_release_upload ')" = 1 ] || fail "the region must publish exactly one asset"
echo "$region" | grep -E '^github_release_upload ' | grep -q '\.pkg' && fail "the region publishes a pkg"

# the notarization helper: explicit API key, --wait, an Accepted verdict, staple
helper=$(sed -n '/^macos_notarize_and_staple () {/,/^}/p' "$run_sh")
[ -n "$helper" ] || fail "macos_notarize_and_staple helper is missing"
echo "$helper" | grep -Eq 'xcrun notarytool submit "\$submission" --key "\$APPLE_API_KEY_P8" --key-id "\$APPLE_API_KEY" --issuer "\$APPLE_API_ISSUER" --wait' || fail "notarytool submit --wait with the API key"
echo "$helper" | grep -q "grep -q 'status: Accepted'" || fail "notarization verdict is not required to be Accepted"
echo "$helper" | grep -q 'xcrun stapler staple "\$target"' || fail "stapler staple"
echo "$helper" | grep -q 'ditto -c -k --keepParent "\$target" "\$submission"' || fail "the app is zipped for submission"
# the .p8 comes from the XCODEBUILD_AUTH discovery loop
grep -q 'APPLE_API_KEY_P8="\$d/AuthKey_\${APPLE_API_KEY}.p8"' "$run_sh" || fail "APPLE_API_KEY_P8 is not set by the p8 discovery loop"

# the Developer ID identity is proved before the builds start and in the region
gate=$(sed -n '/^macos_developer_id_identity () {/,/^}/p' "$run_sh")
echo "$gate" | grep -q 'security find-identity -v -p codesigning | grep "Developer ID Application"' || fail "identity gate does not look for Developer ID Application"
echo "$gate" | grep -q 'make-apple-dist-identity.sh developer-id' || fail "identity gate does not say how to assemble the identity"
[ "$(grep -c '^MACOS_DIRECT_IDENTITY=\$(macos_developer_id_identity)$' "$run_sh")" = 2 ] || fail "identity gate must run once early and once in the region"
early_line=$(grep -n '^MACOS_DIRECT_IDENTITY=\$(macos_developer_id_identity)$' "$run_sh" | head -n 1 | cut -d: -f1)
first_build_line=$(grep -n '^warpctl stage version' "$run_sh" | head -n 1 | cut -d: -f1)
[ "$early_line" -lt "$first_build_line" ] || fail "the early identity gate runs after the release has started"

echo "macos-direct-release: OK"
