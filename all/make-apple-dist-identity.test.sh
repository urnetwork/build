#!/usr/bin/env bash
# Proves the identity script's `developer-id` mode assembles the p12 run.sh
# imports for the macOS direct-download DMG: ~/.identity-devid.p12, friendly
# name "Developer ID Application", from the box's own private key. Runs the
# real script against a throwaway HOME with a self-signed stand-in for the
# portal certificate; the WWDR chain download is stubbed out (no network).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/make-apple-dist-identity.sh"
openssl=/usr/bin/openssl

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export HOME="$work/home"
mkdir -p "$HOME/Library/Keychains" "$work/bin"
printf '#!/bin/sh\nexit 22\n' > "$work/bin/curl"
chmod 700 "$work/bin/curl"
export PATH="$work/bin:$PATH"

# usage (exit 1) names the mode and the output
usage="$("$script" 2>&1 || true)"
echo "$usage" | grep -q 'developer-id <developer-id-application.cer>' || fail "usage does not document the developer-id mode"
echo "$usage" | grep -q '~/.identity-devid.p12' || fail "usage does not name ~/.identity-devid.p12"

# prerequisites are checked in order: the certificate argument, the passphrase file, the key
if "$script" developer-id >/dev/null 2>&1; then fail "developer-id without a certificate succeeded"; fi
printf 'synthetic-passphrase\n' > "$HOME/.p12-pw"
missing_key="$("$script" developer-id "$work/none.cer" 2>&1 || true)"
echo "$missing_key" | grep -q 'identity-dist.key is missing' || fail "developer-id did not require the csr key"

# a certificate issued for this box's key
(umask 077 && $openssl genrsa -out "$HOME/.identity-dist.key" 2048 >/dev/null 2>&1)
$openssl req -x509 -new -key "$HOME/.identity-dist.key" -days 1 -outform der -out "$work/devid.cer" \
  -subj "/UID=6BGU69Q742/CN=Developer ID Application: Synthetic (6BGU69Q742)/OU=6BGU69Q742/O=Synthetic" >/dev/null 2>&1

# a certificate from another key is refused
$openssl genrsa -out "$work/other.key" 2048 >/dev/null 2>&1
$openssl req -x509 -new -key "$work/other.key" -days 1 -outform der -out "$work/other.cer" \
  -subj "/CN=Developer ID Application: Other (6BGU69Q742)" >/dev/null 2>&1
mismatch="$("$script" developer-id "$work/other.cer" 2>&1 || true)"
echo "$mismatch" | grep -q 'modulus mismatch' || fail "a certificate for another key was accepted"
[ ! -e "$HOME/.identity-devid.p12" ] || fail "a refused certificate still wrote the p12"

output="$("$script" developer-id "$work/devid.cer" 2>&1)" || fail "developer-id failed: $output"
p12="$HOME/.identity-devid.p12"
[ -s "$p12" ] || fail "~/.identity-devid.p12 was not written"
[ "$(/usr/bin/stat -f '%Lp' "$p12")" = 600 ] || fail "p12 is not 0600"
$openssl pkcs12 -in "$p12" -passin "file:$HOME/.p12-pw" -nokeys -info 2>&1 | grep -q 'friendlyName: Developer ID Application' ||
  fail "p12 friendly name is not 'Developer ID Application'"
$openssl pkcs12 -in "$p12" -passin "file:$HOME/.p12-pw" -nocerts -nodes 2>/dev/null | grep -q 'PRIVATE KEY' ||
  fail "p12 does not carry the private key"
echo "$output" | grep -q 'Developer ID Application: Synthetic' || fail "verification keychain did not list the identity: $output"
echo "$output" | grep -q 'build/all/run.sh imports every ~/.identity\*.p12' || fail "the run.sh import note is missing"
[ ! -e "$HOME/.identity-dist.p12" ] || fail "developer-id mode wrote the distribution p12"

echo "make-apple-dist-identity: OK"
