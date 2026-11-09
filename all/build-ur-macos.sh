#!/usr/bin/env zsh
# The macOS half of ur's release (DESIGN.md §4.10, D-CORE-12; REPO-M1-02; plan-REPO-small-readings #7
# and #8). Per architecture:
#
#   1. ur.app, id network.ur.cli: Contents/Info.plist, Contents/MacOS/ur and the `ur Download` profile
#      as Contents/embedded.provisionprofile.
#   2. codesign with the Developer ID Application identity, the hardened runtime, a timestamp and
#      ur/macos/ur.entitlements; codesign --verify; notarize and staple (macos-notarize.zsh, run.sh's
#      own); spctl --assess --type execute.
#   3. ur-<version>-darwin-<arch>.tar.gz of the stapled bundle, by urarchive, with LICENSE and NOTICE.
#   4. ur-<version>-darwin-<arch>.pkg: pkgbuild of the bundle at /usr/local/lib/ur/ur.app with a
#      postinstall that links /usr/local/bin/ur and /usr/local/bin/git-remote-ur to its executable;
#      productbuild; productsign with the Developer ID Installer identity; notarize and staple;
#      spctl --assess --type install.
#
# Traps: a restricted entitlement outside a bundle with its profile kills the process at launch, so ur is
# never a bare Mach-O, and the two names on PATH are links, never copies. Staple after signing and pack
# after stapling: re-signing or stripping after that breaks the seal. The .pkg needs the Installer
# certificate, not the Application one. Signing needs run.sh's per-run keychain (REMOTEBUILD.md), so a
# build over plain ssh fails with errSecInternalComponent. Every step is fatal.
#
# Inputs (env), set by build-ur.sh: UR_BIN_DIR, OUT_DIR, BUILD_HOME, EXTERNAL_WARP_VERSION,
# VERSION_BASE, VERSION_RELEASE, GO_MOD_VERSION, SOURCE_DATE_EPOCH, URARCHIVE; and for signing:
#   APPLE_API_KEY_P8, APPLE_API_KEY, APPLE_API_ISSUER   notarytool's key, as run.sh finds it
#   UR_APPLICATION_IDENTITY   the Developer ID Application identity's SHA-1 (default: the keychains')
#   UR_INSTALLER_IDENTITY     the Developer ID Installer identity's SHA-1 (default: the keychains')
#   MACOS_PROFILES_INSTALL_DIR   where Xcode keeps profiles (default: Xcode's own)
#
# SPDX-License-Identifier: MPL-2.0

emulate -R zsh
setopt err_exit pipe_fail no_unset

here="${0:A:h}"
source "$here/macos-notarize.zsh"
(( $+functions[builder_message] )) || builder_message () { print -u2 -- "$1" }

ur_macos_die () {
    print -u2 -- "ERROR: $*"
    exit 1
}

for variable in UR_BIN_DIR OUT_DIR BUILD_HOME EXTERNAL_WARP_VERSION VERSION_BASE VERSION_RELEASE GO_MOD_VERSION SOURCE_DATE_EPOCH URARCHIVE; do
    [[ -n "${(P)variable:-}" ]] || ur_macos_die "set $variable (build-ur.sh does)"
done
ur_profile_name="ur Download"
MACOS_PROFILES_INSTALL_DIR="${MACOS_PROFILES_INSTALL_DIR:-$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles}"
sidecar_module="$BUILD_HOME/sidecar"
[[ -f "$BUILD_HOME/sidecar/v$GO_MOD_VERSION/go.mod" ]] && sidecar_module="$BUILD_HOME/sidecar/v$GO_MOD_VERSION"

application_identity="${UR_APPLICATION_IDENTITY:-}"
if [[ -z "$application_identity" ]]; then
    application_identity=$(security find-identity -v -p codesigning | grep "Developer ID Application" | head -n 1 | awk '{ print $2 }') || true
fi
[[ -n "$application_identity" ]] || ur_macos_die "no 'Developer ID Application' identity in the signing keychains, so ur.app cannot be signed (REMOTEBUILD.md)"
installer_identity="${UR_INSTALLER_IDENTITY:-}"
if [[ -z "$installer_identity" ]]; then
    installer_identity=$(security find-identity -v | grep "Developer ID Installer" | head -n 1 | awk '{ print $2 }') || true
fi
[[ -n "$installer_identity" ]] || ur_macos_die "no 'Developer ID Installer' identity in the signing keychains, so ur's .pkg cannot be signed (REMOTEBUILD.md)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# the installed `ur Download` profile, unexpired; run.sh proves at its start that it was issued for the
# Developer ID Application identity (macos_require_direct_profiles)
profile=""
for candidate in "$MACOS_PROFILES_INSTALL_DIR"/*.provisionprofile(N); do
    security cms -D -i "$candidate" > "$work/profile.plist" 2>/dev/null || continue
    [[ "$(plutil -extract Name raw -o - "$work/profile.plist" 2>/dev/null)" == "$ur_profile_name" ]] || continue
    expires=$(plutil -extract ExpirationDate raw -o - "$work/profile.plist" 2>/dev/null) || continue
    [[ "$expires" > "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ]] || continue
    profile="$candidate"
    break
done
[[ -n "$profile" ]] || ur_macos_die "no unexpired \"$ur_profile_name\" profile in $MACOS_PROFILES_INSTALL_DIR, so ur.app has none to embed"

for arch in amd64 arm64; do
    case "$arch" in
        amd64) host_architecture=x86_64 ;;
        arm64) host_architecture=arm64 ;;
    esac
    binary="$UR_BIN_DIR/darwin/$arch/ur"
    [[ -s "$binary" ]] || ur_macos_die "no darwin/$arch ur at $binary"
    stage="$work/$arch"
    app="$stage/ur.app"
    mkdir -p "$app/Contents/MacOS"
    sed -e "s/@VERSION_BASE@/$VERSION_BASE/g" -e "s/@VERSION_RELEASE@/$VERSION_RELEASE/g" \
        "$here/ur/macos/Info.plist" > "$app/Contents/Info.plist"
    cp "$binary" "$app/Contents/MacOS/ur"
    chmod 0755 "$app/Contents/MacOS/ur"
    cp "$profile" "$app/Contents/embedded.provisionprofile"

    print -r -- ">>> ur.app for darwin/$arch"
    codesign --force --options runtime --timestamp --entitlements "$here/ur/macos/ur.entitlements" \
        --sign "$application_identity" "$app"
    codesign --verify --strict --verbose=2 "$app"
    macos_notarize_and_staple "$app"
    spctl --assess --type execute -vv "$app"

    COPYFILE_DISABLE=1 "$URARCHIVE" tar -o "$OUT_DIR/ur-$EXTERNAL_WARP_VERSION-darwin-$arch.tar.gz" -mtime "$SOURCE_DATE_EPOCH" \
        "ur.app=$app" "LICENSE=$sidecar_module/LICENSE" "NOTICE=$sidecar_module/NOTICE"

    root="$stage/root"
    mkdir -p "$root/usr/local/lib/ur" "$stage/scripts" "$stage/packages"
    # ditto keeps the bundle's signature and staple as they are
    ditto "$app" "$root/usr/local/lib/ur/ur.app"
    cp "$here/ur/macos/postinstall" "$stage/scripts/postinstall"
    chmod 0755 "$stage/scripts/postinstall"
    # the bundle installs where it is put, never relocated to another copy of network.ur.cli
    print -r -- '<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><array><dict>
<key>BundleHasStrictIdentifier</key><true/>
<key>BundleIsRelocatable</key><false/>
<key>BundleIsVersionChecked</key><false/>
<key>BundleOverwriteAction</key><string>upgrade</string>
<key>RootRelativeBundlePath</key><string>usr/local/lib/ur/ur.app</string>
</dict></array></plist>' > "$stage/component.plist"
    pkgbuild --root "$root" --component-plist "$stage/component.plist" --identifier network.ur.cli.pkg \
        --version "$VERSION_BASE" --install-location / --scripts "$stage/scripts" "$stage/packages/ur-component.pkg"
    sed -e "s/@ARCH@/$host_architecture/g" -e "s/@VERSION_BASE@/$VERSION_BASE/g" \
        "$here/ur/macos/distribution.xml" > "$stage/distribution.xml"
    productbuild --distribution "$stage/distribution.xml" --package-path "$stage/packages" "$stage/ur-unsigned.pkg"
    productsign --sign "$installer_identity" --timestamp "$stage/ur-unsigned.pkg" "$stage/ur.pkg"
    macos_notarize_and_staple "$stage/ur.pkg"
    spctl --assess --type install -vv "$stage/ur.pkg"
    [[ -s "$stage/ur.pkg" ]] || ur_macos_die "productsign wrote no ur.pkg for darwin/$arch"
    cp "$stage/ur.pkg" "$OUT_DIR/ur-$EXTERNAL_WARP_VERSION-darwin-$arch.pkg"
done
