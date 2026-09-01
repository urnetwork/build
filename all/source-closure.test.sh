#!/usr/bin/env bash
# Deterministic proof that private acceptance output never becomes build input.
set -euo pipefail
umask 077

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=all/stage-local-repos.sh
source "$here/stage-local-repos.sh"
# shellcheck source=all/windows/lib.sh
source "$here/windows/lib.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-source-closure.test.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT

local_home="$run_dir/local"
build_home="$run_dir/build"
mkdir -p \
  "$local_home/connect/tests/__acceptance__/run-one" \
  "$local_home/connect/build" \
  "$build_home/connect/tests/__acceptance__/old-run" \
  "$build_home/connect/build"
printf 'required source\n' >"$local_home/connect/required.go"
printf 'private artifact\n' >"$local_home/connect/tests/__acceptance__/run-one/result.json"
printf 'compiled test\n' >"$local_home/connect/connect.test"
printf 'finder metadata\n' >"$local_home/connect/.DS_Store"
printf 'preserved output\n' >"$build_home/connect/build/output.bin"
printf 'stale private artifact\n' >"$build_home/connect/tests/__acceptance__/old-run/result.json"
printf 'stale compiled test\n' >"$build_home/connect/connect.test"

BUILD_HOME="$build_home"
SRC_CONNECT="$local_home/connect"
EXTERNAL_WARP_VERSION=test
stage_local_repos connect >/dev/null

[ -f "$build_home/connect/required.go" ] || fail "local staging omitted required source"
[ -f "$build_home/connect/build/output.bin" ] || fail "local staging deleted preserved build output"
[ ! -e "$build_home/connect/tests/__acceptance__" ] || fail "local staging retained acceptance artifacts"
[ ! -e "$build_home/connect/connect.test" ] || fail "local staging retained a compiled Go test"
[ ! -e "$build_home/connect/.DS_Store" ] || fail "local staging retained Finder metadata"

vm_source="$run_dir/vm-source"
vm_dest="$run_dir/vm-dest"
for repo in windows sdk connect glog goidenticons; do
  mkdir -p "$vm_source/$repo/tests/__acceptance__/run-one"
  printf 'required source\n' >"$vm_source/$repo/required.txt"
  printf 'private artifact\n' >"$vm_source/$repo/tests/__acceptance__/run-one/result.json"
  printf 'compiled test\n' >"$vm_source/$repo/$repo.test"
  printf 'finder metadata\n' >"$vm_source/$repo/.DS_Store"
done
mkdir -p "$vm_dest/windows/tests/__acceptance__/old-run"
printf 'stale private artifact\n' >"$vm_dest/windows/tests/__acceptance__/old-run/result.json"
printf 'stale compiled test\n' >"$vm_dest/windows/windows.test"

win_source_rsync_excludes
rsync -a --delete --delete-excluded \
  "${WIN_SOURCE_RSYNC_EXCLUDES[@]}" \
  "$vm_source/windows" "$vm_source/sdk" "$vm_source/connect" \
  "$vm_source/glog" "$vm_source/goidenticons" "$vm_dest/"

for repo in windows sdk connect glog goidenticons; do
  [ -f "$vm_dest/$repo/required.txt" ] || fail "VM sync omitted $repo source"
  [ ! -e "$vm_dest/$repo/tests/__acceptance__" ] || fail "VM sync retained $repo acceptance artifacts"
  [ ! -e "$vm_dest/$repo/$repo.test" ] || fail "VM sync retained $repo compiled test"
  [ ! -e "$vm_dest/$repo/.DS_Store" ] || fail "VM sync retained $repo Finder metadata"
done

echo "build source-closure regression: PASS"
