#!/usr/bin/env bash
# SPDX-License-Identifier: MPL-2.0
#
# Resolve the requested Windows artifact architectures without starting a build.
# The ordinary release plan remains amd64+arm64; focused callers may request one
# architecture explicitly. Values become PowerShell command fragments only after
# this exact validation, so arbitrary input never reaches the guest shell.

win_windows_build_plan() {
  WIN_BUILD_ARCHITECTURES="${WINDOWS_BUILD_ARCHITECTURES:-amd64,arm64}"
  case "$WIN_BUILD_ARCHITECTURES" in
    amd64,arm64)
      WIN_BUILD_SDK_POWERSHELL_ARGUMENTS=""
      WIN_BUILD_APP_POWERSHELL_ARGUMENTS=""
      WIN_BUILD_MSI_SUFFIXES=(x64 arm64)
      ;;
    amd64)
      WIN_BUILD_SDK_POWERSHELL_ARGUMENTS="-Architectures amd64"
      WIN_BUILD_APP_POWERSHELL_ARGUMENTS="-Platforms x64"
      WIN_BUILD_MSI_SUFFIXES=(x64)
      ;;
    arm64)
      WIN_BUILD_SDK_POWERSHELL_ARGUMENTS="-Architectures arm64"
      WIN_BUILD_APP_POWERSHELL_ARGUMENTS="-Platforms ARM64"
      WIN_BUILD_MSI_SUFFIXES=(arm64)
      ;;
    *)
      echo "ERROR: WINDOWS_BUILD_ARCHITECTURES must be amd64, arm64, or amd64,arm64" >&2
      return 2
      ;;
  esac

  WIN_BUILD_SKIP_CONTRACT_TESTS="${WINDOWS_BUILD_SKIP_CONTRACT_TESTS:-0}"
  case "$WIN_BUILD_SKIP_CONTRACT_TESTS" in
    0|1) ;;
    *)
      echo "ERROR: WINDOWS_BUILD_SKIP_CONTRACT_TESTS must be 0 or 1" >&2
      return 2
      ;;
  esac

  export WINDOWS_BUILD_ARCHITECTURES="$WIN_BUILD_ARCHITECTURES"
  export WINDOWS_BUILD_SKIP_CONTRACT_TESTS="$WIN_BUILD_SKIP_CONTRACT_TESTS"
  export WIN_BUILD_ARCHITECTURES
  export WIN_BUILD_SDK_POWERSHELL_ARGUMENTS
  export WIN_BUILD_APP_POWERSHELL_ARGUMENTS
  export WIN_BUILD_SKIP_CONTRACT_TESTS
}

# Require every requested MSI and reject extras. The caller clears its private
# output directory before the build, so an unexpected file is stale or evidence
# that the guest ignored the selected architecture plan.
win_windows_verify_msi_outputs() {
  local output_directory="$1"
  local version="$2"
  local suffix candidate candidate_name expected

  for suffix in "${WIN_BUILD_MSI_SUFFIXES[@]}"; do
    candidate="$output_directory/URnetwork-$version-$suffix.msi"
    if [ ! -s "$candidate" ]; then
      echo "ERROR: requested Windows artifact is missing or empty: $candidate" >&2
      return 1
    fi
  done

  for candidate in "$output_directory"/*.msi; do
    [ -e "$candidate" ] || continue
    candidate_name="${candidate##*/}"
    expected=0
    for suffix in "${WIN_BUILD_MSI_SUFFIXES[@]}"; do
      if [ "$candidate_name" = "URnetwork-$version-$suffix.msi" ]; then
        expected=1
        break
      fi
    done
    if [ "$expected" -ne 1 ]; then
      echo "ERROR: Windows build produced an unrequested MSI: $candidate" >&2
      return 1
    fi
  done
}
