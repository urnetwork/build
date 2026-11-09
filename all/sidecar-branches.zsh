#!/usr/bin/env zsh
# One commit set across the sidecar branches (§4.8, D-REPO-6, REPO-M0-03).
#
#   check                          report the branches, the pins and the commit-msg scan; change nothing
#   repin                          write each sibling's sidecar commit into server's source-graph lock and
#                                  build's gitlinks, and commit both
#   merge-in                       merge main into sidecar in every repo, in §4.8's order, stopping at the
#                                  first conflict, then re-pin
#   merge-out <gate> <commit-set>  merge a gate's recorded commit set into main, connect first, once each
#                                  commit's commit-msg scan and X9's TestX9ReadyForMain pass
#   scan                           run the commit-msg scan of main..sidecar in every repo
#
# §4.8's order is connect, sidecar, sdk, sn, server, warp/config/vault/xops/mmm, build. minio joins the
# middle group, and its master plays main (design-records-minio-merge-rules, decided 2026-11-06).
#
# Merge, never rebase: a rebase orphans pinned commits and skips the commit-msg hook, so nothing here
# rebases or pulls. Nothing here pushes either: each command that commits prints the pushes for the owner.
# merge-out takes the commit set a gate recorded (server/local/sidecar-gate's report, or lines of
# "<repo> <commit>"), never the tip, as design-records-gn-out-merge-commits decided. Agents run this on
# fixture repositories only (the brief: no push, no merge to main); a real merge-in, merge-out or repin
# is the owner's.
#
# The monoroot of sibling checkouts is SIDECAR_BRANCHES_ROOT, by default the directory holding build.
#
# exit: 0 done; 1 refused, stopped or a check failed; 64 usage
#
# SPDX-License-Identifier: MPL-2.0

emulate -R zsh
setopt pipe_fail no_unset

SB_SCRIPT="${0:A}"
SB_ROOT="${SIDECAR_BRANCHES_ROOT:-${SB_SCRIPT:h:h:h}}"
SB_HOOK="${SB_SCRIPT:h:h}/.githooks/commit-msg"
SB_LOCK_PATH="local/source-graph/lock.yml"
# §4.8's order, with minio in the middle group
SB_ORDER=(connect sidecar sdk sn server warp config vault xops mmm minio build)

sb_usage () {
    print -u2 "usage: ${SB_SCRIPT:t} check | repin | merge-in | merge-out <gate> <commit-set> | scan"
    exit 64
}

sb_git () {
    local repo="$1"
    shift
    git -C "$SB_ROOT/$repo" "$@"
}

# The branch that plays main: minio's is upstream's master.
sb_main_branch () {
    case "$1" in
        minio) print -r -- master ;;
        *) print -r -- main ;;
    esac
}

sb_present () {
    [[ -e "$SB_ROOT/$1/.git" ]]
}

sb_has_branch () {
    sb_git "$1" rev-parse --verify --quiet "refs/heads/$2" >/dev/null
}

sb_clean () {
    [[ -z "$(sb_git "$1" status --porcelain --untracked-files=all)" ]]
}

sb_on_sidecar () {
    [[ "$(sb_git "$1" symbolic-ref --quiet --short HEAD 2>/dev/null)" == sidecar ]]
}

# The siblings server's lock pins, by path.
sb_lock_paths () {
    awk '/^[ \t]*path:/ { print $2 }' "$SB_ROOT/server/$SB_LOCK_PATH"
}

sb_lock_ref () {
    awk -v want="$1" '/^[ \t]*path:/ { path = $2 } /^[ \t]*ref:/ && path == want { print $2; exit }' "$SB_ROOT/server/$SB_LOCK_PATH"
}

# build's gitlinks on its sidecar branch, as "<path> <commit>".
sb_gitlinks () {
    sb_git build ls-tree sidecar | awk '$1 == "160000" { print $4, $3 }'
}

sb_gitlink () {
    sb_gitlinks | awk -v want="$1" '$1 == want { print $2 }'
}

# The commit a re-pin writes for a sibling: its sidecar branch's, where it has one, which must be what
# it has checked out, or else its checked-out commit, which some branch must hold so the pin stays
# reachable. A dirty tree is refused, since the pin would not be what builds.
sb_pin_of () {
    local repo="$1" head
    if ! sb_present "$repo"; then
        print -u2 "repin: $repo is not checked out at $SB_ROOT/$repo"
        return 1
    fi
    if ! sb_clean "$repo"; then
        print -u2 "repin: $repo has changes outside a commit, so its pin would not be what builds"
        return 1
    fi
    head=$(sb_git "$repo" rev-parse --verify HEAD) || return 1
    if sb_has_branch "$repo" sidecar; then
        if [[ "$head" != "$(sb_git "$repo" rev-parse refs/heads/sidecar)" ]]; then
            print -u2 "repin: $repo has $head checked out, not its sidecar branch"
            return 1
        fi
    elif [[ -z "$(sb_git "$repo" for-each-ref --contains "$head" --format='%(refname)' refs/heads refs/remotes)" ]]; then
        print -u2 "repin: $repo's $head is on no branch, so a pin to it would not stay reachable"
        return 1
    fi
    print -r -- "$head"
}

# What to pin: every sibling the lock names, and every submodule of build's with a sidecar branch here.
sb_pin_paths () {
    local -aU siblings
    local sibling commit
    siblings=($(sb_lock_paths))
    sb_gitlinks | while read -r sibling commit; do
        if [[ "$sibling" != server && "$sibling" != build ]] && sb_present "$sibling" && sb_has_branch "$sibling" sidecar; then
            siblings+=("$sibling")
        fi
    done
    print -rl -- "${siblings[@]}"
}

sb_repin () {
    local repo sibling pin lock_temp
    local -A pins
    for repo in server build; do
        if ! sb_present "$repo" || ! sb_on_sidecar "$repo" || ! sb_clean "$repo"; then
            print -u2 "repin: $repo must be a clean checkout of its sidecar branch"
            return 1
        fi
    done
    for sibling in $(sb_pin_paths); do
        pin=$(sb_pin_of "$sibling") || return 1
        pins[$sibling]="$pin"
    done

    lock_temp=$(mktemp) || return 1
    awk -v spec="${(j: :)${(kv@)pins}}" '
        BEGIN { n = split(spec, items, " "); for (i = 1; i < n; i += 2) pin[items[i]] = items[i + 1] }
        /^[ \t]*path:/ { path = $2 }
        /^[ \t]*ref:/ && (path in pin) { sub(/ref:.*/, "ref: " pin[path]); path = "" }
        { print }
    ' "$SB_ROOT/server/$SB_LOCK_PATH" > "$lock_temp" || { rm -f "$lock_temp"; return 1 }
    cat "$lock_temp" > "$SB_ROOT/server/$SB_LOCK_PATH" || { rm -f "$lock_temp"; return 1 }
    rm -f "$lock_temp"
    sb_git server add -- "$SB_LOCK_PATH" || return 1
    if ! sb_git server diff --cached --quiet; then
        sb_git server commit -q -m "Re-pin the source-graph lock to each sibling's sidecar commit" -- "$SB_LOCK_PATH" || return 1
        print -r -- "repin: committed server's lock"
        print -r -- "owner: git -C $SB_ROOT/server push origin sidecar"
    fi

    pins[server]=$(sb_git server rev-parse HEAD) || return 1
    for sibling in ${(k)pins}; do
        if [[ -n "$(sb_gitlink "$sibling")" ]]; then
            sb_git build update-index --cacheinfo "160000,${pins[$sibling]},$sibling" || return 1
        fi
    done
    if ! sb_git build diff --cached --quiet; then
        sb_git build commit -q -m "Re-pin the submodules to each sibling's sidecar commit" || return 1
        print -r -- "repin: committed build's gitlinks"
        print -r -- "owner: git -C $SB_ROOT/build push origin sidecar"
    fi
    print -r -- "repin: server's lock and build's gitlinks pin the same commits"
}

# Fast-forwards a repo's local main to origin's when origin is ahead. A local main ahead of origin's is
# kept, and one that has diverged stops: merging either side would take a choice that is the owner's.
sb_update_main () {
    local repo="$1" main="$2"
    if ! sb_git "$repo" remote get-url origin >/dev/null 2>&1; then
        return 0
    fi
    if ! sb_git "$repo" fetch -q origin "refs/heads/$main" 2>/dev/null; then
        return 0
    fi
    if sb_git "$repo" merge-base --is-ancestor "refs/heads/$main" FETCH_HEAD; then
        sb_git "$repo" update-ref "refs/heads/$main" FETCH_HEAD || return 1
    elif ! sb_git "$repo" merge-base --is-ancestor FETCH_HEAD "refs/heads/$main"; then
        print -u2 "merge-in stopped at $repo: its $main and origin's have diverged"
        return 1
    fi
}

sb_merge_in () {
    local repo main
    local -a merged
    for repo in "${SB_ORDER[@]}"; do
        if ! sb_present "$repo"; then
            print -r -- "merge-in: $repo is not checked out here, skipped"
            continue
        fi
        main=$(sb_main_branch "$repo")
        if ! sb_on_sidecar "$repo" || ! sb_clean "$repo"; then
            print -u2 "merge-in stopped at $repo: it must be a clean checkout of its sidecar branch. Nothing after it is merged."
            return 1
        fi
        sb_update_main "$repo" "$main" || return 1
        if ! sb_has_branch "$repo" "$main"; then
            print -u2 "merge-in stopped at $repo: it has no $main branch"
            return 1
        fi
        if ! sb_git "$repo" merge -q --no-edit -m "Merge $main into sidecar" "refs/heads/$main"; then
            print -u2 "merge-in stopped at $repo: merging $main into sidecar conflicts. Resolve it there, commit, and run merge-in again; nothing after $repo is merged and nothing is re-pinned."
            return 1
        fi
        print -r -- "merge-in: merged $main into $repo's sidecar"
        merged+=("$repo")
    done
    sb_repin || return 1
    for repo in "${merged[@]}"; do
        print -r -- "owner: git -C $SB_ROOT/$repo push origin sidecar"
    done
}

# The commit-msg scan of a range in one repo, with its findings.
sb_scan_range () {
    local repo="$1" range="$2"
    if [[ ! -x "$SB_HOOK" ]]; then
        print -u2 "scan: the commit-msg hook is not at $SB_HOOK, so nothing was checked"
        return 1
    fi
    (cd "$SB_ROOT/$repo" && "$SB_HOOK" --scan "$range")
}

sb_scan () {
    local repo main failed=0
    for repo in "${SB_ORDER[@]}"; do
        if ! sb_present "$repo" || ! sb_has_branch "$repo" sidecar; then
            print -r -- "scan: $repo has no sidecar branch here, skipped"
            continue
        fi
        main=$(sb_main_branch "$repo")
        if sb_scan_range "$repo" "refs/heads/$main..refs/heads/sidecar"; then
            print -r -- "scan: $repo clean"
        else
            print -r -- "scan: $repo carries attributed commits or could not be scanned"
            failed=1
        fi
    done
    return $failed
}

# Every report check makes, with nothing changed.
sb_check () {
    local repo main failed=0 sibling lock_ref link
    for repo in "${SB_ORDER[@]}"; do
        if ! sb_present "$repo"; then
            print -r -- "check: $repo is not checked out here"
            continue
        fi
        main=$(sb_main_branch "$repo")
        if ! sb_has_branch "$repo" sidecar; then
            print -r -- "check: $repo has no sidecar branch"
            failed=1
            continue
        fi
        print -r -- "check: $repo on $(sb_git "$repo" symbolic-ref --quiet --short HEAD 2>/dev/null || print -r -- detached), sidecar $(sb_git "$repo" rev-list --count "refs/heads/$main..refs/heads/sidecar" 2>/dev/null || print -r -- '?') ahead of $main and $(sb_git "$repo" rev-list --count "refs/heads/sidecar..refs/heads/$main" 2>/dev/null || print -r -- '?') behind"
        if ! sb_clean "$repo"; then
            print -r -- "check: $repo has changes outside a commit"
            failed=1
        fi
    done
    if sb_present server && sb_present build; then
        for sibling in $(sb_lock_paths); do
            lock_ref=$(sb_lock_ref "$sibling")
            link=$(sb_gitlink "$sibling")
            if [[ -n "$link" && "$link" != "$lock_ref" ]]; then
                print -r -- "check: server's lock pins $sibling at $lock_ref and build's sidecar gitlink at $link"
                failed=1
            fi
            if sb_present "$sibling" && [[ "$(sb_git "$sibling" rev-parse HEAD)" != "$lock_ref" ]]; then
                print -r -- "check: $sibling has moved past its pin $lock_ref, so a re-pin is due"
                failed=1
            fi
        done
    fi
    sb_scan || failed=1
    return $failed
}

# The commit set a gate recorded: its report's "| repo | branch | commit |" rows or "<repo> <commit>" lines.
typeset -gA sb_recorded
sb_read_commit_set () {
    local line
    if [[ ! -r "$1" ]]; then
        print -u2 "merge-out: cannot read the commit set $1"
        return 1
    fi
    while IFS= read -r line; do
        if [[ "$line" =~ '^\|[ ]*([a-z]+)[ ]*\|[^|]*\|[ ]*([0-9a-f]{40})[ ]*\|' ]]; then
            sb_recorded[$match[1]]="$match[2]"
        elif [[ "$line" =~ '^([a-z]+)[ ]+([0-9a-f]{40})[ ]*$' ]]; then
            sb_recorded[$match[1]]="$match[2]"
        fi
    done < "$1"
    if (( ${#sb_recorded} == 0 )); then
        print -u2 "merge-out: $1 records no commit set"
        return 1
    fi
}

# Runs X9's TestX9ReadyForMain at the gate over the recorded sidecar and connect, each in a detached
# worktree beside the other siblings, and needs the test's own pass line: a run that matched no test, or
# skipped it, is not a pass.
sb_x9_ready () {
    local gate="$1" directory output x9_status sibling
    if [[ -z "${sb_recorded[sidecar]:-}" || -z "${sb_recorded[connect]:-}" ]]; then
        print -u2 "merge-out: the commit set records no sidecar or no connect commit, so nothing shows X9 holds"
        return 1
    fi
    directory=$(mktemp -d) || return 1
    if ! sb_git sidecar worktree add -q --detach "$directory/sidecar" "${sb_recorded[sidecar]}" ||
       ! sb_git connect worktree add -q --detach "$directory/connect" "${sb_recorded[connect]}"; then
        sb_git sidecar worktree remove --force "$directory/sidecar" 2>/dev/null
        rm -rf "$directory"
        return 1
    fi
    for sibling in "$SB_ROOT"/*(N); do
        [[ -e "$directory/${sibling:t}" ]] || ln -s "$sibling" "$directory/${sibling:t}"
    done
    output=$(cd "$directory/sidecar/check" && SIDECAR_X9_GATE="$gate" GOWORK=off GOTOOLCHAIN=local \
        go test -count=1 -v -tags sidecar_check -run '^TestX9ReadyForMain$' . 2>&1)
    x9_status=$?
    sb_git sidecar worktree remove --force "$directory/sidecar"
    sb_git connect worktree remove --force "$directory/connect"
    rm -rf "$directory"
    if (( x9_status != 0 )) || [[ "$output" != *"--- PASS: TestX9ReadyForMain"* ]]; then
        print -r -- "$output"
        print -u2 "merge-out: TestX9ReadyForMain did not pass at $gate, so nothing reaches main"
        return 1
    fi
    print -r -- "merge-out: TestX9ReadyForMain passed at $gate"
}

sb_merge_out () {
    local gate="$1" set_file="$2" repo main commit directory
    local -a merged
    if [[ ! "$gate" =~ '^M(10|[0-9])$' ]]; then
        print -u2 "merge-out: $gate is not a gate M0 to M10"
        return 64
    fi
    sb_read_commit_set "$set_file" || return 1
    # every check before any merge, so a refusal changes nothing
    for repo in ${(k)sb_recorded}; do
        if (( ${SB_ORDER[(Ie)$repo]} == 0 )); then
            print -u2 "merge-out: the commit set names $repo, which §4.8's order does not"
            return 1
        fi
    done
    for repo in "${SB_ORDER[@]}"; do
        commit="${sb_recorded[$repo]:-}"
        if [[ -z "$commit" ]]; then
            print -r -- "merge-out: the commit set records no $repo commit, so $repo is not merged"
            continue
        fi
        main=$(sb_main_branch "$repo")
        if ! sb_present "$repo" || ! sb_git "$repo" cat-file -e "$commit^{commit}" 2>/dev/null; then
            print -u2 "merge-out: $repo holds no commit $commit"
            return 1
        fi
        if ! sb_has_branch "$repo" sidecar || ! sb_git "$repo" merge-base --is-ancestor "$commit" refs/heads/sidecar; then
            print -u2 "merge-out: $repo's recorded $commit is not on its sidecar branch"
            return 1
        fi
        if ! sb_has_branch "$repo" "$main"; then
            print -u2 "merge-out: $repo has no $main branch"
            return 1
        fi
        if ! sb_scan_range "$repo" "refs/heads/$main..$commit"; then
            print -u2 "merge-out: $repo's commits up to $commit fail the commit-msg scan, so nothing reaches main"
            return 1
        fi
    done
    sb_x9_ready "$gate" || return 1

    for repo in "${SB_ORDER[@]}"; do
        commit="${sb_recorded[$repo]:-}"
        [[ -n "$commit" ]] || continue
        main=$(sb_main_branch "$repo")
        directory=$(mktemp -d) || return 1
        if ! sb_git "$repo" worktree add -q "$directory/$repo" "$main"; then
            print -u2 "merge-out stopped at $repo: its $main could not be checked out beside sidecar"
            rm -rf "$directory"
            return 1
        fi
        if ! git -C "$directory/$repo" merge -q --no-ff --no-edit -m "Merge the $gate gate's sidecar commit set into $main" "$commit"; then
            print -u2 "merge-out stopped at $repo: merging $commit into $main conflicts in $directory/$repo. Resolve it there and commit; nothing after $repo is merged."
            return 1
        fi
        sb_git "$repo" worktree remove "$directory/$repo"
        rm -rf "$directory"
        print -r -- "merge-out: merged $repo's $commit into $main"
        merged+=("$repo")
    done
    for repo in "${merged[@]}"; do
        print -r -- "owner: git -C $SB_ROOT/$repo push origin $(sb_main_branch "$repo")"
    done
}

(( $# >= 1 )) || sb_usage
case "$1" in
    check) (( $# == 1 )) || sb_usage; sb_check ;;
    repin) (( $# == 1 )) || sb_usage; sb_repin ;;
    merge-in) (( $# == 1 )) || sb_usage; sb_merge_in ;;
    merge-out) (( $# == 3 )) || sb_usage; sb_merge_out "$2" "$3" ;;
    scan) (( $# == 1 )) || sb_usage; sb_scan ;;
    *) sb_usage ;;
esac
