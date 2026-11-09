#!/usr/bin/env bash
# Build ur's release artifacts (DESIGN.md §4.10; REPO-M1-01 to REPO-M1-03): the 14 files of
# all/ur/artifacts.txt, for darwin, linux and windows on amd64 and arm64.
#
#   1. ur for the six targets, by sidecar/cli's `make build`: CGO_ENABLED=0 -trimpath,
#      GOEXPERIMENT=greenteagc, -ldflags "-w -s" (plan-REPO-small-readings #13).
#   2. Each binary must keep cli's gVisor fork and connect's sctp fork, read by go version -m: the
#      release fixture strips every replace and cannot see this (reading 3).
#   3. sidecar's TestHygieneReleaseBinary over all six, the row that refuses a release leg built with
#      sidecar_testhooks.
#   4. This host's ur runs --version, and on a release tree prints the release's version.
#   5. The Linux .tar.gz of each arch: ur, git-remote-ur as a link to it, LICENSE and NOTICE.
#   6. build-ur-macos.sh: the notarized ur.app's .tar.gz and .pkg (REPO-M1-02).
#   7. build-ur-linux.sh: the .deb, .rpm and .pkg.tar.zst (REPO-M1-03).
#   8. build-ur-windows.sh: the signed .zip (REPO-M1-03).
#   9. OUT_DIR then holds exactly the table's 14 names.
#
# git-remote-ur is a link to ur in every tar and package and a copy in the Windows zip, since git finds
# it on PATH and ur dispatches on argv[0]. Archives come from all/urarchive, sorted, owned by 0:0 and
# stamped with SOURCE_DATE_EPOCH, the sidecar commit's time. Reproducible builds are a goal, not a gate
# (R-REPO-7). No self-updater in v0.
#
# This is run.sh's ur step, as a script so it also runs standalone against the local branches as they
# stand, as build-linux.sh does: nothing here pulls, checks out or edits a source.
#
# Inputs (env):
#   BUILD_HOME             build home (default: this script's parent dir)
#   EXTERNAL_WARP_VERSION  release version, e.g. 2026.11.6-1234567 (default: from the v<version>
#                          branch of $BUILD_HOME/sidecar)
#   WARP_VERSION           internal version (default: EXTERNAL_WARP_VERSION with its last '-' a '+')
#   OUT_DIR                where the artifacts land; this script's earlier ones in it are removed
#                          first, so a caller never picks up a stale one
#                          (default: ${BUILD_OUT:-$BUILD_HOME/out}/ur)
#   GO, MAKE               the go and make to run (default: go, make)
#   URARCHIVE              a built all/urarchive (default: built here)
#   The platform scripts' own inputs: see build-ur-macos.sh and build-ur-windows.sh.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export BUILD_HOME="${BUILD_HOME:-$(dirname "$here")}"
# shellcheck source=ur/lib.sh
source "$here/ur/lib.sh"
ur_versions
OUT_DIR="${OUT_DIR:-${BUILD_OUT:-$BUILD_HOME/out}/ur}"
export OUT_DIR
GO="${GO:-go}"
MAKE="${MAKE:-make}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$OUT_DIR"
names=()
while IFS= read -r name; do
  names+=("$name")
done < <(ur_artifact_names "$EXTERNAL_WARP_VERSION")
[ "${#names[@]}" -eq 14 ] || ur_die "all/ur/artifacts.txt names ${#names[@]} artifacts, and §4.10's six targets make 14"
for name in "${names[@]}"; do
  rm -f "$OUT_DIR/$name"
done

ur_source_date_epoch
ur_urarchive "$work"
sidecar_module="$(ur_sidecar_module)"
for document in LICENSE NOTICE; do
  [ -s "$sidecar_module/$document" ] || ur_die "$sidecar_module/$document is missing"
done

echo ">>> ur $EXTERNAL_WARP_VERSION for six targets, SOURCE_DATE_EPOCH $SOURCE_DATE_EPOCH"
"$MAKE" -C "$BUILD_HOME/sidecar/cli" build OUT="$work/bin" GO="$GO"

binaries=()
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  binary="$work/bin/$target/ur"
  case "$target" in windows/*) binary="$binary.exe" ;; esac
  [ -s "$binary" ] || ur_die "make build wrote no ur for $target at $binary"
  binaries+=("$binary")
done

# go version -m prints each replaced module's => line after its dep line. cli's gVisor is the urnetwork
# fork, a version on a release tree or the sibling checkout in development; sctp is connect's
# directory on both.
for binary in "${binaries[@]}"; do
  info="$("$GO" version -m "$binary")" || ur_die "go version -m $binary failed"
  printf '%s\n' "$info" | awk '
    $1 == "dep" && $2 == "gvisor.dev/gvisor" { want = 1; next }
    want && $1 == "=>" && ($2 == "github.com/urnetwork/gvisor" || $2 ~ /^(\.|\/).*\/gvisor$/) { found = 1 }
    { want = 0 }
    END { exit !found }' || ur_die "$binary does not link cli's gVisor fork (go version -m shows no => to it)"
  printf '%s\n' "$info" | awk '
    $1 == "dep" && $2 == "github.com/pion/sctp" { want = 1; next }
    want && $1 == "=>" && $2 ~ /^(\.|\/).*\/connect\/sctp$/ { found = 1 }
    { want = 0 }
    END { exit !found }' || ur_die "$binary does not link connect's sctp fork (go version -m shows no => to connect/sctp)"
done

echo ">>> TestHygieneReleaseBinary over the six"
if ! (cd "$sidecar_module/check" &&
  RELEASE_BINARIES="${binaries[*]}" GOFLAGS= GOWORK=off GOTOOLCHAIN=local \
    "$GO" test -count=1 -v -tags sidecar_check -run '^TestHygieneReleaseBinary$' .) > "$work/hygiene.log" 2>&1; then
  cat "$work/hygiene.log" >&2
  ur_die "TestHygieneReleaseBinary refused a release binary"
fi
grep -q -- '--- PASS: TestHygieneReleaseBinary' "$work/hygiene.log" ||
  { cat "$work/hygiene.log" >&2; ur_die "TestHygieneReleaseBinary did not run"; }

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) host_target=darwin/arm64 ;;
  Darwin/x86_64) host_target=darwin/amd64 ;;
  Linux/x86_64) host_target=linux/amd64 ;;
  Linux/aarch64 | Linux/arm64) host_target=linux/arm64 ;;
  *) ur_die "this host, $(uname -s)/$(uname -m), runs none of the six targets" ;;
esac
version_line="$("$work/bin/$host_target/ur" --version)" || ur_die "ur --version failed on $host_target"
case "$version_line" in
  "ur "*) ;;
  *) ur_die "ur --version printed $version_line" ;;
esac
if [ "$sidecar_module" != "$BUILD_HOME/sidecar" ] && [ "$version_line" != "ur $WARP_VERSION" ]; then
  ur_die "the release's ur prints $version_line, not ur $WARP_VERSION: the Version sed did not reach it"
fi
echo ">>> $host_target: $version_line"

for arch in amd64 arm64; do
  "$URARCHIVE" tar -o "$OUT_DIR/ur-$EXTERNAL_WARP_VERSION-linux-$arch.tar.gz" -mtime "$SOURCE_DATE_EPOCH" \
    "ur=$work/bin/linux/$arch/ur" "git-remote-ur@ur" \
    "LICENSE=$sidecar_module/LICENSE" "NOTICE=$sidecar_module/NOTICE"
done

export UR_BIN_DIR="$work/bin"
"$here/build-ur-macos.sh"
"$here/build-ur-linux.sh"
"$here/build-ur-windows.sh"

for name in "${names[@]}"; do
  [ -s "$OUT_DIR/$name" ] || ur_die "$OUT_DIR/$name is missing or empty"
done
for written in "$OUT_DIR"/*; do
  [ -e "$written" ] || continue
  found=0
  for name in "${names[@]}"; do
    [ "$(basename "$written")" = "$name" ] && found=1
  done
  [ "$found" = 1 ] || ur_die "$written is not one of all/ur/artifacts.txt's names"
done
echo ">>> ur's ${#names[@]} artifacts in $OUT_DIR"
