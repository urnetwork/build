#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0

# Isolated command seams keep certificate matching deterministic in tests.
apple_security() {
  command security "$@"
}

apple_openssl() {
  command openssl "$@"
}

apple_certificate_subject_field() {
  local subject="$1"
  local field="$2"

  printf '%s\n' "${subject#subject=}" |
    tr ',' '\n' |
    sed -n "s/^${field}=//p" |
    sed -n '1p'
}

# Matches a usable development identity by the certificate's team OU.
apple_has_development_identity_for_team() (
  local team="$1"
  local temp_root="${TMPDIR:-/tmp}"
  local temp_dir
  local certificate
  local fingerprint
  local subject
  local common_name
  local certificate_team

  temp_dir="$(mktemp -d "${temp_root%/}/urnetwork-apple-signing.XXXXXX")"
  trap 'rm -R -- "$temp_dir"' EXIT

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
    [ "$certificate_team" = "$team" ] && exit 0
  done

  exit 1
)
