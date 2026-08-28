#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/signing.sh"

identity_hash="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
certificate_without_key_hash="BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
identity_team="6BGU69Q742"
certificate_without_key_team="6BGU69Q742"

apple_security() {
  case "$*" in
    'find-identity -v -p codesigning')
      printf '  1) %s "Apple Development: Brien Colwell (35JW6PYM3B)"\n' "$identity_hash"
      ;;
    'find-certificate -a -p')
      cat <<'EOF'
-----BEGIN CERTIFICATE-----
identity
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
certificate-without-key
-----END CERTIFICATE-----
EOF
      ;;
    *)
      return 2
      ;;
  esac
}

apple_openssl() {
  local certificate=""
  local mode=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -in)
        certificate="$2"
        shift 2
        ;;
      -fingerprint)
        mode=fingerprint
        shift
        ;;
      -subject)
        mode=subject
        shift
        ;;
      *) shift ;;
    esac
  done

  case "$(sed -n '2p' "$certificate"):$mode" in
    identity:fingerprint)
      printf 'sha1 Fingerprint=%s\n' "$identity_hash"
      ;;
    identity:subject)
      printf 'subject=C=US,O=BringYour\\, Inc.,OU=%s,CN=Apple Development: Brien Colwell (35JW6PYM3B),UID=AP2L74LLC6\n' "$identity_team"
      ;;
    certificate-without-key:fingerprint)
      printf 'sha1 Fingerprint=%s\n' "$certificate_without_key_hash"
      ;;
    certificate-without-key:subject)
      printf 'subject=C=US,O=BringYour\\, Inc.,OU=%s,CN=Apple Development: Brien Colwell (35JW6PYM3B),UID=AP2L74LLC6\n' "$certificate_without_key_team"
      ;;
    *)
      return 2
      ;;
  esac
}

if ! apple_has_development_identity_for_team 6BGU69Q742; then
  echo "team OU was not recognized when the certificate name ended in a different developer ID" >&2
  exit 1
fi

if apple_has_development_identity_for_team 35JW6PYM3B; then
  echo "developer ID in the certificate name was mistaken for the signing team" >&2
  exit 1
fi

identity_team="DWD39RZH9Z"
if apple_has_development_identity_for_team 6BGU69Q742; then
  echo "a certificate without its private key was accepted as a signing identity" >&2
  exit 1
fi

echo "build/all/apple signing identity tests passed"
