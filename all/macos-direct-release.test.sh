#!/usr/bin/env bash
# Proves the macOS direct download is released the only way a non-App-Store
# Mac app can be: the URnetworkDirect scheme exported with the Developer ID
# ExportOptions, notarized and stapled, wrapped in a signed + notarized DMG,
# Gatekeeper-assessed, and only then attached to the GitHub release: first the
# stapled app as URnetwork-<version>-macos.zip (ditto --keepParent, the asset
# the app's in-app updater fetches by name), then URnetwork-<version>-macos.dmg
# for humans. Signing is manual: the three Developer ID
# profiles from ~/.provisionprofiles/ are installed and proved up front, and the
# direct archive/export never use -allowProvisioningUpdates. The App Store block before it stays intact
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
expect 'xcodebuild archive -workspace app\.xcodeproj/project\.xcworkspace -config Release -scheme URnetworkDirect -archivePath build-direct\.xcarchive -destination generic/platform=macOS' "archive of the direct scheme"
expect 'xcodebuild archive -exportArchive -exportOptionsPlist ExportOptions-DeveloperID\.plist -archivePath build-direct\.xcarchive -exportPath build/direct' "Developer ID export"
# manual signing: the direct archive/export never talk to the portal, while the
# App Store archive/export above still do
echo "$region" | grep -E 'xcodebuild archive' | grep -Eq 'allowProvisioningUpdates|XCODEBUILD_AUTH' &&
  fail "the direct archive/export must not use -allowProvisioningUpdates (manual signing with installed Developer ID profiles)"
[ "$(grep -c 'xcodebuild archive -allowProvisioningUpdates \$XCODEBUILD_AUTH .* -scheme URnetwork -archivePath build\.xcarchive' "$run_sh")" = 2 ] ||
  fail "the App Store archives lost -allowProvisioningUpdates"
expect 'require_build_artifacts build/direct/URnetwork\.app/Contents/MacOS/URnetwork' "app executable gate"
expect 'macos_notarize_and_staple build/direct/URnetwork\.app' "app notarization + staple"
expect '^    ditto -c -k --keepParent build/direct/URnetwork\.app "build/\$MACOS_DIRECT_ZIP" &&$' "updater zip of the stapled app with --keepParent"
expect 'require_build_artifacts "build/\$MACOS_DIRECT_ZIP"' "zip artifact gate"
expect '^github_release_upload "URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.zip" "\$BUILD_HOME/apple/app/build/\$MACOS_DIRECT_ZIP"' "zip release upload"
echo "$region" | grep -Eq '^MACOS_DIRECT_ZIP="URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.zip"' || fail "zip name is not URnetwork-<version>-macos.zip"
expect 'ln -s /Applications build/direct-dmg/Applications' "Applications symlink in the DMG"
expect 'hdiutil create .* -format UDZO "build/\$MACOS_DIRECT_DMG"' "UDZO DMG"
expect 'codesign --force --timestamp --sign "\$MACOS_DIRECT_IDENTITY" "build/\$MACOS_DIRECT_DMG"' "DMG codesign with the Developer ID identity"
expect 'macos_notarize_and_staple "build/\$MACOS_DIRECT_DMG"' "DMG notarization + staple"
expect 'spctl --assess --type open --context context:primary-signature -vv "build/\$MACOS_DIRECT_DMG"' "DMG Gatekeeper assessment"
expect 'spctl --assess --type execute -vv build/direct/URnetwork\.app' "app Gatekeeper assessment"
expect '^github_release_upload "URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.dmg" "\$BUILD_HOME/apple/app/build/\$MACOS_DIRECT_DMG"' "DMG release upload"
expect '^builder_message "macos direct download' "builder message"
echo "$region" | grep -Eq '^MACOS_DIRECT_DMG="URnetwork-\$\{EXTERNAL_WARP_VERSION\}-macos\.dmg"' || fail "DMG name is not URnetwork-<version>-macos.dmg"

# each chain is trapped, in order: identity, profiles, build+notarize, zip, dmg
echo "$region" | grep -E '^error_trap ' | tr '\n' '|' |
  grep -q "error_trap 'macos direct: Developer ID Application identity'|error_trap 'macos direct: Developer ID provisioning profiles'|error_trap 'macos direct build and notarize'|error_trap 'macos direct zip'|error_trap 'macos direct dmg'|" ||
  fail "the region's error traps changed"
# exactly two assets, the updater zip before the DMG, and both after the app's
# Gatekeeper assessment
[ "$(echo "$region" | grep -c '^github_release_upload ')" = 2 ] || fail "the region must publish exactly two assets (zip, then dmg)"
echo "$region" | grep -E '^github_release_upload ' | cut -d'"' -f2 | tr '\n' '|' |
  grep -q '^URnetwork-${EXTERNAL_WARP_VERSION}-macos.zip|URnetwork-${EXTERNAL_WARP_VERSION}-macos.dmg|$' ||
  fail "the region does not publish the zip and then the dmg: $(echo "$region" | grep -E '^github_release_upload ')"
echo "$region" | grep -E '^github_release_upload ' | grep -q '\.pkg' && fail "the region publishes a pkg"
assess_app_line=$(echo "$region" | grep -n 'spctl --assess --type execute -vv build/direct/URnetwork\.app' | cut -d: -f1)
first_upload_line=$(echo "$region" | grep -n '^github_release_upload ' | head -n 1 | cut -d: -f1)
[ "$(echo "$assess_app_line" | wc -l | tr -d ' ')" = 1 ] && [ "$assess_app_line" -lt "$first_upload_line" ] ||
  fail "the app must pass its Gatekeeper assessment before either asset is published"
zip_line=$(echo "$region" | grep -n 'ditto -c -k --keepParent build/direct/URnetwork\.app "build/\$MACOS_DIRECT_ZIP"' | cut -d: -f1)
staple_line=$(echo "$region" | grep -n 'macos_notarize_and_staple build/direct/URnetwork\.app' | cut -d: -f1)
dmg_line=$(echo "$region" | grep -n 'hdiutil create' | cut -d: -f1)
[ "$staple_line" -lt "$zip_line" ] && [ "$zip_line" -lt "$dmg_line" ] || fail "the updater zip is not made after stapling the app and before the DMG"

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

# manual signing: the three Developer ID profiles come from ~/.provisionprofiles/,
# are installed under their UUID next to the identity import, and every name
# is proved for the signing certificate before the release starts and again
# in the region
grep -q '^MACOS_DIRECT_PROFILE_NAMES=("URnetwork Download" "URnetwork Extension Download" "URnetwork Split Tunnel Download")$' "$run_sh" || fail "required profile names changed"
grep -q '^MACOS_PROFILES_SOURCE_DIR="\$HOME/.provisionprofiles"$' "$run_sh" || fail "profiles are not read from ~/.provisionprofiles"
grep -q '^MACOS_PROFILES_INSTALL_DIR="\$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"$' "$run_sh" || fail "profiles are not installed into Xcode's profile directory"
install_fn=$(sed -n '/^macos_install_provisioning_profiles () {/,/^}/p' "$run_sh")
[ -n "$install_fn" ] || fail "macos_install_provisioning_profiles is missing"
echo "$install_fn" | grep -q 'mkdir -p "\$MACOS_PROFILES_INSTALL_DIR"' || fail "the install step does not create the profile directory"
echo "$install_fn" | grep -q 'cp "\$profile" "\$MACOS_PROFILES_INSTALL_DIR/\$uuid.provisionprofile"' || fail "profiles are not installed as <UUID>.provisionprofile"
problem_fn=$(sed -n '/^macos_profile_problem () {/,/^}/p' "$run_sh")
echo "$problem_fn" | grep -q 'security cms -D -i "\$profile"' || fail "profiles are not decoded with security cms -D"
echo "$problem_fn" | grep -q 'macos_profile_value "\$plist" ExpirationDate' || fail "profile expiry is not checked"
echo "$problem_fn" | grep -q 'macos_profile_value "\$plist" UUID' || fail "profile UUID is not checked"
sed -n '/^macos_profile_signs_with () {/,/^}/p' "$run_sh" | grep -q 'shasum -a 1' || fail "the profile certificate is not compared with the identity SHA-1"
identity_import_line=$(grep -n '^    for identity_p12 in "\$HOME"/.identity\*.p12; do' "$run_sh" | cut -d: -f1)
install_line=$(grep -n '^macos_install_provisioning_profiles$' "$run_sh" | cut -d: -f1)
[ "$(echo "$install_line" | wc -l | tr -d ' ')" = 1 ] || fail "the profile install step must run exactly once"
[ -n "$identity_import_line" ] && [ "$identity_import_line" -lt "$install_line" ] && [ "$install_line" -lt "$early_line" ] ||
  fail "the profile install step is not next to the identity import, before the early identity gate"
[ "$(grep -c '^macos_require_direct_profiles "\$MACOS_DIRECT_IDENTITY"$' "$run_sh")" = 2 ] || fail "profile gate must run once early and once in the region"
early_profile_line=$(grep -n '^macos_require_direct_profiles "\$MACOS_DIRECT_IDENTITY"$' "$run_sh" | head -n 1 | cut -d: -f1)
[ "$early_line" -lt "$early_profile_line" ] && [ "$early_profile_line" -lt "$first_build_line" ] ||
  fail "the early profile gate does not follow the identity gate before the release starts"
echo "$region" | grep -q '^macos_require_direct_profiles "\$MACOS_DIRECT_IDENTITY"$' || fail "the region does not re-prove the profiles"
grep -q 'Profiles -> + -> Distribution: "Developer ID"' "$run_sh" || fail "run.sh does not document how to regenerate the profiles"
grep -q '~/.provisionprofiles/' "$here/make-apple-dist-identity.sh" || fail "make-apple-dist-identity.sh does not document ~/.provisionprofiles/"
grep -q 'URnetwork Extension Download' "$here/make-apple-dist-identity.sh" || fail "make-apple-dist-identity.sh does not name the profiles"
grep -q 'URnetwork Split Tunnel Download' "$here/make-apple-dist-identity.sh" || fail "make-apple-dist-identity.sh does not name the split tunnel profile"

# the install step and gate, run against fixture profiles with a stubbed
# decoder: valid profiles land under their UUID and pass; an expired or
# undecodable file fails by name; a missing, misnamed or foreign-certificate
# profile fails the gate
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/src" "$work/inst"
cert_b64=$(printf 'synthetic developer id certificate' | base64)
cert_sha1=$(printf 'synthetic developer id certificate' | shasum -a 1 | awk '{ print toupper($1) }')
write_profile() {
  cat > "$1" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Name</key><string>$2</string>
<key>UUID</key><string>$3</string>
<key>ExpirationDate</key><date>$4</date>
<key>DeveloperCertificates</key><array><data>$5</data></array>
</dict></plist>
PLIST
}
{
  echo 'builder_message () { echo "message: $1"; }'
  echo 'security () { [ "$1" = cms ] || exit 99; grep -q "<plist" "$4" && cat "$4"; }'
  grep '^MACOS_DIRECT_PROFILE_NAMES=' "$run_sh"
  echo "MACOS_PROFILES_SOURCE_DIR='$work/src'"
  echo "MACOS_PROFILES_INSTALL_DIR='$work/inst'"
  for fn in macos_profile_value macos_profile_problem macos_profile_signs_with macos_install_provisioning_profiles macos_require_direct_profiles; do
    sed -n "/^$fn () {/,/^}/p" "$run_sh"
  done
  echo '"$@"'
} > "$work/harness.zsh"
write_profile "$work/src/download.provisionprofile" "URnetwork Download" 11111111-1111-1111-1111-111111111111 2099-01-01T00:00:00Z "$cert_b64"
write_profile "$work/src/extension.provisionprofile" "URnetwork Extension Download" 22222222-2222-2222-2222-222222222222 2099-01-01T00:00:00Z "$cert_b64"
write_profile "$work/src/splittunnel.provisionprofile" "URnetwork Split Tunnel Download" 44444444-4444-4444-4444-444444444444 2099-01-01T00:00:00Z "$cert_b64"
zsh "$work/harness.zsh" macos_install_provisioning_profiles >/dev/null || fail "valid profiles did not install"
[ -s "$work/inst/11111111-1111-1111-1111-111111111111.provisionprofile" ] && [ -s "$work/inst/22222222-2222-2222-2222-222222222222.provisionprofile" ] &&
  [ -s "$work/inst/44444444-4444-4444-4444-444444444444.provisionprofile" ] ||
  fail "profiles were not installed as <UUID>.provisionprofile"
zsh "$work/harness.zsh" macos_require_direct_profiles "$cert_sha1" >/dev/null || fail "installed profiles did not pass the gate"
if zsh "$work/harness.zsh" macos_require_direct_profiles FFFF >/dev/null; then fail "a profile for another certificate passed the gate"; fi
# the direct app embeds the split tunnel system extension, which signs only with its own profile
mv "$work/inst/44444444-4444-4444-4444-444444444444.provisionprofile" "$work/splittunnel.provisionprofile.saved"
missing_split_tunnel=$(zsh "$work/harness.zsh" macos_require_direct_profiles "$cert_sha1" 2>&1) && fail "a missing split tunnel profile passed the gate"
echo "$missing_split_tunnel" | grep -q 'message: error: provisioning profile "URnetwork Split Tunnel Download" is not installed' ||
  fail "the missing split tunnel profile message does not name the profile: $missing_split_tunnel"
mv "$work/splittunnel.provisionprofile.saved" "$work/inst/44444444-4444-4444-4444-444444444444.provisionprofile"
write_profile "$work/src/extension.provisionprofile" "URnetwork Extension Download" 22222222-2222-2222-2222-222222222222 2020-01-01T00:00:00Z "$cert_b64"
expired=$(zsh "$work/harness.zsh" macos_install_provisioning_profiles 2>&1) && fail "an expired profile installed"
echo "$expired" | grep -q "message: error: provisioning profile $work/src/extension.provisionprofile (\"URnetwork Extension Download\") expired on 2020-01-01T00:00:00Z" ||
  fail "the expired profile message does not name the file: $expired"
echo garbage > "$work/src/extension.provisionprofile"
garbage=$(zsh "$work/harness.zsh" macos_install_provisioning_profiles 2>&1) && fail "an undecodable profile installed"
echo "$garbage" | grep -q "message: error: $work/src/extension.provisionprofile is not a CMS-signed provisioning profile" ||
  fail "the undecodable profile message does not name the file: $garbage"
rm "$work/inst/22222222-2222-2222-2222-222222222222.provisionprofile"
missing=$(zsh "$work/harness.zsh" macos_require_direct_profiles "$cert_sha1" 2>&1) && fail "a missing profile passed the gate"
echo "$missing" | grep -q 'message: error: provisioning profile "URnetwork Extension Download" is not installed' || fail "the missing profile message does not name the profile: $missing"
write_profile "$work/inst/33333333-3333-3333-3333-333333333333.provisionprofile" "URnetwork Extension" 33333333-3333-3333-3333-333333333333 2099-01-01T00:00:00Z "$cert_b64"
if zsh "$work/harness.zsh" macos_require_direct_profiles "$cert_sha1" >/dev/null 2>&1; then fail "a profile with the wrong name passed the gate"; fi

echo "macos-direct-release: OK"
