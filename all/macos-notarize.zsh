#!/usr/bin/env zsh
# Notarization for the macOS releases: run.sh's direct download and build-ur-macos.sh's ur bundle and
# .pkg (REPO-M1-02) source this, so both submit and staple through one function. It needs
# APPLE_API_KEY_P8, APPLE_API_KEY and APPLE_API_ISSUER, and a builder_message, which run.sh defines.
#
# SPDX-License-Identifier: MPL-2.0

# macos_notarize_and_staple <path> — submit an .app (zipped for the upload) or
# a .dmg to Apple's notary service with the App Store Connect API key, wait for
# the verdict, require "Accepted" (a non-accepted verdict is a failure even
# when notarytool exits 0), then staple the ticket to the path.
macos_notarize_and_staple () {
    local target="$1" submission="$1" verdict verdict_status
    if [ ! -f "$APPLE_API_KEY_P8" ]; then
        builder_message "error: notarytool needs the App Store Connect API key AuthKey_${APPLE_API_KEY}.p8 in ~/.private_keys, ~/private_keys or ~/.appstoreconnect/private_keys"
        return 1
    fi
    if [ -d "$target" ]; then
        submission="${target%.app}-notarize.zip"
        rm -f "$submission"
        ditto -c -k --keepParent "$target" "$submission" || return $?
    fi
    verdict=$(xcrun notarytool submit "$submission" --key "$APPLE_API_KEY_P8" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait 2>&1)
    verdict_status=$?
    echo "$verdict"
    if [ $verdict_status != 0 ]; then
        return $verdict_status
    fi
    if ! echo "$verdict" | grep -q 'status: Accepted'; then
        builder_message "error: notarization of $target was not accepted"
        return 1
    fi
    if [ "$submission" != "$target" ]; then
        rm -f "$submission"
    fi
    xcrun stapler staple "$target"
}
