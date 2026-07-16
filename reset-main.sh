#!/usr/bin/env bash
# reset-main.sh — put every submodule of the build repo on the latest tip of its
# production branch (main, or master for the few repos that still use it) and
# leave the resulting submodule-pointer changes UNCOMMITTED in the build repo's
# working tree.
#
# This is for local testing: it moves every repo to the freshest main so you can
# build/test against latest, without staging a release or committing anything.
#
# By default it operates on ALL submodules. Pass one or more submodule paths to
# limit it, e.g.:  ./reset-main.sh sdk connect server
#
# Per submodule it:
#   1. initialises it if it isn't checked out yet
#   2. git fetch --prune origin
#   3. picks the branch: prefer origin/main, else origin/master, else origin HEAD
#   4. git checkout -B <branch> origin/<branch>   (reset local branch to origin)
#
# It does NOT commit anything and does NOT run `git submodule update`, so the
# updated submodule commits show up as pending changes in `git status` of the
# build repo — exactly what you want for a local test build. To throw the
# changes away afterwards and return to the recorded commits:
#   git submodule update --init
#
# Non-destructive to uncommitted work: if a submodule has local changes that a
# checkout would clobber, its checkout aborts and the repo is reported [FAIL]
# rather than silently reset — fix it by hand and re-run. Exits non-zero if any
# submodule could not be reset.

set -u -o pipefail

indent() { sed 's/^/    /'; }

# Operate from the build repo root (this script's directory).
BUILD_HOME="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$BUILD_HOME"

if [ ! -f .gitmodules ]; then
    echo "error: no .gitmodules in $BUILD_HOME — run this from the build repo root" >&2
    exit 1
fi

# All submodule paths, in .gitmodules order.
all_paths=()
while IFS= read -r p; do
    all_paths+=("$p")
done < <(git config --file .gitmodules --get-regexp '^submodule\..*\.path$' | awk '{print $2}')

if [ "${#all_paths[@]}" -eq 0 ]; then
    echo "error: no submodules found in .gitmodules" >&2
    exit 1
fi

# Targets: the given paths (validated), or every submodule when none are given.
targets=()
if [ "$#" -gt 0 ]; then
    for want in "$@"; do
        found=0
        for p in "${all_paths[@]}"; do
            [ "$p" = "$want" ] && { found=1; break; }
        done
        if [ "$found" -eq 1 ]; then
            targets+=("$want")
        else
            echo "error: '$want' is not a submodule (see .gitmodules)" >&2
            exit 1
        fi
    done
else
    targets=("${all_paths[@]}")
fi

echo "reset-main: resetting ${#targets[@]} submodule(s) in $BUILD_HOME to latest origin"
echo

ok=()
failed=()

for sm in "${targets[@]}"; do
    echo "==> $sm"

    # Initialise if the submodule work tree isn't checked out yet.
    if ! git -C "$sm" rev-parse --git-dir >/dev/null 2>&1; then
        echo "    initialising..."
        if ! git submodule update --init -- "$sm" 2>&1 | indent; then
            echo "    [FAIL] could not initialise" >&2
            failed+=("$sm")
            continue
        fi
    fi

    old="$(git -C "$sm" rev-parse --short HEAD 2>/dev/null || echo '?')"

    # Refresh remote-tracking branches so origin/<branch> is the real latest.
    if ! err="$(git -C "$sm" fetch --prune --quiet origin 2>&1)"; then
        [ -n "$err" ] && printf '%s\n' "$err" | indent >&2
        echo "    [FAIL] git fetch failed" >&2
        failed+=("$sm")
        continue
    fi

    # Pick the production branch: prefer main, then master (a few repos use it),
    # then fall back to whatever origin's default HEAD points at.
    if git -C "$sm" show-ref --verify --quiet refs/remotes/origin/main; then
        branch=main
    elif git -C "$sm" show-ref --verify --quiet refs/remotes/origin/master; then
        branch=master
    else
        git -C "$sm" remote set-head origin --auto >/dev/null 2>&1 || true
        branch="$(git -C "$sm" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null)"
        branch="${branch#origin/}"
        if [ -z "$branch" ]; then
            echo "    [FAIL] no origin/main or origin/master and no default HEAD" >&2
            failed+=("$sm")
            continue
        fi
    fi

    # Reset the local branch to the latest origin tip and check it out. This
    # aborts (non-zero) rather than clobber conflicting uncommitted changes.
    if out="$(git -C "$sm" checkout -B "$branch" "origin/$branch" 2>&1)"; then
        [ -n "$out" ] && printf '%s\n' "$out" | indent
    else
        [ -n "$out" ] && printf '%s\n' "$out" | indent >&2
        echo "    [FAIL] checkout $branch failed (local changes in the way?)" >&2
        failed+=("$sm")
        continue
    fi

    new="$(git -C "$sm" rev-parse --short HEAD 2>/dev/null || echo '?')"
    if [ "$old" = "$new" ]; then
        echo "    [ok] $branch @ $new (unchanged)"
    else
        echo "    [ok] $branch @ $new (was $old)"
    fi
    ok+=("$sm")
done

echo
echo "done: ${#ok[@]} reset, ${#failed[@]} failed"
if [ "${#failed[@]}" -gt 0 ]; then
    echo "failed: ${failed[*]}" >&2
fi

echo
echo "Submodule state (pointer changes are left UNCOMMITTED in the build repo):"
git submodule status
echo
echo "Left uncommitted on purpose so you can run local test builds against latest."
echo "To discard and return to the recorded commits: git submodule update --init"

[ "${#failed[@]}" -eq 0 ]
