# Sourced by the canonical release entrypoint, never by generated state files.
release_state() {
    python3 "$BUILD_HOME/all/release-state.py" "$1" "$WARP_HOME" "$EXTERNAL_WARP_VERSION" "${@:2}"
}

release_sdk_outputs() {
    release_state sdk-outputs "$1"
}

# Only exit 10 means a verified completed stage. Corruption, changed inputs and
# failed inspection terminate the run rather than falling into a rebuild.
release_build() {
    local result
    release_state begin "$@"
    result=$?
    case "$result" in
        0) return 0 ;;
        10) return 1 ;;
        *) release_failure_hint; exit "$result" ;;
    esac
}

release_complete() {
    release_state complete "$@"
    local result=$?
    if (( result != 0 )); then release_failure_hint; exit "$result"; fi
}

release_transition() {
    release_state transition "$1" || exit $?
}

release_failure_hint() {
    if [[ -n "${EXTERNAL_WARP_VERSION:-}" && -f "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/manifest.json" ]]; then
        if ! release_state failure-hint >&2; then
            print -u2 -- 'Cannot verify the stopped release journal. Preserve its logs and receipts; automatic resume eligibility has not been established.'
        fi
    else
        print -u2 -- 'This invocation has not sealed immutable release preparation and cannot resume. Preserve the failure evidence, then start a new full build.'
    fi
}

# The operation's intent is durable before altool starts. A failed/terminated
# upload may have reached Apple, so neither a duplicate upload nor a skipped
# upload is safe. A successful exit is the only automatic success observation.
release_apple_upload() {
    local platform="$1" file="$2" observed
    observed=$(release_state effect-begin "apple-$platform-upload") || return $?
    case "$observed" in
        complete) print -r -- "Apple $platform upload already completed for this sealed release"; return 0 ;;
        uncertain)
            print -u2 -- "Apple $platform upload has an uncertain result. Inspect App Store Connect for this exact version/build. Do not re-upload or mark success without a matching observed build receipt."
            return 65 ;;
    esac
    xcrun altool --show-progress --upload-app --file "$file" -t "$platform" --apiKey "$APPLE_API_KEY" --apiIssuer "$APPLE_API_ISSUER" || return $?
    release_state effect-complete "apple-$platform-upload"
}

# Registry reads compare the full manifest bytes, including every platform's
# digest. Reuse is permitted only while the immutable version tag still names
# exactly that manifest. The receipt lives privately outside generated trees.
release_image_receipt() {
    local service="$1" target="$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/image-$1.json"
    local probe
    probe=$(mktemp "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/.image.XXXXXXXX") || return $?
    if ! docker buildx imagetools inspect --raw "bringyour/${BUILD_ENV}-${service}:${EXTERNAL_WARP_VERSION}" > "$probe"; then
        rm -f "$probe"
        return 1
    fi
    if [[ -f "$target" ]]; then
        cmp -s "$probe" "$target"
        local result=$?
        rm -f "$probe"
        if (( result != 0 )); then
            print -u2 -- "Versioned image changed: $service $EXTERNAL_WARP_VERSION; refusing to replace published content"
            return 65
        fi
    else
        mv "$probe" "$target" || return $?
    fi
    print -r -- "$target"
}

release_image_absent() {
    local diagnostic result
    diagnostic=$(mktemp "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/.registry.XXXXXXXX") || return $?
    docker buildx imagetools inspect --raw "bringyour/${BUILD_ENV}-$1:${EXTERNAL_WARP_VERSION}" >/dev/null 2>"$diagnostic"
    result=$?
    if (( result == 0 )); then
        print -u2 -- "Image $1 already exists without a completed checkpoint; refusing to overwrite an unverified publication. Reconcile the exact image or start a new release."
        rm -f "$diagnostic"
        return 65
    fi
    # Bare "not found" can describe a missing credential helper, not a
    # missing manifest. Accept only the exact image reference or OCI's named
    # manifest-absence errors, and never an accompanying auth/transport error.
    if grep -Eqi 'unauthorized|denied|forbidden|credential|authentication|token|transport|tls|certificate|timeout|timed out' "$diagnostic" ||
       { ! grep -Eqi 'manifest unknown|no such manifest' "$diagnostic" &&
         ! grep -F "docker.io/bringyour/${BUILD_ENV}-$1:${EXTERNAL_WARP_VERSION}: not found" "$diagnostic" >/dev/null; }; then
        print -u2 -- "Cannot establish that image $1 is absent (registry/auth/transport error); no image will be replaced"
        rm -f "$diagnostic"
        return 65
    fi
    rm -f "$diagnostic"
}

# BuildKit may continue a registry push after every host-side process exits.
# Tag absence is not proof that an earlier submitted job cannot publish later.
# The absence probe is read-only; persist intent before the black-box build or
# import can start, and never automatically replay a previously recorded one.
release_image_publication_begin() {
    local service="$1" observed
    release_image_absent "$service" || return $?
    observed=$(release_state effect-begin "image-$service-publication") || return $?
    if [[ -n "$observed" ]]; then
        print -u2 -- "Image $service publication was already attempted without a completed checkpoint. A daemon-owned BuildKit job may still publish even though the tag is currently absent. Verify the old job and exact version with the owning operator; do not retry or clear its intent. Start a new release after owned cleanup if authoritative reconciliation is unavailable."
        return 65
    fi
}

# Called only after the actual build/import returned successfully and all its
# component gates passed. A registry receipt alone cannot establish that an
# interrupted job has terminated. The later stage completion retains the
# receipt atomically; a gap before that checkpoint still refuses publication.
release_image_publication_complete() {
    release_image_receipt "$1" || return $?
    release_state effect-complete "image-$1-publication"
}

release_verify_mmm() {
    local os arch metadata
    for os in linux darwin; do
        for arch in arm64 amd64; do
            metadata=$(go version -m "$WARP_HOME/mmm/mmm/build/$os/$arch/mmm") || return $?
            print -r -- "$metadata" | grep -F -- "$BUILD_HOME/warp" >/dev/null || return 65
            print -r -- "$metadata" | grep -F -- "main.Version=$WARP_VERSION" >/dev/null || return 65
        done
    done
}

# Never replace a tag or infer absence from an authentication/network failure.
# A previously observed publication is reusable only at the exact peeled commit.
release_git_tag() {
    local tag="$1" commit="$2" remote local_commit result
    remote=$(git ls-remote --exit-code --tags origin "refs/tags/$tag" "refs/tags/$tag^{}")
    result=$?
    if (( result == 0 )); then
        local peeled=$(print -r -- "$remote" | awk '$2 ~ /\^\{\}$/ {print $1}')
        [[ -n "$peeled" ]] || peeled=$(print -r -- "$remote" | awk 'NR == 1 {print $1}')
        if [[ "$peeled" != "$commit" ]]; then
            print -u2 -- "Published tag $tag points at a different commit; refusing to overwrite"
            return 65
        fi
        return 0
    elif (( result != 2 )); then
        print -u2 -- "Unable to read exact remote tag $tag"
        return "$result"
    fi
    if local_commit=$(git rev-parse --verify "refs/tags/$tag^{commit}" 2>/dev/null); then
        [[ "$local_commit" = "$commit" ]] || return 65
    else
        git tag -a "$tag" -m "$tag" "$commit" || return $?
    fi
    git push origin "refs/tags/$tag"
}

release_prepared_paths() {
    RELEASE_PREPARED_OUTPUTS=(
        "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/web-candidate.json"
        "$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/mmm-candidate.json"
        "$BUILD_HOME/sdk/js/build"
        "$BUILD_HOME/sdk/js/wasm"
        "$BUILD_HOME/sdk/js/release"
        "$BUILD_HOME/sdk/packaging/release/plan.json"
    )
    if [[ "$SDK_NPM_PUBLISH" = yes ]]; then
        RELEASE_PREPARED_OUTPUTS+=("$WARP_HOME/.release-checkpoints/$EXTERNAL_WARP_VERSION/npm")
    fi
}

# Registry adapters without a safe read reconciliation must never blindly
# replay an uncertain multi-registry publish. Completed stages retain checked
# packages. An interruption is explicit, actionable, and cannot overwrite.
release_sdk_publication_begin() {
    local observed
    observed=$(release_state effect-begin "$1") || return $?
    if [[ "$observed" = uncertain || "$observed" = complete ]]; then
        print -u2 -- "SDK publication $1 has an uncertain result. Inspect exact-version registry receipts before continuing; the harness will not re-publish potentially accepted artifacts."
        return 65
    fi
}
