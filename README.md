# build

This repo contains the build code used for production releases. The goal is to publish reproducible builds so that anyone can build and verify the code in production.

The `main` branch of all repos is the latest production-ready code.

The project uses warp versions, `yyyy.mm.dd-version_code`, so that versions are tied to dates and not release schedules. The project does not plan to have specific release schedules in the near future, so that we can iterate more fluidly on major features. We will maintain backwards compatibility as much as possible. Anytime there is a breaking change we will document it in the corresponding repo.

`metadata/en-US/changelogs` will be populated for each version code that is published in a store.


## Changelogs

Changelogs are generated from the commits, not written by hand. `all/changelog.py`
reads the submodule pins of two releases -- a release tag in this repo pins every
submodule at an exact commit -- walks the commit range that fell between them in
each component, and renders two different artifacts, because there are two
audiences with very different limits:

- **the store note**, `metadata/en-US/changelogs/<version code>.txt`. Capped at
  **500 characters**: F-Droid truncates at 500 silently (`fdroidserver`'s
  `char_limits['whatsNew']`) and Google Play rejects an over-length note outright.
  The generator packs whole bullets inside that budget, so it is never a store
  that does the cutting. It draws only on the components that ship inside the
  Android artifact, and holds back commits that touched only repository
  configuration, tests, docs or build scripts -- those did not change the app
  anyone installed.
- **the full changelog**, in the GitHub release body. Every commit in every
  submodule, grouped by component, with the first paragraph of each commit body
  and a link to each commit. Budgeted against GitHub's 125,000-character release
  body limit.

`all/run.sh` generates both during a release. Generation is required when it is
enabled; a failed generator or missing output fails the release. The file
`metadata/en-US/changelogs/pending.txt` remains the place for a human-written
headline for the next release -- it is placed above the generated bullets.

The same text is written under the base version code and under each offset in
`FDROID_VERSION_CODE_OFFSETS` (default `0 2 3`), plus `default.txt`. That is not
redundancy: fdroiddata's recipe declares `VercodeOperation: ['%c + 2', '%c + 3']`
for the ABI-split APKs and fdroidserver matches changelog files by exact
filename, so a base-code-only file means F-Droid shows no changelog at all.

Set `BUILD_URIO_CHANGELOG=0` to skip this release-body/store-note generator.
The ur.io website uses its committed changelog and desktop release metadata;
the build does not refresh either through the GitHub API. Refresh them with
the standalone `mmm/ur.io/react/scripts/generate-changelog.mjs` and
`generate-releases.mjs` maintenance scripts and commit their outputs separately.
The local `check-changelog-fresh` gate still requires the committed changelog
to cover the desktop downloads the site advertises. Content and page dates
are still generated locally and committed during the release. When generation
remains enabled, set `BUILD_RELEASE_BODY_CHANGELOG=` to omit only the full
changelog from the GitHub release body while retaining the generated store notes.

Release-body and store-note generation reads commit ranges and changed files
from local Git, with **zero GitHub API requests**. Missing component history is
listed under “Not walked”; candidates whose file lists are unavailable are kept.
The standalone ur.io website generators still query GitHub when run manually.
GitHub release creation, artifact upload and finalization remain enabled.
For a manual release-note run in an incomplete checkout, `--api-limit N`
explicitly permits at most N fallback
requests across all components, notes, and retries. A 403/429 stops further API
requests immediately. `all/run.sh` explicitly passes `--api-limit 0`.

Run it by hand for any pair of releases:

```
all/changelog.py --from v2026.8.21-1025339670 --to v2026.8.21-1025613560
all/changelog.py --help          # filters, budgets, audiences
all/changelog.py --self-test     # no network
```


## Build and deploy all

Builds run on our own hardware, through `all/run.sh`, which runs daily on the
build host:
- builds all clients and services from `main`
- runs local tests
- branches and tags repos for reproducible builds
- uploads clients to test tracks
- deploys services to production over 16 hours

If you want to join the test tracks to get regular test builds via TestFlight and Google Play, please send a request to <support@ur.io>.


## The sidecar repository's release

`all/run.sh` releases the sidecar repository after connect's and glog's tags and
before the SDK's first edit. One repository holds three things:

- the root module, moved into `v2026/` and tagged `v<version>`;
- `smtpproof`, a module of its own in `smtpproof/v2026/`, tagged
  `smtpproof/v<version>`, since Go leaves the `/v2026` directory out of a nested
  module's tag;
- `cli` and `mlsinterop`, which stay in place, require both tags and are never
  tagged.

The design documents stay beside the module, out of every consumer's module
cache. `.pb.go` files are committed and never regenerated on a version branch.
`make check` then runs from the moved module. The SDK's four modules and server
take the tags, server smtpproof's too, and SN drops both with server, reaching the
root through the SDK. In every repository smtpproof's path is rewritten before the
root's, because the root's pass skips only a line already holding `/vN`.

`TestSidecarReleaseFixture` runs this real code on copies, with each tag served
from a file proxy, on through the `make check` that follows the tags, then builds
consumers at those tags with no replaces. A failure only the moved module shows
would otherwise come after both tags exist and burn the version. The whole of
`make check` runs, so the fixture takes as long as it does. It needs this
checkout's real path, so run it without `-trimpath`:

```sh
cd all && go test -tags sidecar_release_fixture -timeout 60m -run Fixture .
```

`run.sh` runs it, and sidecar's `make size`, in the `BUILD_TEST` candidate gate,
before a version is allocated. Sidecar's `make check` holds its own idea of this
transformation to these same functions (`check/release_step_test.go`).

## The sidecar branches

Every repository the sidecar work touches has a `sidecar` branch (DESIGN.md §4.8).
`all/sidecar-branches.zsh` keeps them one commit set, over the monoroot of sibling
checkouts that holds this one (`SIDECAR_BRANCHES_ROOT` names another):

- `check` reports each branch, the pins and the commit-msg scan, and changes nothing.
- `repin` writes each sibling's `sidecar` commit into server's
  `local/source-graph/lock.yml` and this repository's gitlinks, and commits both, so
  server's `TestSidecarPinsEqualBuildSubmodules` holds. It refuses a sibling with
  uncommitted changes, one checked out off its `sidecar` branch, and a commit no
  branch holds, since a pin must stay reachable.
- `merge-in` merges `main` into `sidecar` in §4.8's order (connect, sidecar, sdk, sn,
  server, then warp, config, vault, xops, mmm and minio, whose `master` plays `main`,
  then build), stops at the first conflict and names the repository, and re-pins
  once every merge is in.
- `merge-out <gate> <commit-set>` merges the commit set a gate recorded (the
  "Commit set" table of server's `local/sidecar-gate` report, or `<repo> <commit>`
  lines) into each `main` in the same order, connect first, never the tip. It merges
  nothing until every commit passes `.githooks/commit-msg --scan main..<commit>` and
  sidecar's `TestX9ReadyForMain` passes at the gate over the recorded sidecar and
  connect.
- `scan` runs the commit-msg scan of `main..sidecar` in every repository.

It merges and never rebases, since a rebase orphans pinned commits and skips the
commit-msg hook, and it never pushes: each command that commits prints the pushes
for the owner. Until the owner's first re-pin, server's lock still holds main's pins
beside this repository's, so `TestSidecarPinsEqualBuildSubmodules` fails, as the
source graph's checkout tests fail on anything that moved since the last re-pin.
The tests run on fixture repositories and need this checkout's real path:

```sh
cd all && go test -run 'MergeIn|MergeOut|Repin|CheckReports|ScanReports|ScriptNeverPushes' .
```

## Reproducible builds

Builds use the semver `<version>-<version_code>`. Each semver has a corresponding tag in each repo, `v<version>-<version_code>`. You can check out the `build` repo at this tag to get all the repos at the correct tag also.

Because warp versions are `yyyy.mm.dd`, the go modules in the version branches are translated to the `vyyyy` major by appending `/vyyyy` to the end. This makes it so that any go module can be imported as `<module>/vyyyy`.

### ABI-specific versions codes

If the version code ends in non-zero, it means you have an architecture-specific build that was done to reduce download size. In this case, just change the last digit to zero to find the git tags and branch names. 

e.g.

```
614087741 # specific to arm64
614087740 # the version code to use for git tags, branches, etc.
``` 


## Ungoogle flavor

We maintain a flavor of each version that removes all google dependencies. This is the most compatible with global devices.

Use the tag `v<version>-<version_code>-ungoogle` of the `build` repo to get the ungoogle variant.


## Example build android client

If you downloaded the app from a store, go to Apps -> URnetwork to view the version and version code for the app. You can then build your own debug copy of the app with the following.

```
# check your apk for the version and version code
# git checkout v<version>-<version_code>
git checkout v2025.4.1-58770364
git submodule update --init
cd android/app
./gradlew assembleDebug
```


## Resource notes

On macOS, the docker engine must have at least 16GiB of memory allocated.

## DockerHub Images

Production images can be found at [DockerHub](https://hub.docker.com/u/bringyour). These are deployed with the [`warpctl` tool](https://github.com/urnetwork/warp).
