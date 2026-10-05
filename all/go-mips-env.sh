#!/usr/bin/env bash
# Go float-mode environment for MIPS release targets. Sourced by run.sh.
#
#   go_mips_softfloat_env <goarch>
#
# Prints the env assignment that selects soft float for a MIPS GOARCH, or
# nothing for other architectures. Many MIPS routers have no FPU, and a
# hardfloat binary dies on them with an illegal instruction (see
# https://go.dev/wiki/GoMips). The 32-bit arches read GOMIPS; the 64-bit
# arches read GOMIPS64 and ignore GOMIPS, so setting GOMIPS alone silently
# leaves mips64/mips64le as hardfloat.
#
# SPDX-License-Identifier: MPL-2.0

go_mips_softfloat_env() {
    case "$1" in
        mips|mipsle) echo "GOMIPS=softfloat" ;;
        mips64|mips64le) echo "GOMIPS64=softfloat" ;;
    esac
}

#   go_mips_require_softfloat <goarch> <binary>
#
# Fails when the Go binary is missing or does not record soft float for its
# MIPS GOARCH; other architectures pass. Use it on binaries a Makefile built,
# where go_mips_softfloat_env did not choose the float variable.
go_mips_require_softfloat() {
    local float_env
    float_env="$(go_mips_softfloat_env "$1")"
    [ -n "$float_env" ] || return 0
    if ! go version -m "$2" | grep -Eq "^[[:space:]]+build[[:space:]]+${float_env}\$"; then
        echo "error: $2 ($1) is not softfloat" >&2
        return 1
    fi
}
