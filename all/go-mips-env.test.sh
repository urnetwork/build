#!/usr/bin/env bash
# Proves every MIPS release target is built soft float, including mips64.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=all/go-mips-env.sh
source "$here/go-mips-env.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ "$(go_mips_softfloat_env mips)" = "GOMIPS=softfloat" ] || fail "mips env"
[ "$(go_mips_softfloat_env mipsle)" = "GOMIPS=softfloat" ] || fail "mipsle env"
[ "$(go_mips_softfloat_env mips64)" = "GOMIPS64=softfloat" ] || fail "mips64 env"
[ "$(go_mips_softfloat_env mips64le)" = "GOMIPS64=softfloat" ] || fail "mips64le env"
[ -z "$(go_mips_softfloat_env arm)" ] || fail "arm env is not empty"
[ -z "$(go_mips_softfloat_env amd64)" ] || fail "amd64 env is not empty"

# The float mode the toolchain actually recorded in the binary.
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-go-mips-env.test.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT
printf 'module mipsenvtest\n\ngo 1.21\n' >"$run_dir/go.mod"
printf 'package main\n\nfunc main() {}\n' >"$run_dir/main.go"
for arch in mips mipsle mips64 mips64le; do
  (cd "$run_dir" &&
    env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" $(go_mips_softfloat_env "$arch") \
      go build -o "$run_dir/$arch" .)
  go version -m "$run_dir/$arch" | grep -Eq 'GOMIPS(64)?=softfloat' ||
    fail "$arch binary is not softfloat: $(go version -m "$run_dir/$arch" | grep GOMIPS)"
done

echo "go-mips-env: OK"
