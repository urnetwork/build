#!/bin/sh
# Tests bringyour-provider.sh, the community provider image's wrapper, against
# a fake provider. It runs as root on Linux, where the image runs it:
#   docker run --rm -v "$PWD:/t:ro" -v "$(mktemp -d):/ro:ro" ubuntu:22.04 sh /t/bringyour-provider.test.sh
# (community_provider_test.go runs it so). /ro, a read-only mount, is
# optional; without it the read-only case is skipped.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
wrapper="$here/bringyour-provider.sh"
[ "$(id -u)" = 0 ] || { echo "SKIP: needs root (run it in a container)"; exit 0; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
log="$work/log"
fake="$work/provider"
cat > "$fake" <<'EOF'
#!/bin/sh
# Records what the wrapper ran; `auth` and `auth-provide` write a jwt.
{
    printf 'args:'
    for arg in "$@"; do printf ' %s' "$arg"; done
    printf '\n'
    printf 'state:%s\n' "${URNETWORK_STATE_DIR:-<unset>}"
    printf 'substituted:%s\n' "${BRINGYOUR_PROVIDER_STATE_DIR:-<unset>}"
    printf 'problem:%s\n' "${BRINGYOUR_PROVIDER_STATE_PROBLEM:-<unset>}"
    if [ -e "/proc/$$/fd/9" ]; then
        printf 'fd9:%s\n' "$(readlink "/proc/$$/fd/9")"
        if flock -n "$(readlink "/proc/$$/fd/9")" true; then
            printf 'locked:no\n'
        else
            printf 'locked:yes\n'
        fi
    else
        printf 'fd9:none\n'
    fi
} > "$FAKE_LOG"
case "$1" in
auth|auth-provide)
    printf 'new-jwt\n' > "${URNETWORK_STATE_DIR:-$HOME/.urnetwork}/jwt"
    ;;
esac
# the miner logs to stderr; the wrapper must not swallow it
echo "fake provider log line" >&2
exit "${FAKE_EXIT:-0}"
EOF
chmod 755 "$fake"

failures=0
fail() {
    echo "FAIL: $*"
    failures=$((failures + 1))
}

# run NAME STATE ARGS...: runs the wrapper with STATE as the state directory
# and a fresh private root; leaves the fake's record in $log and the
# wrapper's stderr in $work/err.
run() {
    name=$1
    state=$2
    shift 2
    private_root="$work/private-$name"
    rm -f "$log"
    if env HOME=/nonexistent URNETWORK_STATE_DIR="$state" FAKE_LOG="$log" \
        BRINGYOUR_PROVIDER_BIN="$fake" BRINGYOUR_PROVIDER_PRIVATE="$private_root" \
        sh "$wrapper" "$@" 2> "$work/err"; then
        status=0
    else
        status=$?
    fi
}

expect() {
    grep -qxF -- "$2" "$log" || fail "$1: expected '$2' in: $(tr '\n' '|' < "$log" 2>/dev/null)"
}

expect_err() {
    grep -qF -- "$2" "$work/err" || fail "$1: expected '$2' on stderr: $(tr '\n' '|' < "$work/err")"
}

mode_of() {
    stat -c '%a' -- "$1"
}

new_state() {
    dir="$work/state-$1"
    mkdir -p "$dir"
    chmod "$2" "$dir"
    echo "$dir"
}

# A missing state directory: the miner makes it. The flag is added.
run missing "$work/missing" provide
expect missing "args: provide --allow-client-registration"
expect missing "state:$work/missing"
expect missing "fd9:none"

# The miner's own directory: in place, locked for as long as it runs.
dir=$(new_state own 0700)
printf 'jwt\n' > "$dir/jwt"
chmod 600 "$dir/jwt"
run own "$dir" provide
expect own "args: provide --allow-client-registration"
expect own "state:$dir"
expect own "fd9:$dir"
expect own "locked:yes"
expect own "substituted:<unset>"
expect_err own "fake provider log line"
[ ! -e "$private_root/private-state" ] || fail "own: private-state marker written"

# The old provider's state: jwt 0700 and a bare key. In place.
dir=$(new_state july 0755)
printf 'jwt\n' > "$dir/jwt"
chmod 700 "$dir/jwt"
head -c 32 /dev/urandom > "$dir/.provider.key"
chmod 600 "$dir/.provider.key"
run july "$dir" provide
expect july "state:$dir"
expect july "locked:yes"

# An old key beside other provider files can't be adopted: private.
printf 'cert\n' > "$dir/.provider.cert"
chmod 600 "$dir/.provider.cert"
run july-cert "$dir" provide
expect july-cert "state:$work/private-july-cert/state"
expect_err july-cert "old provider key beside other provider files"
# ... unless the miner already marked it.
printf 'marker\n' > "$dir/.provider.key.identity"
chmod 600 "$dir/.provider.key.identity"
run july-marked "$dir" provide
expect july-marked "state:$dir"

# Writable by everyone (chmod -R 777): private, seeded with jwt and proxy.
dir=$(new_state world 0777)
printf 'mounted-jwt\n' > "$dir/jwt"
printf '{"servers":{}}\n' > "$dir/proxy"
chmod 777 "$dir/jwt" "$dir/proxy"
run world "$dir" provide
private="$work/private-world/state"
expect world "args: provide --allow-client-registration"
expect world "state:$private"
expect world "fd9:none"
expect_err world "writable by group or others (mode 777)"
expect_err world "Fix: mount a named volume at $dir"
# the miner is told which directory it stands in for and why
expect world "substituted:$dir"
expect world "problem:it is writable by group or others (mode 777)"
[ "$(cat "$private/jwt")" = mounted-jwt ] || fail "world: jwt not seeded"
[ "$(mode_of "$private/jwt")" = 600 ] || fail "world: private jwt mode $(mode_of "$private/jwt")"
[ "$(mode_of "$private")" = 700 ] || fail "world: private dir mode $(mode_of "$private")"
[ -f "$private/proxy" ] || fail "world: proxy not seeded"
[ -f "$work/private-world/private-state" ] || fail "world: no private-state marker"
# A restart keeps the private jwt (the miner renews it there) and follows
# the directory's proxy settings, even when they are removed.
printf 'renewed-jwt\n' > "$private/jwt"
rm "$dir/proxy"
env HOME=/nonexistent URNETWORK_STATE_DIR="$dir" FAKE_LOG="$log" BRINGYOUR_PROVIDER_BIN="$fake" \
    BRINGYOUR_PROVIDER_PRIVATE="$work/private-world" sh "$wrapper" provide 2>/dev/null
[ "$(cat "$private/jwt")" = renewed-jwt ] || fail "world restart: private jwt replaced"
[ ! -e "$private/proxy" ] || fail "world restart: removed proxy settings kept"
# status in this container reads the private state
env HOME=/nonexistent URNETWORK_STATE_DIR= FAKE_LOG="$log" BRINGYOUR_PROVIDER_BIN="$fake" \
    BRINGYOUR_PROVIDER_PRIVATE="$work/private-world" sh "$wrapper" status 2>/dev/null
expect world-status "args: status"
expect world-status "state:$private"
expect world-status "substituted:$dir"
expect world-status "problem:it is writable by group or others (mode 777)"

# Owned by the host user (a bind mount): private.
dir=$(new_state hostuser 0755)
chown 1000:1000 "$dir"
run hostuser "$dir" provide
expect hostuser "state:$work/private-hostuser/state"
expect_err hostuser "it is owned by uid 1000, and this container runs as uid 0"
expect hostuser "substituted:$dir"
expect hostuser "problem:it is owned by uid 1000, and this container runs as uid 0"

# A jwt others can read: private.
dir=$(new_state openjwt 0755)
printf 'jwt\n' > "$dir/jwt"
chmod 644 "$dir/jwt"
run openjwt "$dir" provide
expect openjwt "state:$work/private-openjwt/state"
expect_err openjwt "jwt is not a private file"

# A symbolic link: private.
ln -s "$work/state-own" "$work/state-link"
run link "$work/state-link" provide
expect link "state:$work/private-link/state"

# Shared by a running container: the second runs on private state.
dir=$(new_state shared 0700)
flock -n "$dir" sleep 30 &
holder=$!
sleep 1
run shared "$dir" provide
expect shared "state:$work/private-shared/state"
expect_err shared "another provider uses it"
expect_err shared "fake provider log line"
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true

# A read-only mount: private.
if [ -d /ro ]; then
    run readonly /ro provide
    expect readonly "state:$work/private-readonly/state"
    expect_err readonly "read-only"
else
    echo "SKIP: read-only case (no /ro mount)"
fi

# Arguments that choose registration themselves are left alone.
dir=$(new_state own2 0700)
for choice in --allow-client-registration --adopt-legacy-provider-key \
    --whole-work-capture=/p --original-contract-capture=/p \
    --require-whole-work-capture --require-original-contract-capture; do
    run choice "$dir" provide "$choice"
    expect "choice $choice" "args: provide $choice"
done
run help "$dir" provide --help
expect help "args: provide --help"
expect help "fd9:none"

# The flag goes right after the command, before any other argument.
run authprovide "$dir" auth-provide CODE -f
expect authprovide "args: auth-provide --allow-client-registration CODE -f"
run start "$dir" start
expect start "args: start --allow-client-registration"
run allops "$dir" provide --all-operators -v
expect allops "args: provide --allow-client-registration --all-operators -v"

# Other commands run unchanged.
for command in "--version" "proxy add 203.0.113.7:1080:u:p" "wallet set 5Fabc" "operators"; do
    # shellcheck disable=SC2086
    run passthrough "$dir" $command
    expect "passthrough $command" "args: $command"
    expect "passthrough $command" "fd9:none"
done

# auth in the miner's own directory runs in place.
dir=$(new_state authown 0700)
run authown "$dir" auth CODE
expect authown "args: auth CODE"
expect authown "state:$dir"
[ "$(cat "$dir/jwt")" = new-jwt ] || fail "authown: jwt not written"

# auth in a directory the miner refuses writes the jwt through a private one.
dir=$(new_state authworld 0777)
run authworld "$dir" auth CODE
expect authworld "args: auth CODE"
grep -q "^state:${TMPDIR:-/tmp}/bringyour-provider-auth\." "$log" || fail "authworld: not run on a private directory: $(cat "$log")"
[ "$status" = 0 ] || fail "authworld: status $status"
[ "$(cat "$dir/jwt" 2>/dev/null)" = new-jwt ] || fail "authworld: jwt not written into the directory"
[ "$(mode_of "$dir/jwt")" = 600 ] || fail "authworld: jwt mode $(mode_of "$dir/jwt")"
ls -a "$dir" | grep -q '^\.jwt\.' && fail "authworld: temporary jwt left behind"
ls -d "${TMPDIR:-/tmp}"/bringyour-provider-auth.* >/dev/null 2>&1 && fail "authworld: private auth directory left behind"
# A failed auth leaves the directory's jwt alone and keeps its status.
printf 'old-jwt\n' > "$dir/jwt"
FAKE_EXIT=3
export FAKE_EXIT
run authfail "$dir" auth CODE
unset FAKE_EXIT
[ "$status" = 3 ] || fail "authfail: status $status"
# (the fake wrote new-jwt into the private directory, which is discarded)
[ "$(cat "$dir/jwt")" = old-jwt ] || fail "authfail: jwt replaced"
# --operator runs in place
run authop "$dir" auth --operator=example.com CODE
expect authop "state:$dir"

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)"
    exit 1
fi
echo "PASS: bringyour-provider.sh"
