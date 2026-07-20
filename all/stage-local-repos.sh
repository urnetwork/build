#!/usr/bin/env bash
# Stage local working-tree repos into BUILD_HOME so the desktop builds compile
# LOCAL (possibly uncommitted) changes instead of the release-staged copies that
# live under BUILD_HOME. Sourced by build-linux.sh / build-windows.sh; a no-op
# unless the caller points it at local sources, so the normal release path (which
# stages BUILD_HOME itself, e.g. via run.sh) is unaffected.
#
#   stage_local_repos <repo> [<repo> ...]
#
# For each repo, a local source path is resolved from the first that is set:
#   SRC_<REPO>        per-repo override, e.g. SRC_SDK=/path/to/sdk
#   $SRC_HOME/<repo>  if SRC_HOME (a local monorepo root) is set
# If a source resolves, it is rsync'd into $BUILD_HOME/<repo> (source tree only:
# .git and heavy/generated dirs are skipped, so the copy is small and the dest's
# own build outputs and .git are preserved). Repos with no source configured are
# left as-is.
#
# Because a locally-staged repo generally is NOT on a v<version> branch, pass
# EXTERNAL_WARP_VERSION explicitly when staging local sources (the version
# auto-detection reads a v<version> branch off BUILD_HOME/<repo>).
#
# SPDX-License-Identifier: MPL-2.0

# Resolve the configured local source for a repo, or empty if none.
_stage_src_for() {
  local repo="$1" upper src
  upper="$(printf '%s' "$repo" | tr '[:lower:]-' '[:upper:]_')"
  eval "src=\"\${SRC_${upper}:-}\""
  if [ -z "$src" ] && [ -n "${SRC_HOME:-}" ]; then
    src="$SRC_HOME/$repo"
  fi
  printf '%s' "$src"
}

stage_local_repos() {
  : "${BUILD_HOME:?stage_local_repos: set BUILD_HOME}"
  local repo src dest f any=0
  for repo in "$@"; do
    src="$(_stage_src_for "$repo")"
    [ -z "$src" ] && continue
    if [ ! -d "$src" ]; then
      echo "ERROR: stage_local_repos: local source for '$repo' not found: $src" >&2
      return 1
    fi
    dest="$BUILD_HOME/$repo"
    # a real (non-symlink) same-path is a no-op; guard against rsync'ing onto self
    if [ "$(cd "$src" 2>/dev/null && pwd -P)" = "$(cd "$dest" 2>/dev/null && pwd -P)" ]; then
      echo ">>> local $repo already IS the build copy ($dest) — not staging"
      any=1
      continue
    fi
    echo ">>> staging local $repo: $src -> $dest"
    mkdir -p "$dest"
    # source tree only. The excludes apply to BOTH the copy (don't drag in the
    # local .git / build artifacts) and the --delete (preserve the dest's build
    # outputs and .git so version auto-detection still works).
    rsync -a --delete \
      --exclude='.git/' \
      --exclude='build/' \
      --exclude='out/' \
      --exclude='node_modules/' \
      --exclude='third_party/urnetwork-sdk/' \
      --exclude='.DS_Store' \
      --exclude='*.test' \
      --exclude='*.o' \
      --exclude='*.a' \
      "$src/" "$dest/"
    # The blanket 'build/' exclude above (any depth — it must skip gradle/app
    # build dirs) also skips SOURCE files that live in sdk/build, the gomobile
    # build module (Makefile, go.mod, go.sum, main.go). A release run rewrites
    # that go.mod to pin the PUBLISHED /vYYYY modules, so a stale preserved
    # copy makes a staged build silently compile the published sdk instead of
    # the staged local trees. Re-sync those source files explicitly; the
    # output dirs (android/, apple/, ios/) stay preserved.
    if [ "$repo" = sdk ]; then
      mkdir -p "$dest/build"
      for f in Makefile makefile go.mod go.sum main.go; do
        if [ -f "$src/build/$f" ]; then
          cp -p "$src/build/$f" "$dest/build/$f"
        fi
      done
    fi
    any=1
  done
  if [ "$any" = 1 ] && [ -z "${EXTERNAL_WARP_VERSION:-}" ]; then
    echo ">>> note: local repos staged but EXTERNAL_WARP_VERSION unset — version" \
         "auto-detection reads BUILD_HOME/<repo>'s v<version> branch, which a" \
         "local checkout usually lacks. Pass EXTERNAL_WARP_VERSION for a clean build." >&2
  fi
  return 0
}
