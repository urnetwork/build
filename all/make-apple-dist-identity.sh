#!/bin/zsh
# make-apple-dist-identity.sh — create the builder's Apple Distribution signing
# identity without a GUI. The private key is generated ON this box and never
# leaves it; Apple's developer portal turns the CSR into certificates; this
# script then assembles keychain-ready .p12 files that build/all/run.sh imports
# into the per-run build keychain (it imports every ~/.identity*.p12, all with
# the ~/.p12-pw passphrase — see REMOTEBUILD.md).
#
# usage:
#   ./make-apple-dist-identity.sh csr
#       Generates ~/.identity-dist.key (0600, reused if it already exists) and
#       ~/identity-dist.csr, and prints the portal steps.
#
#   ./make-apple-dist-identity.sh assemble <distribution.cer> [<installer.cer>]
#       distribution.cer = the "Apple Distribution" cert downloaded from the
#                          portal (covers the iOS + macOS App Store export)
#       installer.cer    = optional "Mac Installer Distribution" cert (signs
#                          the macOS App Store .pkg when cloud signing is
#                          unavailable). The same CSR works for both.
#       Writes ~/.identity-dist.p12 (and ~/.identity-installer.p12), then
#       verifies each by importing into a throwaway keychain.
#
# Alternative: if a Mac with the identity already in Xcode exists, skip this
# script — export the identity there (Xcode > Settings > Accounts > Manage
# Certificates > right-click > Export) with the passphrase from ~/.p12-pw and
# scp it here as ~/.identity-dist.p12.
#
# /usr/bin/openssl (LibreSSL) is used on purpose: p12 files it produces use
# encryption `security import` accepts; homebrew's openssl@3 defaults do not.

set -u
OPENSSL=/usr/bin/openssl
KEY="$HOME/.identity-dist.key"
CSR="$HOME/identity-dist.csr"
PW_FILE="$HOME/.p12-pw"

die () {
    echo "error: $1" >&2
    exit 1
}

require_pw_file () {
    [ -f "$PW_FILE" ] || die "$PW_FILE is missing; it must hold the shared p12 passphrase (see REMOTEBUILD.md)"
}

make_csr () {
    if [ -f "$KEY" ]; then
        echo "reusing existing private key $KEY"
    else
        (umask 077 && $OPENSSL genrsa -out "$KEY" 2048) || die "key generation failed"
        echo "generated $KEY (0600; never leaves this box)"
    fi
    $OPENSSL req -new -key "$KEY" -out "$CSR" \
        -subj "/CN=URnetwork Builder/O=URnetwork" || die "csr generation failed"
    echo "wrote $CSR"
    echo ""
    echo "next, in the Apple developer portal (an Admin/Account Holder):"
    echo "  1. https://developer.apple.com/account/resources/certificates/add"
    echo "  2. choose 'Apple Distribution', upload identity-dist.csr, download the .cer"
    echo "  3. (recommended) repeat with 'Mac Installer Distribution' using the SAME csr"
    echo "     (needed for the headless macOS App Store .pkg when not cloud signing)"
    echo "     If the portal refuses (certificate limit reached), revoke an unused"
    echo "     certificate of that type first."
    echo "  4. copy them back here, then:"
    echo "       ./make-apple-dist-identity.sh assemble distribution.cer mac_installer.cer"
}

# cer_to_pem <in.cer> <out.pem> — portal certs are DER; accept PEM too
cer_to_pem () {
    $OPENSSL x509 -inform der -in "$1" -out "$2" 2>/dev/null ||
        $OPENSSL x509 -inform pem -in "$1" -out "$2" 2>/dev/null ||
        die "$1 is not a certificate (expected the portal .cer)"
}

# fetch the Apple WWDR intermediates so the p12 carries its full chain; the
# import then validates even on a box whose keychains lack the newer G-series
fetch_wwdr_chain () {
    local chain="$1" g
    : > "$chain"
    for g in G3 G4 G5 G6; do
        if curl -fsS --max-time 30 -o "$WORK/wwdr$g.cer" "https://www.apple.com/certificateauthority/AppleWWDRCA$g.cer" 2>/dev/null; then
            $OPENSSL x509 -inform der -in "$WORK/wwdr$g.cer" 2>/dev/null >> "$chain"
        fi
    done
    [ -s "$chain" ] || echo "warning: could not fetch WWDR intermediates; continuing without a bundled chain" >&2
}

# assemble_p12 <cert.cer> <friendly-name> <out.p12>
assemble_p12 () {
    local cer="$1" name="$2" out="$3"
    local pem="$WORK/$name.pem"
    cer_to_pem "$cer" "$pem"

    # the cert must belong to our private key
    local key_mod cert_mod
    key_mod=`$OPENSSL rsa -noout -modulus -in "$KEY"`
    cert_mod=`$OPENSSL x509 -noout -modulus -in "$pem"`
    [ "$key_mod" = "$cert_mod" ] || die "$cer was not issued for $KEY (modulus mismatch) — was the csr from this box used?"

    echo "subject: `$OPENSSL x509 -noout -subject -in "$pem" | sed 's/^subject= *//'`"

    local certfile_args=()
    [ -s "$WORK/wwdr-chain.pem" ] && certfile_args=(-certfile "$WORK/wwdr-chain.pem")
    (umask 077 && $OPENSSL pkcs12 -export \
        -inkey "$KEY" -in "$pem" "${certfile_args[@]}" \
        -name "$name" \
        -passout "file:$PW_FILE" \
        -out "$out") || die "p12 export failed for $name"
    echo "wrote $out"
}

verify_p12s () {
    local kc="$HOME/Library/Keychains/identity-verify.keychain-db"
    local kc_pw=`$OPENSSL rand -base64 24`
    security delete-keychain "$kc" 2>/dev/null
    security create-keychain -p "$kc_pw" "$kc" || die "verify keychain create failed"
    {
        local p12
        for p12 in "$@"; do
            security import "$p12" -P "$(cat "$PW_FILE")" -f pkcs12 -k "$kc" ||
                die "verification import failed for $p12"
        done
        echo ""
        echo "identities now in a fresh keychain from these p12 files:"
        security find-identity -v "$kc"
    } always {
        security delete-keychain "$kc" 2>/dev/null
    }
}

case "${1:-}" in
csr)
    make_csr
    ;;
assemble)
    [ $# -ge 2 ] || die "usage: $0 assemble <distribution.cer> [<installer.cer>]"
    require_pw_file
    [ -f "$KEY" ] || die "$KEY is missing; run '$0 csr' first"
    WORK=`mktemp -d` || die "mktemp failed"
    trap 'rm -rf "$WORK"' EXIT

    fetch_wwdr_chain "$WORK/wwdr-chain.pem"

    assemble_p12 "$2" "Apple Distribution" "$HOME/.identity-dist.p12"
    outputs=("$HOME/.identity-dist.p12")
    if [ $# -ge 3 ]; then
        assemble_p12 "$3" "Mac Installer Distribution" "$HOME/.identity-installer.p12"
        outputs+=("$HOME/.identity-installer.p12")
    fi
    verify_p12s "${outputs[@]}"
    echo ""
    echo "done. build/all/run.sh imports every ~/.identity*.p12 into the per-run"
    echo "build keychain automatically; the next build's 'find-identity' log line"
    echo "should list the identities above. The portal .cer files and ~/identity-dist.csr"
    echo "are safe to delete; keep ~/.identity-dist.key (0600)."
    ;;
*)
    sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
