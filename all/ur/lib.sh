# Shared by build/all's ur scripts (build-ur.sh, build-ur-linux.sh, build-ur-windows.sh; DESIGN.md §4.10).
# Sourced by bash; build-ur-macos.sh, which is zsh, reads the artifact table itself.
#
# SPDX-License-Identifier: MPL-2.0

ur_die() {
  echo "ERROR: $*" >&2
  exit 1
}

# The versions, as build-linux.sh derives them: run.sh exports EXTERNAL_WARP_VERSION and WARP_VERSION,
# and a standalone run reads the sidecar repo's v<version> branch. VERSION_BASE and VERSION_RELEASE
# split EXTERNAL_WARP_VERSION at its last hyphen, as nfpm needs, and GO_MOD_VERSION is its year, the
# release tree's v<year> (§4.9).
ur_versions() {
  if [ -z "${EXTERNAL_WARP_VERSION:-}" ]; then
    local branch
    branch="$(git -C "$BUILD_HOME/sidecar" branch --show-current)"
    case "$branch" in
      v?*) EXTERNAL_WARP_VERSION="${branch#v}" ;;
      *) ur_die "set EXTERNAL_WARP_VERSION or put $BUILD_HOME/sidecar on its v<version> branch (currently: ${branch:-detached})" ;;
    esac
  fi
  case "$EXTERNAL_WARP_VERSION" in
    *[!0-9A-Za-z.+-]* | "" | -* | *-) ur_die "EXTERNAL_WARP_VERSION $EXTERNAL_WARP_VERSION is not a release version" ;;
  esac
  if [ -z "${WARP_VERSION:-}" ]; then
    case "$EXTERNAL_WARP_VERSION" in
      *-*) WARP_VERSION="${EXTERNAL_WARP_VERSION%-*}+${EXTERNAL_WARP_VERSION##*-}" ;;
      *) WARP_VERSION="$EXTERNAL_WARP_VERSION" ;;
    esac
  fi
  case "$EXTERNAL_WARP_VERSION" in
    *-*)
      VERSION_BASE="${EXTERNAL_WARP_VERSION%-*}"
      VERSION_RELEASE="${EXTERNAL_WARP_VERSION##*-}"
      ;;
    *)
      VERSION_BASE="$EXTERNAL_WARP_VERSION"
      VERSION_RELEASE="0"
      ;;
  esac
  GO_MOD_VERSION="${EXTERNAL_WARP_VERSION%%.*}"
  export EXTERNAL_WARP_VERSION WARP_VERSION VERSION_BASE VERSION_RELEASE GO_MOD_VERSION
}

# The sidecar repository's root module: v<year>/ on a release tree, where LICENSE and NOTICE moved
# with it (plan-REPO-small-readings #5), else the checkout's top.
ur_sidecar_module() {
  if [ -f "$BUILD_HOME/sidecar/v$GO_MOD_VERSION/go.mod" ]; then
    echo "$BUILD_HOME/sidecar/v$GO_MOD_VERSION"
  else
    echo "$BUILD_HOME/sidecar"
  fi
}

# Every name of all/ur/artifacts.txt for a version, one a line.
ur_artifact_names() {
  local table="$BUILD_HOME/all/ur/artifacts.txt" template
  [ -r "$table" ] || ur_die "the artifact table $table is not readable"
  while IFS= read -r template; do
    case "$template" in
      "" | "#"*) continue ;;
    esac
    echo "${template//\{version\}/$1}"
  done < "$table"
}

# SOURCE_DATE_EPOCH is the sidecar commit's time, never the caller's (REPO-M1-01).
ur_source_date_epoch() {
  SOURCE_DATE_EPOCH="$(git -C "$BUILD_HOME/sidecar" log -1 --format=%ct HEAD)" ||
    ur_die "cannot read the sidecar commit's time"
  case "$SOURCE_DATE_EPOCH" in
    "" | *[!0-9]*) ur_die "the sidecar commit's time is $SOURCE_DATE_EPOCH" ;;
  esac
  export SOURCE_DATE_EPOCH
}

# A built urarchive: URARCHIVE when the caller built one, else one built here into $1.
ur_urarchive() {
  if [ -n "${URARCHIVE:-}" ]; then
    [ -x "$URARCHIVE" ] || ur_die "URARCHIVE $URARCHIVE is not executable"
    return 0
  fi
  (cd "$BUILD_HOME/all" && GOFLAGS= GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 "${GO:-go}" build -trimpath -o "$1/urarchive" ./urarchive) ||
    ur_die "cannot build urarchive"
  URARCHIVE="$1/urarchive"
  export URARCHIVE
}
