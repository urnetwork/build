#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0

# Isolated command seams keep certificate matching deterministic in tests.
apple_security() {
  command security "$@"
}

apple_openssl() {
  command openssl "$@"
}

apple_timeout() {
  command timeout "$@"
}

apple_certificate_subject_field() {
  local subject="$1"
  local field="$2"

  printf '%s\n' "${subject#subject=}" |
    tr ',' '\n' |
    sed -n "s/^${field}=//p" |
    sed -n '1p'
}

# Returns a usable development identity fingerprint matched by certificate
# team OU. The developer ID embedded in the common name is not the team ID.
apple_development_identity_for_team() (
  local team="$1"
  local temp_root="${TMPDIR:-/tmp}"
  local temp_dir
  local certificate
  local fingerprint
  local subject
  local common_name
  local certificate_team
  local quoted_temp_dir

  temp_dir="$(mktemp -d "${temp_root%/}/urnetwork-apple-signing.XXXXXX")"
  printf -v quoted_temp_dir '%q' "$temp_dir"
  trap "rm -R -- $quoted_temp_dir" EXIT

  apple_security find-identity -v -p codesigning |
    awk '
      length($2) == 40 && $2 ~ /^[[:xdigit:]]+$/ &&
      $0 ~ /"(Apple Development|iPhone Developer): / {
        print toupper($2)
      }
    ' >"$temp_dir/identity-hashes"

  apple_security find-certificate -a -p |
    awk -v directory="$temp_dir" '
      $0 == "-----BEGIN CERTIFICATE-----" {
        certificate += 1
        path = sprintf("%s/certificate-%d.pem", directory, certificate)
      }
      path != "" { print >path }
      $0 == "-----END CERTIFICATE-----" {
        close(path)
        path = ""
      }
    '

  for certificate in "$temp_dir"/certificate-*.pem; do
    [ -f "$certificate" ] || continue
    fingerprint="$(
      apple_openssl x509 -in "$certificate" -noout -fingerprint -sha1 2>/dev/null |
        sed -n 's/.*=//p' |
        tr -d ':[:space:]' |
        tr '[:lower:]' '[:upper:]'
    )"
    [ -n "$fingerprint" ] || continue
    grep -Fqx "$fingerprint" "$temp_dir/identity-hashes" || continue

    subject="$(apple_openssl x509 -in "$certificate" -noout -subject -nameopt RFC2253 2>/dev/null)" || continue
    common_name="$(apple_certificate_subject_field "$subject" CN)"
    certificate_team="$(apple_certificate_subject_field "$subject" OU)"
    case "$common_name" in
      'Apple Development:'*|'iPhone Developer:'*) ;;
      *) continue ;;
    esac
    if [ "$certificate_team" = "$team" ]; then
      printf '%s\n' "$fingerprint"
      exit 0
    fi
  done

  exit 1
)

apple_has_development_identity_for_team() {
  apple_development_identity_for_team "$1" >/dev/null
}

# Proves that the private key is authorized for non-interactive codesign use.
# Merely listing an identity does not exercise its keychain ACL and can leave a
# later Xcode build waiting indefinitely behind a hidden SecurityAgent prompt.
apple_verify_signing_identity_access() (
  local identity="$1"
  local temp_root="${TMPDIR:-/tmp}"
  local temp_dir
  local quoted_temp_dir

  temp_dir="$(mktemp -d "${temp_root%/}/urnetwork-apple-signing-probe.XXXXXX")"
  printf -v quoted_temp_dir '%q' "$temp_dir"
  trap "rm -R -- $quoted_temp_dir" EXIT
  cp /usr/bin/true "$temp_dir/signing-probe"
  if apple_timeout 15 codesign --force --sign "$identity" --timestamp=none \
    "$temp_dir/signing-probe" >/dev/null 2>&1; then
    exit 0
  else
    exit $?
  fi
)
