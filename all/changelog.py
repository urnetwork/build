#!/usr/bin/env python3
# =============================================================================
# changelog.py -- generate a real changelog for a release from the submodule pins.
#
# WHY THIS EXISTS
# Every published changelog this repo has ever shipped says the same thing.
# all/run.sh copies metadata/en-US/changelogs/pending.txt verbatim into
# metadata/en-US/changelogs/<version code>.txt on every release, pending.txt is a
# static one-liner ("- Bug and performance fixes."), and so 1025763520,
# 1025613560, 1025339670 and 1025223880 are byte-identical. That is not a
# changelog, it is a placeholder that nobody ever replaced.
#
# The material for a real one is already in this repository and always has been.
# A release tag here PINS EVERY SUBMODULE at an exact commit, so for any two
# releases the difference between the two trees is, per component, an exact
# commit range -- and therefore every commit and every commit message in it.
# This script reads those pins and turns the range into notes.
#
# TWO AUDIENCES, TWO ARTIFACTS -- this is the central design decision
# The store note and the full changelog cannot be the same text, because the
# store note is capped and the cap is small:
#   * F-Droid: fdroidserver common.py sets char_limits['whatsNew'] = 500, and
#     update.py's _set_localized_text_entry does `text = fp.read(limit * 2)` then
#     `text[:limit]`. That is a SILENT mid-sentence slice -- no warning, no error,
#     no log. Python str slicing, so the unit is Unicode code points, not bytes.
#     whatsNew is not in the ('name','summary','video') strip branch, so leading
#     whitespace and a BOM are counted too; this script emits neither.
#   * Google Play: "up to 500 Unicode characters per language". The Publisher API
#     rejects an over-length note outright rather than truncating. Not on this
#     repo's path today (run.sh:1603 is still `# FIXME android play release`, the
#     .aab goes to a GitHub release and a human uploads it), but the Console
#     enforces the same 500.
# So the store file gets a hard 500-character budget, packed at bullet
# boundaries, and truncation is never left to the store. Everything else -- every
# commit, grouped by component, with bodies and links -- goes to the GitHub
# release body, which has its own much larger cap (125,000 characters; the API
# answers 422 "body is too long" above it, and run.sh PATCHes that body under
# error_trap, so overrunning it would abort a release after every artifact has
# already been uploaded). --full-limit exists for exactly that reason.
#
# ONE NOTE PER STOREFRONT, AND ONE SHARED TAIL -- the second design decision
# There is no such thing as "the store note". Four storefronts serve four
# different artifacts, and not one of the three numbers that matter -- the
# limit, the format, the field -- is shared between them:
#
#   android   500 chars   plain text      metadata/en-US/changelogs/<code>.txt
#   apple    4000 chars   plain text      changelogs/<versionCode>_Apple.txt
#   windows  1500 chars   plain text      changelogs/<versionCode>_Windows.txt
#   linux     no limit    AppStream XML   changelogs/<versionCode>_Linux.xml
#
# A single 500-character plain-text note for all four would waste seven eighths
# of what App Store Connect accepts, and would be INVALID on Linux, where the
# field is not text at all but a markup fragment inside a metainfo XML file that
# a validator gates the build on. So each storefront gets its own audience, its
# own budget and its own renderer; see AUDIENCES below for the per-store
# reasoning and every citation.
#
# Each note is ITS OWN APP FIRST, then the parts every app is built from. The
# shared tail is appended after a boundary a human can find and delete in one
# motion, because the same sdk/connect commit is honest news on one store and
# noise on another and only the person publishing knows which. The boundary is
# NOT the same syntax everywhere, and that is not an inconsistency:
#   * plain text has no comment syntax. Whatever separator is written there is
#     PUBLISHED VERBATIM -- fdroidserver reads the changelog file raw and
#     slices it raw -- and it is charged against the limit. So the marker has to
#     be text that reads acceptably on the day somebody forgets to delete it.
#   * AppStream XML does have comment syntax, and it is genuinely free:
#     as_validator_check_description_tag() only inspects XML_ELEMENT_NODE
#     children, so a comment validates, and GNOME Software / KDE Discover parse
#     the XML and drop it. So Linux gets an explicit boxed marker for nothing.
#
# THE SHARED TAIL IS ALWAYS INSIDE THE LIMIT, on every store, and the developer
# deleting it is an option rather than an obligation. It has to be, because on
# android there is no developer in the loop at all: F-Droid pulls this repo's
# metadata tree on its own schedule and truncates at 500 in silence. Budgeting
# the tail outside the limit would be safe on the three stores a human edits and
# invisibly wrong on the one that nobody reviews. It costs the app tier nothing,
# because the app tier is packed FIRST and may take the whole budget -- the tail
# only ever fills what is left over, so a deleted tail never turns out to have
# displaced an app change.
#
# DEGRADE, NEVER BREAK
# run.sh's rule is that a failed step must not lose a release. So: a component
# that cannot be walked is reported inline as unwalkable and the rest of the
# changelog is still produced; only a failure to resolve the pins at all is
# fatal, and in that case NOTHING is written (both outputs are rendered in full
# before either file is opened) so the caller's fallback sees an untouched tree.
# run.sh calls this under warn_trap and falls back to the old pending.txt copy.
#
# DETERMINISM
# Same two refs in, same bytes out: components are emitted in a fixed order,
# commits in the order git itself reports them, and nothing here reads the clock
# or a random source. The one input that is not a constant is `--from`'s default
# ("the previous release"), which is resolved from the tag list at run time and
# then PRINTED IN THE OUTPUT, so any generated changelog names the exact pair
# that produced it and can be reproduced.
#
# NO DEPENDENCIES, ON PURPOSE
# stdlib only, python3 only. It has to run on the macOS build host (python3 comes
# with the Xcode command line tools, which that host has by definition) and on a
# GitHub runner, with no pip step in either place. GITHUB_API_KEY is used when
# run.sh exports it, but every repo walked here is public (all 17 submodules plus
# build; sn lives under urfoundation), so an anonymous run works too -- at the
# lower anonymous rate limit.
#
#   all/changelog.py --from v2026.8.21-1025339670 --to v2026.8.21-1025613560
#   all/changelog.py --to worktree --store-out store.txt --full-out full.md
#   all/changelog.py --to worktree --notes-dir changelogs
#   all/changelog.py --audience apple --to v2026.8.21-1025763520
#   all/changelog.py --self-test
# =============================================================================

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

BUILD_REPO = "urnetwork/build"
API = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")
WEB = "https://github.com"

# Order components by how visible they are to somebody reading release notes,
# not alphabetically: the apps first, then what the apps are built from, then the
# infrastructure behind them. A submodule that is not named here (one added
# later) still appears -- it sorts alphabetically after this list, so a new repo
# shows up in the changelog instead of silently vanishing from it.
COMPONENT_ORDER = [
    "android", "apple", "linux", "windows", "extension", "web",
    "build",
    "sdk", "connect", "warp", "sn",
    "server", "proxy", "userwireguard",
    "localizations", "docs", "glog", "goidenticons",
]

# Everything that is compiled INTO every one of the four apps and is not one of
# them. sdk is the Go client library each app links; connect is what sdk is built
# on; glog and goidenticons are compiled into sdk (its go.mod requires all three
# and `replace`s them at ../). warp is warpctl, the build tool -- it produces the
# artifact, it is not in it -- and server, proxy, sn and userwireguard run on the
# network's own machines, not on anybody's device.
#
# This list is IDENTICAL for all four storefronts, and that is a fact about the
# build rather than a simplification worth flagging in the notes: android is a
# `gomobile bind` of sdk, apple is ONE
# `gomobile bind -target ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64`
# of the same package (sdk build/Makefile, target build_apple), and linux and
# windows both build sdk/cgo -- all/build-linux.sh and all/build-windows.sh each
# stage exactly `sdk connect glog goidenticons` over the build root, because
# sdk/cgo/go.mod replaces all of them with local paths at once.
SHARED_COMPONENTS = ["sdk", "connect", "glog", "goidenticons"]

# THE STOREFRONT TABLE.
#
# "app" is what ships as THIS app and nothing else; "shared" is what ships inside
# it and inside the other three. The two are kept apart rather than concatenated
# because the whole point of the split is that a human publishing to one store
# can delete the second half -- see the header. Priority is app first, then
# shared, newest commit first inside each; if the budget only fits three bullets
# they are the three most recent, most user-facing ones.
#
# "limit" is the storefront's REAL limit, one per store, sourced individually:
#
#   android  500   fdroidserver common.py default_config sets
#                  char_limits['whatsNew'] = 500 and update.py's
#                  _set_localized_text_entry does `text = fp.read(limit * 2)`
#                  then `text[:limit]` -- a silent mid-word slice, no warning,
#                  no log, and F-Droid reads this repo directly (fdroiddata's
#                  com.bringyour.network.yml: `Repo: https://github.com/urnetwork/build`)
#                  so nobody is in the loop to catch it. Play's cap is the same
#                  number from a different source: "You can enter release notes
#                  using up to 500 Unicode characters per language"
#                  (support.google.com/googleplay/android-developer/answer/9859348),
#                  and androidpublisher rejects rather than truncates
#                  ("length 546, which is too long (max: 500)").
#   apple   4000   App Store Connect, What's New in This Version: "Limited to
#                  4000 characters" (developer.apple.com/help/app-store-connect/
#                  reference/app-information/platform-version-information/).
#                  Same field and same limit on the iOS and the macOS version --
#                  see the apple entry for why one note is honest for both.
#   windows 1500   Partner Center, "What's new in this version": "Character
#                  limit: 1,500 characters" (learn.microsoft.com/windows/apps/
#                  publish/publish-your-app/msi/add-and-edit-store-listing-info).
#                  The MSIX page states the same 1500, so this survives a move
#                  off MSI; this repo ships MSIs today (all/build-windows.sh).
#   linux   none   AppStream bounds a release description nowhere -- not in the
#                  spec and not in the validator; a 35,000-character description
#                  passes `appstreamcli validate --no-net --pedantic`. The number
#                  below is EDITORIAL, not a format limit: GNOME Software and
#                  the Flathub page render the newest release inline, and forty
#                  bullets there is not a release note, it is a commit log.
#
# "boundary" is what marks the start of the shared tail, and it is a per-format
# decision because the cost is per-format (header, and see render_store /
# render_appstream):
#   ""              a blank line and nothing else. This is android's, and it is
#                   the whole of what 500 characters can afford: the real note
#                   for a one-week span measures 485/500, so a prose heading
#                   would displace an actual bullet. One newline still gives the
#                   person pasting into the Play Console a one-motion delete
#                   target ("from the blank line to the end"), and it publishes
#                   to F-Droid as a harmless paragraph break.
#   "Under the hood"  a blank line plus a plain-prose heading, for the two stores
#                   whose budget is three to eight times android's. Deliberately
#                   not "--- SHARED ---" or "<!-- sdk -->": this text is user-
#                   visible App Store and Microsoft Store copy the moment
#                   somebody forgets to delete it, so it has to read as a
#                   release note rather than as a leaked machine marker.
#   None            do not mark a boundary at all (the "all" audience, which has
#                   no shared tier because it has no app tier either).
#
# "out" is the SUFFIX of the file --notes-dir writes, which lands there as
# "<prefix>_<suffix>" -- 1025763520_Apple.txt, 1025763520_Linux.xml. The prefix is
# the release's version code, so the directory is a per-release archive that sorts
# chronologically, the same convention metadata/en-US/changelogs/<versionCode>.txt
# already uses.
#
# android has a file here AND its fastlane copy: F-Droid reads
# metadata/en-US/changelogs/<versionCode>.txt unattended and that path cannot move,
# while the copy here is what a human opens when writing the Play note. run.sh
# writes the fastlane one; both come from this same render, so they cannot drift.
#
# The notes directory must stay OUT of metadata/. fdroidserver globs
# `build/[A-Za-z]*/metadata/[a-z][a-z]*` and then os.walk()s the result, matching
# changelog files by bare filename at any depth -- a directory under metadata/
# would fabricate a locale in the F-Droid index. A top-level changelogs/ is
# invisible to that glob.
AUDIENCES = {
    # F-Droid reads this file off this repository with no human in between, and
    # Google Play gets the same text pasted by hand (run.sh:1603 is still
    # `# FIXME android play release to play internal testing`). Both cap at 500.
    "android": {
        "app": ["android"],
        "shared": SHARED_COMPONENTS,
        "limit": 500,
        "format": "text",
        "boundary": "",
        # A file here AND the fastlane copy run.sh writes. The fastlane path is
        # what F-Droid reads unattended and cannot move; this one is the
        # per-release record a human opens when writing the Play note, and it
        # sits beside the other storefronts so nobody has to know that Android
        # keeps its real copy somewhere else.
        "out": "Android.txt",
        "where": "metadata/en-US/changelogs/<versionCode>.txt (F-Droid, unattended) "
                 "and Play Console -> release notes (pasted by hand)",
    },
    # ONE NOTE FOR TWO STORES, and it is honest for both. The brief's question
    # was whether the sdk slice differs between iOS and macOS: it does not.
    # urnetwork/sdk build/Makefile builds all four slices
    # (ios/arm64, iossimulator/arm64, macos/arm64, macos/amd64) in a SINGLE
    # `gomobile bind`, from one Go package, with no per-platform build tags, and
    # both platform archives come from the same apple pin and the same sdk pin.
    # So between any two release tags the apple/sdk/connect commit range is
    # byte-identical for the iOS and the macOS version, and the same 4000
    # characters are accepted in the same field on both.
    #
    # Named "apple", never "ios": the app target's SUPPORTED_PLATFORMS already
    # lists `xros xrsimulator`, so a third paste target is latent and would
    # otherwise rename this tree later.
    "apple": {
        "app": ["apple"],
        "shared": SHARED_COMPONENTS,
        "limit": 4000,
        "format": "text",
        "boundary": "Under the hood",
        # fastlane deliver's own shape (<metadata_path>/<locale>/release_notes.txt,
        # and en-US is a real App Store Connect locale), so if the manual paste is
        # ever replaced by `deliver -p ios` / `deliver -p osx` the file is already
        # where that tool looks. Nothing in urnetwork/apple uses fastlane today.
        "out": "Apple.txt",
        "where": "App Store Connect -> What's New in This Version (iOS and macOS)",
    },
    # Manual submission today: all/windows/README.md says "MSIs are uploaded to
    # the GitHub release; Store submission is manual", so this file exists to be
    # pasted. The Partner Center CSV column is WhatsNew and the MSI/EXE
    # submission API field is listings.whatsNew, if it is ever automated.
    "windows": {
        "app": ["windows"],
        "shared": SHARED_COMPONENTS,
        "limit": 1500,
        "format": "text",
        "boundary": "Under the hood",
        "out": "Windows.txt",
        "where": "Partner Center -> Store listings -> <language> -> "
                 "What's new in this version",
    },
    # NOT A STORE AND NOT PLAIN TEXT. Linux has no single storefront; what every
    # channel actually reads is the AppStream <release><description> in
    # urnetwork/linux app/packaging/com.bringyour.network.metainfo.xml.in, which
    # GNOME Software, KDE Discover and the Flathub page all render. That field is
    # restricted markup, a validator gates the build on it
    # (.github/workflows/build.yml: `appstreamcli validate --no-net --pedantic`,
    # where a WARNING is fatal), and so this audience gets its own renderer
    # rather than a different number.
    "linux": {
        "app": ["linux"],
        "shared": SHARED_COMPONENTS,
        "limit": 1800,
        # THE ONLY STOREFRONT THAT NEEDS THIS, and it needs it for a reason that
        # does not apply to the other three. Everywhere else the app tier may
        # take the entire budget, because there the budget belongs to the STORE:
        # spending android's last 40 characters on an app change instead of an
        # sdk change is the right trade, and the tail simply gets what is left.
        # Here the 1800 above is OURS -- AppStream imposes nothing -- so letting
        # a number this file invented be the thing that deletes the shared tail
        # would be inventing a constraint and then obeying it. urnetwork/linux
        # is the most actively developed of the four apps and produced 28
        # qualifying commits in one week, which is exactly enough to swallow any
        # round number, so the app tier is capped and the tail always has room.
        "app_limit": 1000,
        "format": "appstream",
        "boundary": "Under the hood",
        "out": "Linux.xml",
        "where": "urnetwork/linux app/packaging/com.bringyour.network.metainfo.xml.in "
                 "-> <releases><release><description>",
    },
    # Not a storefront: every component, in COMPONENT_ORDER, which is what
    # `--audience all` has always meant. No app/shared split, so no boundary --
    # there is nothing to delete when nothing was separated.
    "all": {
        "app": None,
        "shared": [],
        "limit": 500,
        "format": "text",
        "boundary": None,
        "out": "Simple.txt",
        "where": "no storefront -- the everything-included short view, for reading",
    },
}

# The version string all/run.sh stamps into its own commits: 2026.8.21-1025613560.
_V = r"\d{4}\.\d{1,2}\.\d{1,2}-\d+"

# ---------------------------------------------------------------------------
# NOISE FILTERS.
#
# Every one of these is a commit all/run.sh itself created as part of shipping a
# release. They are not news -- they are the build system's own footprints, and
# on a quiet release they outnumber the real commits ten to one. Each has a name,
# each is listed with its count in the rendered output, and --filters selects the
# set (--filters= keeps everything), so nothing is dropped silently.
#
# Deliberately NOT filtered: terse subjects ("fixes", "checkpoint", "wip: ..."),
# because those are real work by a person, and hiding them would misrepresent the
# release. They read badly; that is an argument for better commit messages, not
# for a generator that quietly edits history.
# ---------------------------------------------------------------------------
FILTERS = {
    # run.sh:745 `git commit -m "${EXTERNAL_WARP_VERSION}"` (git_commit, run into
    # every submodule), run.sh:1020 the same in the build repo, plus the three
    # variants it stamps at 523/538/553-574.
    "release": (
        re.compile(r"^v?" + _V + r"(?:-ungoogle)?"
                   r"(?: (?:ip security and blocker|ur\.io changelog|localizations) update)?$"),
        "version-stamp commit%s written by all/run.sh on every release",
    ),
    # run.sh:1516 `git commit -m "$HOST build ungoogle"`.
    "ungoogle": (
        re.compile(r"^\S+ build ungoogle$"),
        "ungoogle re-tag commit%s written by all/run.sh",
    ),
    # The localization sync, when it lands without a version prefix.
    "localization": (
        re.compile(r"^(?:\S+ )?localizations? (?:update|sync)$", re.I),
        "generated localization sync%s (localizations/keys/*.yaml is the source)",
    ),
    # Merge commits carry no content of their own; both sides are already in the
    # range. Matched on parent count, not on the subject -- see is_filtered().
    "merge": (None, "merge commit%s (both sides are already listed)"),
}
DEFAULT_FILTERS = "release,ungoogle,localization,merge"

# Store-note-only filters. These commits ARE in the full changelog; they are held
# out of the 500-character note because a store reader gets nothing from them.
#
# The first group is by commit-type prefix: `test(routing): ...`, `ci: ...`,
# `docs: ...`. The second is subjects that do not describe a change at all --
# measured on the real range v2026.7.22-999364020...v2026.8.21-1025763520, an
# unfiltered note came out as "Audit", "Fix version", "Gitignore update",
# "Performance checkpoint" and "Drop the fork's multipleIps UI rename from the
# checkpoint". Those are honest commits and they stay in the full changelog, but
# as one of the four or five bullets a phone user ever sees they are worse than
# nothing.
# wip and partial are in the FIRST group, the type-prefix one, deliberately.
# "Wip(egress): mark the daemon's sockets at creation via cgroup-BPF -- NOT
# MERGEABLE YET" and "Partial(canvas): settle dots when there is no clock -- NOT
# the missing-dots fix" are both real subjects from urnetwork/linux in
# v2026.8.15-1020621320...v2026.8.21-1025763520, and both would otherwise have
# been published on the Flathub page. The author labelled them as unfinished;
# taking them at their word is the whole rule. They stay in the full changelog.
STORE_SKIP = re.compile(
    r"^(?:test|tests|ci|chore|build|docs?|refactor|style|lint|deps|bump|release|"
    r"wip|partial)\b\s*(?:\([^)]*\))?\s*:"
    r"|^(?:wip|checkpoint|fixes|fix|update|updates|cleanup|tweaks?|misc|audit|"
    r"nit|nits|typo|typos|rebase|merge|revert|format|formatting|refactor)\b\s*(?:\([^)]*\))?\s*:?$"
    r"|\bcheckpoint\b"                       # "perfvar checkpoint", "reliability checkpoint"
    r"|^(?:\.?gitignore|readme|changelog|license|version|version bump|bump version)\b"
    r"|\b(?:gitignore|version) update$",
    re.I,
)

# A subject shorter than this cannot describe a change to somebody who did not
# write it ("Audit", "Fix version"). Store note only, and configurable.
STORE_MIN_SUBJECT = 16

# A conventional-commit type and scope is addressed to other developers. Inside a
# 500-character store note it is eight wasted characters and a word the reader
# does not know, so the store note keeps the sentence and drops the label:
# "Fix(android): don't capture apps through a tunnel with no live exit" ->
# "Don't capture apps through a tunnel with no live exit". The full changelog
# keeps subjects exactly as they were written.
STORE_TYPE_PREFIX = re.compile(r"^(?:fix|feat|feature|perf|improve|add|update|tune)\s*"
                               r"(?:\([^)]*\))?\s*:\s*", re.I)

# Paths that are not the app.
#
# The store note answers one question -- "what changed in the thing I installed?"
# -- with 500 characters. A commit that touched only CI config, tests, docs or a
# build script is real work, and it is in the full changelog, but it did not
# change the app on anybody's phone, and letting it take one of the three or four
# bullets that fit is how a changelog goes back to being noise. Measured on
# v2026.8.21-1025223880...v2026.8.21-1025763520, this is not hypothetical: the two
# highest-priority Android commits in that range are
# "Add a build-and-test workflow" and "Tighten the job timeouts to the observed
# run times", and both touch exactly one file, .github/workflows/build-and-test.yml.
#
# A commit is held back only when EVERY file it touched matches. Mixed commits
# (adeb7927 touches app/app/src/main/.../MainApplication.kt as well as
# app/scripts/*.mjs and a .md) count as app changes and stay.
#
# Turn it off with --no-store-path-check; the count held back is always reported.
STORE_NONSHIPPING_PATHS = re.compile(
    r"^\.github/"
    r"|^\.gitignore$|^\.gitmodules$|^LICENSE|^NOTICE"
    r"|(^|/)docs?/"
    r"|\.md$|\.txt$"
    r"|(^|/)(test|tests|androidTest|testing)/"
    r"|(^|/)[^/]*_test\.[a-z]+$|(^|/)[^/]*[Tt]ests?\.[a-z]+$"
    r"|(^|/)(scripts|packaging|build|ci)/"
    r"|\.sh$|(^|/)Makefile$|(^|/)Dockerfile"
    , re.I)

# Identity and tooling trailers. They are metadata about who typed the commit,
# not about what changed, and at ~120 characters a piece they would eat a real
# share of the release-body budget. Fixes:/Closes:/Refs: are NOT here -- those
# say something.
TRAILER = re.compile(
    r"^(?:Co-authored-by|Signed-off-by|Change-Id|Claude-Session|Reviewed-by|"
    r"Acked-by|Tested-by|Reported-by|Suggested-by|Cc)\s*:", re.I)

# The pending.txt texts that are placeholders rather than somebody's actual note.
# pending.txt keeps its job -- a human-written headline for the next release -- but
# the shipped default is not a headline, and echoing it above real notes would
# reproduce exactly the problem this script exists to fix.
PLACEHOLDER_LEDES = {
    "- bug and performance fixes.",
    "- bug and performance fixes",
    "- bug fixes and improvements.",
    "- bug fixes and improvements",
    "- bug and performance fixes.\n- bug fixes and improvements.",
}


_QUIET = False


def warn(msg):
    # --self-test drives the degradation ladder on purpose; its warnings are the
    # expected outcome there, not news, and printing them makes a passing CI job
    # look like a failing one.
    if not _QUIET:
        print("changelog: %s" % msg, file=sys.stderr)


# ---------------------------------------------------------------------------
# git and GitHub plumbing
# ---------------------------------------------------------------------------

def git(repo, *args):
    """Run git in `repo`. Returns stdout, or None if git failed or is absent.

    Never raises: every caller has an API fallback or a skip path, because this
    has to work in a shallow CI checkout as well as on the build host."""
    try:
        p = subprocess.run(("git", "-C", repo) + args,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError:
        return None
    if p.returncode != 0:
        return None
    return p.stdout.decode("utf-8", "replace")


def api_token():
    # run.sh already exports GITHUB_API_KEY for the mmm/ur.io changelog walk;
    # GITHUB_TOKEN is what Actions provides; GH_TOKEN is what the gh cli uses.
    # Any of them raises the rate limit. None of them is required.
    for k in ("GITHUB_API_KEY", "GITHUB_TOKEN", "GH_TOKEN"):
        v = os.environ.get(k)
        if v:
            return v
    return None


class ApiError(Exception):
    pass


def api_get(path, token, retries=3):
    """GET an API path and parse the JSON. Retries transient failures only.

    403/429 with a rate-limit body is retried with backoff; 404 and other 4xx are
    permanent and raise immediately, because retrying a wrong SHA just wastes the
    rate limit we are already short of."""
    url = path if path.startswith("http") else API + path
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "urnetwork-build-changelog",
    }
    if token:
        headers["Authorization"] = "Bearer " + token
    delay = 2.0
    for attempt in range(retries):
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                return json.loads(r.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")[:400]
            transient = e.code in (403, 429, 500, 502, 503, 504)
            if not transient or attempt == retries - 1:
                raise ApiError("HTTP %d %s: %s" % (e.code, url, body))
            warn("HTTP %d on %s, retrying in %.0fs" % (e.code, url, delay))
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as e:
            if attempt == retries - 1:
                raise ApiError("%s: %s" % (url, e))
            warn("%s on %s, retrying in %.0fs" % (type(e).__name__, url, delay))
        time.sleep(delay)
        delay *= 2
    raise ApiError(url)


# ---------------------------------------------------------------------------
# resolving a release ref to a set of submodule pins
# ---------------------------------------------------------------------------

_URL_RE = re.compile(r"(?:git@github\.com:|https://github\.com/)([^/]+/[^/\s]+?)(?:\.git)?$")


def parse_gitmodules(text):
    """.gitmodules -> {path: "org/repo"}, in file order."""
    out, path, url = {}, None, None
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("[submodule"):
            path, url = None, None
        elif line.startswith("path"):
            path = line.split("=", 1)[1].strip()
        elif line.startswith("url"):
            url = line.split("=", 1)[1].strip()
        if path and url:
            m = _URL_RE.match(url)
            if m:
                out[path] = m.group(1)
            path, url = None, None
    return out


def resolve_ref(repo, ref, token):
    """A release ref -> (label, {component: sha}, {component: "org/repo"}).

    `ref` is either a tag/commit in the build repo, or the literal "worktree",
    which reads the pins out of the current checkout. worktree mode is what the
    release path needs: at the point run.sh writes the changelog the new release
    is not committed or tagged yet, the submodules are simply sitting on their
    freshly pulled main HEADs -- which is exactly what the release will pin."""
    if ref == "worktree":
        gm = git(repo, "show", "HEAD:.gitmodules")
        if gm is None:
            gm = open(os.path.join(repo, ".gitmodules"), encoding="utf-8").read()
        repos = parse_gitmodules(gm)
        pins = {}
        head = git(repo, "rev-parse", "HEAD")
        if head:
            pins["build"] = head.strip()
        for path in repos:
            d = os.path.join(repo, path)
            # `git -C <dir> rev-parse HEAD` in an EMPTY submodule directory does
            # not fail -- it walks up and answers with the PARENT repo's HEAD, so
            # an uninitialised checkout would silently pin every component to the
            # build repo's own commit and then 404 on every compare. Confirm the
            # directory is its own repository before believing the answer.
            top = git(d, "rev-parse", "--show-toplevel")
            if top is None or os.path.realpath(top.strip()) != os.path.realpath(d):
                warn("submodule %s is not checked out; skipping it" % path)
                continue
            sha = git(d, "rev-parse", "HEAD")
            if sha is None:
                warn("submodule %s has no HEAD; skipping it" % path)
                continue
            pins[path] = sha.strip()
        repos["build"] = BUILD_REPO
        label = git(repo, "describe", "--tags", "--always", "HEAD")
        return ("worktree (%s)" % label.strip() if label else "worktree"), pins, repos

    # A tag or commit. Prefer the local clone; fall back to the API so this also
    # works from a shallow checkout that has no tags.
    pins, repos = {}, {}
    tree = git(repo, "ls-tree", ref)
    gm = git(repo, "show", "%s:.gitmodules" % ref)
    if tree is not None and gm is not None:
        for line in tree.splitlines():
            meta, _, path = line.partition("\t")
            parts = meta.split()
            if len(parts) == 3 and parts[1] == "commit":
                pins[path] = parts[2]
        repos = parse_gitmodules(gm)
        sha = git(repo, "rev-parse", "%s^{commit}" % ref)
        if sha:
            pins["build"] = sha.strip()
    else:
        warn("%s is not in the local clone; reading it from the API" % ref)
        t = api_get("/repos/%s/git/trees/%s" % (BUILD_REPO, urllib.parse.quote(ref)), token)
        for e in t.get("tree", []):
            if e.get("type") == "commit":
                pins[e["path"]] = e["sha"]
            elif e.get("path") == ".gitmodules":
                blob = api_get("/repos/%s/git/blobs/%s" % (BUILD_REPO, e["sha"]), token)
                import base64
                repos = parse_gitmodules(
                    base64.b64decode(blob["content"]).decode("utf-8", "replace"))
        c = api_get("/repos/%s/commits/%s" % (BUILD_REPO, urllib.parse.quote(ref)), token)
        pins["build"] = c["sha"]
    repos["build"] = BUILD_REPO
    return ref, pins, repos


def previous_release_tag(repo, to_ref):
    """The newest base release tag that is not `to_ref`.

    Base tags end in 0; the +2/+3 tags are the same release re-tagged for the
    ABI-split APKs (README.md: "just change the last digit to zero to find the
    git tags"), so comparing against one of those would produce an empty
    changelog. Sorted by creatordate, which is what `git tag --sort` gives and
    what the release order actually is."""
    out = git(repo, "for-each-ref", "--sort=-creatordate", "--format=%(refname:short)",
              "refs/tags/v*")
    if not out:
        return None
    pat = re.compile(r"^v" + _V + r"$")
    for t in out.splitlines():
        t = t.strip()
        if not pat.match(t) or not t.endswith("0"):
            continue
        if t == to_ref:
            continue
        return t
    return None


# ---------------------------------------------------------------------------
# walking one component's commit range
# ---------------------------------------------------------------------------

def compare(slug, base, head, token):
    """Every commit in base..head, oldest first, as GitHub reports them.

    PAGINATION IS NOT OPTIONAL. /compare caps its `commits` array at 250 (and at
    `per_page` below that) while `total_commits` reports the truth, and it does so
    with no error -- a connect range of 700 commits silently returns 250. Old
    release pairs are exactly the long ones, so a generator that must work for
    "any pair" has to page until the array comes back empty.

    "diverged" is the NORMAL status here, not a failure: each release's own
    version-stamp commit is orphaned by the next cycle's rebase, so 12 of 13
    moved submodules report diverged with behind_by 1 on a typical adjacent pair.
    Only the `commits` array (the ahead side) is read, so that is harmless -- but
    treating diverged as an error would skip nearly every component."""
    commits, page = [], 1
    while True:
        d = api_get("/repos/%s/compare/%s...%s?per_page=100&page=%d"
                    % (slug, base, head, page), token)
        got = d.get("commits") or []
        commits.extend(got)
        # `total_commits` is the honest count; stop when we have it, or when a
        # page comes back short/empty.
        if not got or len(commits) >= d.get("total_commits", len(commits)) or len(got) < 100:
            break
        page += 1
        if page > 100:                       # 10,000 commits: something is wrong
            warn("%s: stopping after 100 pages" % slug)
            break
    return commits


# One process now renders up to four storefront notes from the same sections,
# and the highest-priority commits are the SAME commits in all four (the shared
# sdk/connect tail is identical by construction -- see SHARED_COMPONENTS). Paying
# the API four times for one commit's file list would quadruple the request count
# for nothing and, anonymously, would burn the 60/hour allowance four times as
# fast. Keyed by (slug, sha), which is immutable, so the cache cannot go stale
# inside a run and cannot change the output: same input, same answer.
_FILES_CACHE = {}


def commit_files(slug, sha, token):
    """The paths one commit touched.

    /compare returns a `files` array for the WHOLE range, never per commit, so
    this is a separate request per candidate -- which is why only the handful of
    commits that could actually fit in the store note are ever looked up
    (--store-candidates), and why the full changelog never uses this."""
    key = (slug, sha)
    if key in _FILES_CACHE:
        return _FILES_CACHE[key]
    d = api_get("/repos/%s/commits/%s" % (slug, sha), token)
    files = [f["filename"] for f in (d.get("files") or [])]
    _FILES_CACHE[key] = files
    return files


def subject_of(message):
    return message.split("\n", 1)[0].strip()


def body_of(message):
    """The commit body with identity/tooling trailers removed."""
    _, _, rest = message.partition("\n")
    lines = [l for l in rest.splitlines() if not TRAILER.match(l.strip())]
    return "\n".join(lines).strip()


def is_filtered(commit, active):
    """-> the name of the filter that matched, or None."""
    if "merge" in active and len(commit.get("parents") or []) > 1:
        return "merge"
    subject = subject_of(commit["commit"]["message"])
    for name in active:
        rx = FILTERS.get(name, (None, ""))[0]
        if rx is not None and rx.match(subject):
            return name
    return None


# ---------------------------------------------------------------------------
# rendering
# ---------------------------------------------------------------------------

def order_components(names):
    rank = {n: i for i, n in enumerate(COMPONENT_ORDER)}
    return sorted(names, key=lambda n: (rank.get(n, len(rank)), n))


def clip(text, limit):
    """Trim to `limit` characters at a word boundary, marking the cut.

    Deterministic, and it never leaves a half-word: the alternative is what
    F-Droid does to us today."""
    if len(text) <= limit:
        return text
    cut = text[:limit - 4]
    sp = cut.rfind(" ")
    if sp > limit // 2:
        cut = cut[:sp]
    return cut.rstrip(" ,;:-") + " ..."


def first_paragraph(body, limit):
    """The first paragraph of a commit body.

    Chosen after reading this org's actual commits: the informative ones open
    with a paragraph that states what changed and why ("This repo had no CI at
    all. Every push and pull request against main now builds the app and runs its
    JVM unit tests.") and then spend fifteen more paragraphs on implementation
    detail that belongs in the commit, not in release notes. First paragraph is
    where the signal is."""
    if not body:
        return ""
    para = body.split("\n\n", 1)[0]
    para = " ".join(para.split())
    return clip(para, limit)


def render_full(meta, sections, filtered_counts, unwalkable, body_mode, body_limit,
                per_component_cap=None):
    out = []
    out.append("## Changes in %s" % meta["to_label"])
    out.append("")
    total = sum(len(s["commits"]) for s in sections)
    out.append("`%s` ... `%s` -- %d commit%s across %d component%s."
               % (meta["from_label"], meta["to_label"],
                  total, "" if total == 1 else "s",
                  len(sections), "" if len(sections) == 1 else "s"))
    out.append("")
    if not sections:
        out.append("_No component changed between these two releases._")
        out.append("")
    for s in sections:
        shown = s["commits"]
        hidden = 0
        if per_component_cap is not None and len(shown) > per_component_cap:
            hidden = len(shown) - per_component_cap
            shown = shown[:per_component_cap]
        out.append("### %s" % s["name"])
        out.append("")
        out.append("%s [`%s...%s`](%s/%s/compare/%s...%s)"
                   % (s["slug"], s["base"][:9], s["head"][:9],
                      WEB, s["slug"], s["base"], s["head"]))
        out.append("")
        for c in shown:
            sha = c["sha"]
            subject = subject_of(c["commit"]["message"])
            out.append("- **%s** ([`%s`](%s/%s/commit/%s))"
                       % (md_escape(subject, subject=True), sha[:9], WEB, s["slug"], sha))
            if body_mode != "none":
                body = body_of(c["commit"]["message"])
                para = (first_paragraph(body, body_limit) if body_mode == "first-paragraph"
                        else " ".join(body.split()))
                if para:
                    out.append("  %s" % md_escape(para))
        if hidden:
            out.append("- _... and %d more commit%s -- see the compare link above._"
                       % (hidden, "" if hidden == 1 else "s"))
        out.append("")
    if unwalkable:
        out.append("### Not walked")
        out.append("")
        # Honest about the hole rather than quietly short. A component that could
        # not be reached is named here with the reason and its compare link, so a
        # reader can go and look; the release still shipped.
        for name, slug, base, head, why in unwalkable:
            out.append("- **%s** (%s): %s -- [compare](%s/%s/compare/%s...%s)"
                       % (name, slug, why, WEB, slug, base, head))
        out.append("")
    if filtered_counts:
        parts = ", ".join("%d %s" % (n, FILTERS[k][1] % ("" if n == 1 else "s"))
                          for k, n in sorted(filtered_counts.items()))
        out.append("---")
        out.append("")
        # The hint has to be a command somebody can actually run, which is why
        # it quotes the LABELS and not the raw refs: the release path passes
        # `--to worktree --to-label v<version>`, and re-running "--to worktree"
        # later would compare against a moved checkout, while the tag it names is
        # created moments after this file is written and is reproducible forever.
        out.append("_Filtered out of this changelog: %s. Re-run "
                   "`all/changelog.py --from %s --to %s --filters=` to include them._"
                   % (parts, meta["from_label"], meta["to_label"]))
        out.append("")
    return "\n".join(out).rstrip() + "\n"


# Escaping is deliberately split, because the two strings are not the same kind
# of text.
#
# A BODY is prose the author wrote in markdown on purpose -- this org's commit
# messages use backticks for code and paths constantly -- so escaping them would
# make the release body worse than the commit it came from. The one thing that
# must still be escaped there is `<`/`>`, because GFM passes anything shaped like
# a tag through as raw HTML and `urnetwork-daemon-<version>.pkg.tar.zst` would
# lose "<version>" silently, and `\`, because a stray one escapes what follows.
#
# A SUBJECT is rendered inside `**...**` next to a `[..](..)` link, so a bare `*`
# or `[` there breaks the line's structure rather than just looking odd.
_MD_BODY = re.compile(r"([\\<>])")
_MD_SUBJECT = re.compile(r"([\\<>*\[\]])")


def md_escape(s, subject=False):
    return (_MD_SUBJECT if subject else _MD_BODY).sub(r"\\\1", s)


def store_bullets(sections, app_names, shared_names=(), token=None, path_check=False,
                  limit=500, max_candidates=40, min_subject=STORE_MIN_SUBJECT):
    """The lines the store note may draw from, in priority order.

    Priority is the app components first and then the shared ones, newest commit
    first inside each component, so if the budget only fits three bullets they
    are the three most recent, most user-facing ones.

    Returns (lines, shared_from, held_back). `shared_from` is the index in
    `lines` at which the shared tier starts -- everything before it is this
    app's own work, everything from it on is the sdk/connect tail a publisher
    may want to delete. It is len(lines) when there is no tail at all, which is
    what the renderers treat as "nothing to mark".

    held_back maps a reason to the number of commits it held out of the note.
    Every one of them is still in the full changelog, and the reasons are
    printed -- nothing here drops silently."""
    by_name = {s["name"]: s for s in sections}
    app = list(app_names) if app_names is not None else [s["name"] for s in sections]
    # A component cannot be in both tiers. If an audience ever names one in both
    # (say a future all-in-one artifact), the app tier wins: that is the tier the
    # reader of THAT store cares about, and duplicating a bullet under a
    # "shared" heading would be worse than not marking it at all.
    shared = [n for n in (shared_names or ()) if n not in app]
    seen, lines, looked_up = set(), [], 0
    held_back = {}
    shared_from = None
    budget = 0
    for name in app + shared:
        if shared_from is None and name in shared:
            # Recorded when the tier CHANGES, not when the first shared bullet
            # is appended, so a shared component that contributes nothing still
            # ends the app tier at the right index.
            shared_from = len(lines)
        s = by_name.get(name)
        if not s:
            continue
        for c in s["commits"]:
            subject = subject_of(c["commit"]["message"])
            if STORE_SKIP.search(subject):
                held_back["nothing a store reader can use in the subject"] = \
                    held_back.get("nothing a store reader can use in the subject", 0) + 1
                continue
            text = polish(subject)
            if len(text) < min_subject:
                held_back["a subject too short to describe the change"] = \
                    held_back.get("a subject too short to describe the change", 0) + 1
                continue
            key = text.lower()
            if key in seen:
                continue
            if path_check:
                # The budget is on API REQUESTS, so a commit whose files another
                # storefront already looked up is free and does not spend it.
                # It matters: the shared sdk/connect tier is the same commits in
                # all four notes, so without this the first note would spend the
                # whole allowance and the later ones would silently stop
                # filtering -- which is how a test-only commit ended up in the
                # Linux tail while apple and windows correctly held it back.
                files_key = (s["slug"], c["sha"])
                files = _FILES_CACHE.get(files_key)
                if files is None and looked_up < max_candidates:
                    looked_up += 1
                    try:
                        files = commit_files(s["slug"], c["sha"], token)
                    except ApiError as e:
                        # Fail OPEN. A missed lookup must not delete somebody's
                        # work from the release notes; at worst the note is less
                        # relevant.
                        warn("%s %s: %s (keeping it)" % (s["slug"], c["sha"][:9], e))
                        files = []
                if files and all(STORE_NONSHIPPING_PATHS.search(f) for f in files):
                    held_back["CI, test, docs or build files only"] = \
                        held_back.get("CI, test, docs or build files only", 0) + 1
                    continue
            seen.add(key)
            lines.append(text)
            # Gather comfortably more than the budget so render_store() can skip
            # an over-long subject and still have a shorter one behind it, then
            # stop -- there is no point paying for lookups that cannot fit.
            budget += len(text) + 3
            if budget > limit * 2:
                # The gather budget is shared across both tiers on purpose. If
                # the app tier alone overflows twice the note's budget, no shared
                # bullet could have fitted underneath it anyway, and stopping
                # here also stops paying for file-list lookups that cannot land.
                return lines, shared_from if shared_from is not None else len(lines), held_back
    return lines, shared_from if shared_from is not None else len(lines), held_back


def polish(subject):
    """Commit subject -> a line a store reader can read."""
    s = " ".join(subject.split())
    s = re.sub(r"^\[[^\]]*\]\s*", "", s)          # "[android] foo"
    s = STORE_TYPE_PREFIX.sub("", s)
    s = s.rstrip(" .")
    if s and s[0].islower():
        s = s[0].upper() + s[1:]
    return s


def boundary_lines(boundary):
    """The literal lines a PLAIN-TEXT note spends on the app/shared boundary.

    "" is a blank line and nothing else -- android's, because 500 characters
    cannot afford a heading (see AUDIENCES). Any other string is a blank line
    plus that string as a heading. None is "do not mark one".

    Every one of these lines is PUBLISHED. There is no comment syntax in a
    store note: fdroidserver reads the changelog file raw and slices it raw, and
    App Store Connect and Partner Center show the field verbatim. So this is
    text a reader is allowed to see, not a machine marker."""
    if boundary is None:
        return []
    return [""] if boundary == "" else ["", boundary]


def pack_bullets(lines, limit, lede=None, shared_from=None, boundary=None, prefix="- ",
                 app_limit=None):
    """Pack bullets into `limit` characters, cutting only at bullet boundaries.

    Returns (lede_kept, app_kept, shared_kept, boundary_kept): the raw texts, in
    order, that fit -- the caller adds whatever markup its format needs. The
    split is returned rather than a joined string because the same packing
    serves plain text and AppStream XML, which mark the boundary differently.

    The budget is `limit` characters of CONTENT, excluding the single trailing
    newline: F-Droid slices [:500] on a read that includes the newline, so a
    500-character body plus "\\n" survives intact -- reserving a character for the
    newline would throw away a character of real changelog for nothing.

    Characters, not bytes. Every changelog file in this repo today is ASCII so
    `wc -c` happens to agree, but subjects pulled from 17 submodules carry em
    dashes and non-ASCII names, and every store counts code points.

    THE APP TIER IS PACKED FIRST AND MAY TAKE THE WHOLE BUDGET. The shared tail
    fills what is left, so appending it never displaces an app change -- which is
    what makes "the tail counts against the limit" cost nothing. `app_limit`
    caps the app tier below `limit` and so reserves the difference for the tail;
    it exists for the one storefront whose limit this file invented rather than
    read off a store (see AUDIENCES["linux"]), and defaults to no cap at all.

    THE BOUNDARY IS CHARGED before the first shared bullet that fits, and only
    when something was emitted above it: a heading with nothing over it separates
    nothing, and would be two lines of a small budget spent on punctuation."""
    lede_out, app_out, shared_out = [], [], []
    used, emitted = 0, 0

    def cost(items):
        # Joined with "\n": each new item costs its own length, plus one
        # newline -- except the very first item in the note, which has nothing
        # in front of it to separate from.
        return sum(len(i) for i in items) + (len(items) if emitted else len(items) - 1)

    if lede:
        for l in lede.strip().splitlines():
            l = l.rstrip()
            if not l:
                continue
            if used + cost([l]) > limit:
                break
            used += cost([l])
            emitted += 1
            lede_out.append(l)

    n_app = len(lines) if shared_from is None else shared_from
    marker = boundary_lines(boundary) if shared_from is not None else []
    if app_limit is None:
        app_limit = limit
    charged = False
    for i, text in enumerate(lines):
        is_shared = i >= n_app
        pre = marker if (is_shared and not charged and (app_out or lede_out)) else []
        items = pre + [prefix + text]
        if used + cost(items) > (limit if is_shared else app_limit):
            # Skip this bullet and try the next; a shorter one may still fit.
            # Never truncate mid-subject -- a half-sentence is what we are here to
            # stop the stores from producing.
            continue
        used += cost(items)
        emitted += len(items)
        if pre:
            charged = True
        (shared_out if is_shared else app_out).append(text)
    return lede_out, app_out, shared_out, (marker if charged else [])


def render_store(lines, limit, lede, fallback, shared_from=None, boundary=None,
                 app_limit=None):
    """A plain-text store note: android, apple, windows.

    With shared_from and boundary left at None this is byte-for-byte what this
    function produced before the per-storefront split existed, which is what
    keeps --audience android and --audience all unchanged and what
    --no-store-boundary reproduces on demand."""
    lede_out, app, shared, marker = pack_bullets(lines, limit, lede, shared_from, boundary,
                                                app_limit=app_limit)
    out = list(lede_out) + ["- " + t for t in app] + list(marker) + ["- " + t for t in shared]
    if not out:
        # Never emit an empty store file. An empty <versionCode>.txt would make
        # F-Droid show a blank "What's New" -- strictly worse than the placeholder
        # it replaces.
        return (fallback or "- Bug and performance fixes.").strip() + "\n"
    return "\n".join(out) + "\n"


# ---------------------------------------------------------------------------
# AppStream: the Linux release description
#
# This is not a store note with a different limit, it is a different LANGUAGE,
# and a validator gates urnetwork/linux's build on it
# (.github/workflows/build.yml: `appstreamcli validate --no-net --pedantic`,
# where a WARNING is fatal, and app/meson.build runs the same check as a test).
# Every rule below was checked against appstreamcli 1.1.3 rather than inferred:
#
#   * ONLY <p>, <ul>/<ol> and <li> are legal here. <b>, a nested <ul>, or raw
#     text sitting directly under <description> are all failures
#     (description-markup-invalid / description-para-markup-invalid /
#     description-spurious-text). That is why the bullets are wrapped rather
#     than emitted as "- " lines: "- " text under <description> does not merely
#     look wrong, it fails the build.
#   * & < > MUST be escaped. A raw "&" is a hard XML parse error
#     (xml-markup-invalid, xmlParseEntityRef: no name), and commit subjects in
#     this org contain them.
#   * NO literal http:// https:// ftp:// anywhere in the description
#     (description-has-plaintext-url, a WARNING, therefore fatal). Commit
#     subjects do carry URLs, so they are stripped -- and a bullet that is
#     NOTHING BUT a URL then has to be dropped entirely, because an empty <li>
#     is tag-empty, also a WARNING, also fatal. Stripping without dropping would
#     turn one fatal error into a different one.
#   * XML COMMENTS ARE LEGAL and free: the validator only inspects element
#     children of <description>, and GNOME Software / KDE Discover parse the XML
#     and drop comments. This is the one format where the app/shared boundary
#     costs nothing and is invisible to users, so it gets a real boxed marker.
#   * The fragment is the CHILDREN of <description>, not a whole <release> and
#     not a whole document. urnetwork/linux's metainfo template already
#     configure_file()s @APPSTREAM_VERSION@ and @APPSTREAM_DATE@ into a single
#     <release>; a third placeholder fed from this file is the whole
#     integration. Standalone it is therefore not a well-formed document (no
#     single root), which is deliberate -- it is meant to be substituted, not
#     parsed on its own.
# ---------------------------------------------------------------------------

# Children of <description> sit at 8 spaces in
# app/packaging/com.bringyour.network.metainfo.xml.in (component 0, releases 2,
# release 4, description 6). Emitting them already indented means the
# substitution needs no re-indent step and the result still reads as XML.
APPSTREAM_INDENT = 8

# No "--" anywhere inside: that sequence is illegal in an XML comment and would
# turn the boundary marker into a parse error.
APPSTREAM_SHARED_OPEN = ("<!-- Shared with the other URnetwork apps. To publish a note about "
                         "the Linux app alone, delete from here to the end comment below. -->")
APPSTREAM_SHARED_CLOSE = "<!-- end of the shared section -->"

_APPSTREAM_URL = re.compile(r"\S*(?:https?|ftp)://\S*")


def appstream_text(text):
    """One bullet -> text that is legal inside <li> / <p>, or "" to drop it.

    URL first, escape second: stripping after escaping would leave "&amp;" in a
    URL's query string behind as debris."""
    t = " ".join(_APPSTREAM_URL.sub(" ", text).split())
    t = t.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    return t.strip()


def render_appstream(lines, limit, lede, fallback, shared_from=None, boundary=None,
                     app_limit=None, indent=APPSTREAM_INDENT):
    """The <p>/<ul> children of <release><description> for urnetwork/linux."""
    # Clean and drop BEFORE packing, so a bullet that was only a URL never gets
    # charged against the budget and never reaches the renderer as an empty <li>.
    n_app = len(lines) if shared_from is None else shared_from
    kept, kept_app = [], 0
    for i, text in enumerate(lines):
        t = appstream_text(text)
        if not t:
            continue
        if i < n_app:
            kept_app += 1
        kept.append(t)
    lede_out, app, shared, _ = pack_bullets(
        kept, limit, appstream_text(lede) if lede else None,
        None if shared_from is None else kept_app,
        # The XML boundary is charged nothing: AppStream has no length limit at
        # all, so `limit` here is an editorial budget on the bullets a store page
        # can usefully show, and the marker is not one of them.
        boundary=None, prefix="", app_limit=app_limit)

    pad = " " * indent
    out = []
    for l in lede_out:
        out.append("%s<p>%s</p>" % (pad, l))
    if app:
        out.append("%s<ul>" % pad)
        out.extend("%s  <li>%s</li>" % (pad, t) for t in app)
        out.append("%s</ul>" % pad)
    if shared:
        # Mark the boundary only when there is something above it to separate
        # from, and only when a boundary was asked for at all
        # (--no-store-boundary turns it off everywhere, in every format).
        marked = bool(out) and boundary is not None
        if marked:
            out.append("%s%s" % (pad, APPSTREAM_SHARED_OPEN))
            if boundary:
                # A heading as well as the comment: the comment is for the
                # person deleting the tail, the <p> is for the person reading
                # the store page, and here -- unlike in a 500-character plain
                # text note -- both are affordable.
                out.append("%s<p>%s</p>" % (pad, appstream_text(boundary)))
        out.append("%s<ul>" % pad)
        out.extend("%s  <li>%s</li>" % (pad, t) for t in shared)
        out.append("%s</ul>" % pad)
        if marked:
            out.append("%s%s" % (pad, APPSTREAM_SHARED_CLOSE))
    if not out:
        # <description> may not be empty -- a description with no valid element
        # child is description-no-valid-content, and a WARNING is fatal here.
        # The fallback is written as a paragraph rather than a one-item list:
        # "- " is the plain-text stores' bullet syntax and means nothing in XML.
        for l in (fallback or "- Bug and performance fixes.").strip().splitlines():
            l = appstream_text(l.lstrip("-* ").strip())
            if l:
                out.append("%s<p>%s</p>" % (pad, l))
    return "\n".join(out) + "\n"


# The tags AppStream permits inside a <description>. ol and em and code are not
# emitted by this generator, but they ARE legal, so a hand-edited fragment that
# uses them must not be reported as broken.
APPSTREAM_ALLOWED = {"p", "ul", "ol", "li", "em", "code"}
APPSTREAM_BLOCK = {"p", "ul", "ol"}


def appstream_problems(fragment):
    """Every rule `appstreamcli validate` would fail the Linux build on, checked
    offline and with no dependency on appstreamcli being installed.

    This exists because the failure is REMOTE and LATE: the fragment is consumed
    by urnetwork/linux, whose CI runs `appstreamcli validate --no-net --pedantic`
    with warnings treated as failures, and whose meson build runs the same check
    as a test. A malformed fragment would not break the release that generated
    it -- it would break the next Linux build, in another repository, for
    somebody who did not write it. Checking here turns that into a warning at
    the point of generation.

    Returns a list of human-readable problems; empty means it would validate.
    Comments are deliberately not checked for anything: the validator only
    inspects element children, which is exactly why the boundary marker is free
    in this format."""
    problems = []
    try:
        root = ET.fromstring("<description>\n" + fragment + "\n</description>")
    except ET.ParseError as e:
        # An unescaped & lands here: "xmlParseEntityRef: no name" in
        # appstreamcli, a hard ERROR rather than a warning.
        return ["not well-formed XML: %s" % e]

    def walk(node, parent):
        tag = node.tag
        if tag not in APPSTREAM_ALLOWED:
            problems.append("<%s> is not allowed in a description" % tag)
        elif parent == "description" and tag not in APPSTREAM_BLOCK:
            problems.append("<%s> may not be a direct child of <description>" % tag)
        elif tag == "li" and parent not in ("ul", "ol"):
            problems.append("<li> outside a <ul>/<ol>")
        elif tag in ("ul", "ol") and parent in ("ul", "ol", "li"):
            problems.append("nested <%s> (description-markup-nesting-too-deep)" % tag)
        text = "".join(node.itertext())
        if tag in ("p", "li") and not text.strip():
            # tag-empty is a WARNING, and a WARNING fails the build.
            problems.append("empty <%s> (tag-empty)" % tag)
        for child in node:
            walk(child, tag)

    for child in root:
        walk(child, "description")
    # Raw text directly under <description> is description-spurious-text; a "- "
    # bullet list pasted in unwrapped is exactly what that catches.
    stray = (root.text or "").strip() + "".join((c.tail or "").strip() for c in root)
    if stray:
        problems.append("text outside an element: %r (description-spurious-text)"
                        % stray[:60])
    whole = "".join(root.itertext())
    for scheme in ("http://", "https://", "ftp://"):
        if scheme in whole:
            problems.append("literal %s in the text (description-has-plaintext-url)"
                            % scheme)
    if not [c for c in root if c.tag in APPSTREAM_BLOCK]:
        problems.append("no <p>/<ul>/<ol> content (description-no-valid-content)")
    return problems


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def build(args, token):
    repo = args.repo
    to_label, to_pins, to_repos = resolve_ref(repo, args.to, token)
    from_ref = args.from_ref or previous_release_tag(repo, args.to)
    if not from_ref:
        raise SystemExit("changelog: could not work out a previous release tag; "
                         "pass --from explicitly")
    from_label, from_pins, _ = resolve_ref(repo, from_ref, token)

    names = order_components(set(to_pins) | {"build"})
    sections, unwalkable, filtered_counts = [], [], {}
    active = [f for f in args.filters.split(",") if f]
    for f in active:
        if f not in FILTERS:
            raise SystemExit("changelog: unknown filter %r (known: %s)"
                             % (f, ",".join(sorted(FILTERS))))

    for name in names:
        head = to_pins.get(name)
        base = from_pins.get(name)
        slug = to_repos.get(name)
        if not head or not slug:
            continue
        if not base:
            # A submodule added since the `from` release: there is no range to
            # walk, so say so rather than dumping its entire history.
            unwalkable.append((name, slug, head, head, "new since %s" % from_label))
            continue
        if base == head:
            continue
        try:
            commits = compare(slug, base, head, token)
        except ApiError as e:
            warn("%s: %s" % (name, e))
            unwalkable.append((name, slug, base, head, "could not be walked (%s)" % e))
            continue
        kept = []
        for c in commits:
            f = is_filtered(c, active)
            if f:
                filtered_counts[f] = filtered_counts.get(f, 0) + 1
                continue
            kept.append(c)
        if not kept:
            continue
        # Newest first: release notes are read top-down for "what changed", and
        # the newest work is what the reader is looking for. The API hands them
        # back oldest-first, so this reverse is the only ordering decision here.
        kept.reverse()
        sections.append({"name": name, "slug": slug, "base": base,
                         "head": head, "commits": kept})

    meta = {"from_label": from_label, "to_label": args.to_label or to_label}
    full = fit_full(meta, sections, filtered_counts, unwalkable, args)

    # Render the note --audience selected, plus -- when --notes-dir was
    # given -- every storefront that has a file of its own. One walk, four notes:
    # the ranges are already in memory and commit_files() is cached, so the extra
    # storefronts cost no API requests that the first one did not already make.
    wanted = [args.audience]
    if args.notes_dir:
        wanted += [n for n in sorted(AUDIENCES)
                   if AUDIENCES[n]["out"] and n not in wanted]
    notes = {}
    for name in wanted:
        notes[name] = render_note(name, sections, args, token)
    return full, notes, sections, filtered_counts, unwalkable, meta


def render_note(name, sections, args, token):
    """One storefront's note -> {"text", "limit", "held_back", ...}.

    Renders in that storefront's own format at that storefront's own limit; the
    only thing --store-limit does is override the number, for testing."""
    a = AUDIENCES[name]
    limit = a["limit"] if args.store_limit is None else args.store_limit
    # Absent (three storefronts out of four) means "the app tier may take the
    # whole budget"; see the linux entry for why exactly one sets it.
    app_limit = min(limit, a.get("app_limit", limit))
    lines, shared_from, held_back = store_bullets(
        sections, a["app"], a["shared"], token=token,
        path_check=args.store_path_check,
        limit=limit,
        max_candidates=args.store_candidates,
        min_subject=args.store_min_subject)
    boundary = a["boundary"] if args.store_boundary else None
    render = render_appstream if a["format"] == "appstream" else render_store
    text = render(lines, limit, args.lede_text, args.store_fallback,
                  shared_from, boundary, app_limit=app_limit)
    if a["format"] == "appstream":
        # WARN, never raise. A fragment that would not validate is still better
        # than losing the release, and urnetwork/linux's own CI is the backstop;
        # this is here so the problem is named at the point it is created rather
        # than in another repository three steps later.
        for p in appstream_problems(text):
            warn("%s note would fail appstreamcli: %s" % (name, p))
    return {"name": name, "text": text, "limit": limit, "format": a["format"],
            "out": a["out"], "where": a["where"], "held_back": held_back,
            "app": len(lines[:shared_from]), "shared": len(lines[shared_from:])}


def fit_full(meta, sections, filtered_counts, unwalkable, args):
    """Render the full changelog inside --full-limit, degrading in fixed steps.

    The GitHub release body caps at 125,000 characters and the API answers 422
    above it. run.sh PATCHes that body with $BUILD_CURL (which carries
    --fail-with-body, so an HTTP error is a non-zero exit) under error_trap, so an
    over-long body would abort the release AFTER every artifact was uploaded. The
    ladder below always terminates: bodies -> no bodies -> fewer commits per
    component -> a hard line-boundary trim."""
    limit = args.full_limit
    for mode in (args.body, "none"):
        text = render_full(meta, sections, filtered_counts, unwalkable, mode, args.body_limit)
        if len(text) <= limit:
            return text
        if mode == args.body:
            warn("full changelog over %d chars with bodies; dropping bodies" % limit)
    for cap in (100, 50, 25, 10, 5, 3, 1):
        text = render_full(meta, sections, filtered_counts, unwalkable, "none",
                           args.body_limit, per_component_cap=cap)
        if len(text) <= limit:
            warn("full changelog capped at %d commits per component" % cap)
            return text
    warn("full changelog still over %d chars; trimming at a line boundary" % limit)
    text = text[:limit - 200]
    return text[:text.rfind("\n") + 1] + "\n_... truncated to fit the release body limit._\n"


def self_test():
    """Network-free checks of the parts that would silently do the wrong thing.

    This is what CI runs: it needs no token, no rate limit and no clone, and it
    pins the two behaviours that matter -- the noise filters match the exact
    strings run.sh writes, and the store budget is never exceeded."""
    ok = True

    def check(name, cond):
        nonlocal ok
        print("%-58s %s" % (name, "ok" if cond else "FAIL"))
        if not cond:
            ok = False

    def commit(subject, body="", parents=1, sha="0" * 40):
        return {"sha": sha, "parents": [{}] * parents,
                "commit": {"message": subject + ("\n\n" + body if body else "")}}

    active = DEFAULT_FILTERS.split(",")
    # The literal strings all/run.sh commits (lines 523, 538, 553-574, 745, 1020,
    # 1510, 1516). If run.sh's wording changes, this is what notices.
    for s in ("2026.8.21-1025613560",
              "2026.8.21-1025613560 ip security and blocker update",
              "2026.8.21-1025613560 ur.io changelog update",
              "2026.8.21-1025613560 localizations update",
              "2026.8.21-1025613560-ungoogle",
              "buildhost build ungoogle"):
        check("filtered: %s" % s, is_filtered(commit(s), active) is not None)
    check("filtered: merge commit", is_filtered(commit("Merge pull request #470", parents=2), active) == "merge")
    check("kept: real subject", is_filtered(commit("Reduce steady-state allocation"), active) is None)
    check("kept: terse but real", is_filtered(commit("checkpoint"), active) is None)
    check("filters= keeps everything", is_filtered(commit("2026.8.21-1025613560"), []) is None)

    check("trailers stripped",
          body_of("s\n\nreal text\n\nCo-Authored-By: x <y>\nClaude-Session: z") == "real text")
    check("Fixes: kept", "Fixes: #12" in body_of("s\n\nreal\n\nFixes: #12"))
    check("first paragraph only",
          first_paragraph("one two.\n\nthree four.", 200) == "one two.")
    check("body clipped at a word boundary",
          first_paragraph("aaaa bbbb cccc dddd", 12).endswith("...")
          and len(first_paragraph("aaaa bbbb cccc dddd", 12)) <= 12)

    # The store budget, on a subject that is longer than the whole budget and on
    # a pile that overflows it.
    long_lines = ["x" * 600] + ["Fix %d" % i for i in range(200)]
    s = render_store(long_lines, 500, None, "- fallback")
    check("store <= 500 chars", len(s.rstrip("\n")) <= 500)
    check("store cut at a bullet boundary", all(l.startswith("- ") for l in s.strip().splitlines()))
    check("store has no leading whitespace", not s[:1].isspace())
    check("store ends in one newline", s.endswith("\n") and not s.endswith("\n\n"))
    check("store falls back when nothing fits",
          render_store([], 500, None, "- fallback").strip() == "- fallback")
    check("placeholder lede is recognised",
          "- bug and performance fixes." in PLACEHOLDER_LEDES)
    check("store skips test/chore commits",
          "- Pin the fixture" not in render_store(
              store_bullets([{"name": "android", "slug": "urnetwork/android", "commits": [
                  commit("test(routing): pin the fixture please")]}], ["android"])[0],
              500, None, "- f"))

    # -----------------------------------------------------------------------
    # PER-STOREFRONT NOTES.
    #
    # The three things that would go wrong silently: a storefront quietly
    # inheriting android's 500 (an Apple note capped at an eighth of what the
    # store accepts), the boundary marker pushing a note over a limit that is
    # enforced by a silent truncation, and the AppStream fragment being invalid
    # in a way that only breaks the NEXT Linux build, in another repository.
    # -----------------------------------------------------------------------
    check("--audience android and all both still exist",
          "android" in AUDIENCES and "all" in AUDIENCES)
    check("android is still 500 and still plain text",
          AUDIENCES["android"]["limit"] == 500
          and AUDIENCES["android"]["format"] == "text")
    check("android draws from exactly the components it always did",
          AUDIENCES["android"]["app"] + AUDIENCES["android"]["shared"]
          == ["android", "sdk", "connect", "glog", "goidenticons"])
    check("every storefront has its own limit, none inherits 500",
          [AUDIENCES[a]["limit"] for a in ("android", "windows", "apple")]
          == [500, 1500, 4000])
    check("every storefront plus the short view writes one file",
          all(AUDIENCES[a]["out"]
              for a in ("android", "apple", "linux", "windows", "all")))
    check("note filenames are flat suffixes, not nested paths",
          not any("/" in (AUDIENCES[a]["out"] or "") for a in AUDIENCES))
    check("no note is written under metadata/ (fdroid globs locales there)",
          not any((AUDIENCES[a]["out"] or "").startswith("metadata")
                  for a in AUDIENCES))
    check("all four apps share exactly one sdk tier",
          all(AUDIENCES[a]["shared"] == SHARED_COMPONENTS
              for a in ("android", "apple", "linux", "windows")))

    tiered = [{"name": "android", "slug": "urnetwork/android", "base": "a" * 40,
               "head": "b" * 40,
               "commits": [commit("Show degraded Auto transport status"),
                           commit("Stop capturing apps with no live exit")]},
              {"name": "sdk", "slug": "urnetwork/sdk", "base": "c" * 40,
               "head": "d" * 40,
               "commits": [commit("Expose degraded transport status across SDK")]}]
    lines, shared_from, _ = store_bullets(tiered, ["android"], SHARED_COMPONENTS)
    check("the tier boundary lands where the components change",
          shared_from == 2 and len(lines) == 3)
    # The same subject really does land in two components at once -- connect and
    # sdk both carried "Optimize low-bar network performance" in
    # v2026.8.15-1020621320...v2026.8.21-1025763520 -- and a store note that
    # says it three times is worse than one that says it once.
    dupes = tiered + [{"name": "connect", "slug": "urnetwork/connect", "base": "e" * 40,
                       "head": "f" * 40,
                       "commits": [commit("Expose degraded transport status across SDK"),
                                   commit("Show degraded Auto transport status")]}]
    check("a subject in two components is listed once",
          store_bullets(dupes, ["android"], SHARED_COMPONENTS)[0]
          == ["Show degraded Auto transport status",
              "Stop capturing apps with no live exit",
              "Expose degraded transport status across SDK"])
    check("no shared component, no boundary index past the end",
          store_bullets(tiered[:1], ["android"], SHARED_COMPONENTS)[1] == 2)

    plain = render_store(lines, 500, None, "- f", shared_from, "")
    check("android boundary is one blank line and nothing else",
          plain.splitlines()[2] == "" and len(plain.splitlines()) == 4)
    check("android boundary costs exactly one character",
          len(plain.rstrip("\n"))
          == len(render_store(lines, 500, None, "- f").rstrip("\n")) + 1)
    check("--no-store-boundary is byte-identical to the old single block",
          render_store(lines, 500, None, "- f", shared_from, None)
          == render_store(lines, 500, None, "- f"))
    headed = render_store(lines, 4000, None, "- f", shared_from, "Under the hood")
    check("apple/windows boundary is a blank line plus a plain heading",
          headed.splitlines()[2:4] == ["", "Under the hood"])
    check("the boundary appears exactly once",
          headed.count("\nUnder the hood\n") == 1)
    check("no boundary when no app bullet fitted above it",
          "Under the hood" not in
          render_store(lines, 60, None, "- f", shared_from, "Under the hood"))
    check("no boundary when the tail is empty",
          "Under the hood" not in
          render_store(lines[:2], 4000, None, "- f", 2, "Under the hood"))

    # The packer's arithmetic against the rendered bytes, at every real limit.
    # If those two ever disagree, a note goes over a cap that is enforced by a
    # silent mid-word truncation.
    pile = ["Subject number %d that is long enough to be kept" % i for i in range(200)]
    for name in ("android", "windows", "apple"):
        lim = AUDIENCES[name]["limit"]
        note = render_store(pile, lim, "A human headline", "- f", 120,
                            AUDIENCES[name]["boundary"])
        check("%s note fits %d chars including the boundary" % (name, lim),
              len(note.rstrip("\n")) <= lim)

    # app_limit, and the failure it exists to stop: on the real span
    # v2026.8.15-1020621320...v2026.8.21-1025763520 the linux app tier produced
    # 28 qualifying bullets, which took the whole editorial budget and left the
    # shared sdk/connect tail with nothing -- a note with no tail to delete.
    hoggish = ["A linux app change number %02d, spelled out at some length" % i
               for i in range(40)] + ["A shared sdk change, spelled out at the same length"]
    _, app_only, tail_only, _ = pack_bullets(hoggish, 1800, shared_from=40)
    check("without a cap the app tier can starve the tail", tail_only == [])
    _, capped, tail, _ = pack_bullets(hoggish, 1800, shared_from=40, app_limit=1000)
    check("app_limit reserves room for the shared tail",
          len(tail) == 1 and 0 < len(capped) < len(app_only))
    check("app_limit never widens the budget",
          len("\n".join("- " + t for t in capped + tail)) <= 1800)

    # The exact subjects these two rules were written against, from
    # urnetwork/linux and urnetwork/connect in the same real span.
    for wip in ("Wip(egress): mark the daemon's sockets at creation via cgroup-BPF",
                "Partial(canvas): settle dots when there is no clock",
                "wip: something unfinished"):
        check("store holds back %r" % wip[:28], bool(STORE_SKIP.search(wip)))
    check("a real subject containing the word partial is still kept",
          not STORE_SKIP.search("Restore partial provider windows on reconnect"))
    check("store drops a tune() label like any other commit type",
          polish("Tune(window): raise the quality window 2/6/12 -> 6/12/16")
          == "Raise the quality window 2/6/12 -> 6/12/16")

    # AppStream. Everything here was checked against appstreamcli 1.1.3, which
    # is what urnetwork/linux's CI runs; a WARNING there is a build failure.
    xml = render_appstream(lines, 1800, None, "- f", shared_from, "Under the hood")
    check("appstream fragment would validate", appstream_problems(xml) == [])
    check("appstream wraps bullets in <li>, never in \"- \"",
          "<li>Show degraded Auto transport status</li>" in xml
          and "\n- " not in xml)
    check("appstream marks the boundary with a free XML comment",
          APPSTREAM_SHARED_OPEN in xml and APPSTREAM_SHARED_CLOSE in xml)
    check("appstream comment markers contain no illegal '--'",
          "--" not in APPSTREAM_SHARED_OPEN[4:-3]
          and "--" not in APPSTREAM_SHARED_CLOSE[4:-3])
    check("appstream escapes an ampersand",
          "<li>Fix A &amp; B routing on the exit side</li>"
          in render_appstream(["Fix A & B routing on the exit side"], 1800, None, "- f"))
    check("appstream strips a plaintext URL rather than failing the build",
          appstream_problems(render_appstream(
              ["Document the flow at https://ur.io/docs for operators"],
              1800, None, "- f")) == [])
    # Strip-and-leave-empty would be a DIFFERENT fatal issue (tag-empty), so the
    # bullet has to disappear, not shrink.
    urlonly = render_appstream(["https://ur.io/releases", "Keep this real bullet here"],
                               1800, None, "- f")
    check("a bullet that was only a URL is dropped, not emptied",
          "<li></li>" not in urlonly and appstream_problems(urlonly) == []
          and urlonly.count("<li>") == 1)
    check("appstream falls back to a paragraph, not to a \"- \" line",
          render_appstream([], 1800, None, "- Bug and performance fixes.").strip()
          == "<p>Bug and performance fixes.</p>")
    check("the appstream checker catches raw text under <description>",
          appstream_problems("- a bullet\n") != [])
    check("the appstream checker catches an unescaped ampersand",
          appstream_problems("<p>A &amp B</p>") != [])
    check("the appstream checker catches a plaintext URL",
          appstream_problems("<p>See https://ur.io</p>") != [])
    check("the appstream checker catches an empty <li>",
          appstream_problems("<ul><li></li></ul>") != [])
    check("the appstream checker catches a disallowed tag",
          appstream_problems("<p><b>no</b></p>") != [])
    # The exact file lists of the two Android CI commits in
    # v2026.8.21-1025223880...v2026.8.21-1025763520, and of the app commit next
    # to them, so this rule is pinned against real data rather than a guess.
    check("path rule: CI-only commit is not app news",
          STORE_NONSHIPPING_PATHS.search(".github/workflows/build-and-test.yml"))
    check("path rule: app source is app news",
          not STORE_NONSHIPPING_PATHS.search(
              "app/app/src/main/java/com/bringyour/network/MainApplication.kt"))
    check("path rule: a go source file is app news",
          not STORE_NONSHIPPING_PATHS.search("message_pool.go")
          and not STORE_NONSHIPPING_PATHS.search("ip_remote_multi_client.go"))
    check("path rule: a go test file is not",
          bool(STORE_NONSHIPPING_PATHS.search("memory_budget_test.go")))
    check("path rule: a build script is not",
          bool(STORE_NONSHIPPING_PATHS.search("build/check_apple_size.sh")))
    check("path rule: mixed commit stays (all() not any())",
          not all(STORE_NONSHIPPING_PATHS.search(f) for f in [
              "app/app/build.gradle",
              "app/app/src/main/java/com/bringyour/network/MainApplication.kt",
              "app/scripts/PHYSICAL_LOWBAR.md"]))
    check("store polishes a subject",
          polish("[android] a thing.") == "A thing")
    check("store drops the conventional-commit label",
          polish("Fix(android): don't capture apps with no live exit")
          == "Don't capture apps with no live exit")
    # Every one of these came out of the real note for
    # v2026.7.22-999364020...v2026.8.21-1025763520 before this rule existed.
    for vague in ("Audit", "Gitignore update", "Performance checkpoint",
                  "Drop the fork's multipleIps UI rename from the checkpoint"):
        check("store holds back %r" % vague, bool(STORE_SKIP.search(vague)))
    check("store holds back a subject that is too short",
          len(polish("Fix version")) < STORE_MIN_SUBJECT)
    check("store keeps a real subject",
          not STORE_SKIP.search("Reduce steady-state allocation and pool retention")
          and len(polish("Reduce steady-state allocation and pool retention")) >= STORE_MIN_SUBJECT)
    check("body escaping leaves authored markdown alone",
          md_escape("use `pacman -R` on <version>")
          == r"use `pacman -R` on \<version\>")
    check("subject escaping protects the bold/link structure",
          md_escape("a *b* [c]", subject=True) == r"a \*b\* \[c\]")

    check("gitmodules parsed",
          parse_gitmodules("[submodule \"sn\"]\n\tpath = sn\n\turl = git@github.com:urfoundation/sn.git\n")
          == {"sn": "urfoundation/sn"})
    check("https gitmodules parsed",
          parse_gitmodules("[submodule \"sdk\"]\n\tpath = sdk\n\turl = https://github.com/urnetwork/sdk.git\n")
          == {"sdk": "urnetwork/sdk"})
    check("component order: apps before infra",
          order_components(["glog", "android", "zzz-new", "connect"])
          == ["android", "connect", "glog", "zzz-new"])

    meta = {"from_label": "vA", "to_label": "vB"}
    sections = [{"name": "android", "slug": "urnetwork/android", "base": "a" * 40,
                 "head": "b" * 40, "commits": [commit("Do a thing", "why it matters", sha="c" * 40)]}]
    full = render_full(meta, sections, {"release": 3}, [], "first-paragraph", 500)
    check("full links the commit", "urnetwork/android/commit/" + "c" * 40 in full)
    check("full links the compare", "/compare/" + "a" * 40 + "..." + "b" * 40 in full)
    check("full names what it filtered", "3 version-stamp commits" in full)
    check("filter count is singular when it is one",
          "1 version-stamp commit written" in
          render_full(meta, sections, {"release": 1}, [], "none", 500))
    check("full carries the body", "why it matters" in full)

    args = argparse.Namespace(body="first-paragraph", body_limit=500, full_limit=400)
    big = [{"name": "android", "slug": "urnetwork/android", "base": "a" * 40, "head": "b" * 40,
            "commits": [commit("Subject number %d" % i, "body " * 50, sha="%040d" % i)
                        for i in range(300)]}]
    global _QUIET
    _QUIET = True
    fitted = fit_full(meta, big, {}, [], args)      # warns by design; see warn()
    _QUIET = False
    check("full changelog fits its limit", len(fitted) <= 400)

    print("self-test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def main(argv=None):
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    p = argparse.ArgumentParser(
        description="Generate a changelog for a release from the submodule pins.")
    p.add_argument("--from", dest="from_ref", metavar="REF",
                   help="the previous release tag (default: the newest base release "
                        "tag that is not --to)")
    p.add_argument("--to", default="worktree", metavar="REF",
                   help="this release: a tag, a commit, or 'worktree' to read the "
                        "pins out of the current checkout (default: worktree)")
    p.add_argument("--to-label", metavar="TEXT",
                   help="what to call --to in the rendered heading (the release "
                        "path passes v<version>, because 'worktree' means nothing "
                        "to somebody reading a release body)")
    p.add_argument("--repo", default=here, help="the build repo checkout (default: %(default)s)")
    p.add_argument("--store-out", metavar="PATH",
                   help="write the --audience storefront's note here")
    p.add_argument("--notes-dir", metavar="DIR",
                   help="ALSO write one file per storefront under DIR, plus the full "
                        "changelog and the short everything-included view, each named "
                        "<--notes-prefix>_Full.md, _Simple.txt, _Android.txt, "
                        "_Apple.txt, _Windows.txt, _Linux.xml. Each store's note is "
                        "rendered in that store's own format at that store's own "
                        "limit. Keep DIR out of metadata/ -- see AUDIENCES.")
    p.add_argument("--notes-prefix", metavar="TEXT",
                   help="filename prefix inside --notes-dir (default: the version "
                        "code trailing --to-label, else the label itself)")
    p.add_argument("--full-out", metavar="PATH", help="write the full changelog here")
    p.add_argument("--store-limit", type=int, default=None, metavar="N",
                   help="override the store note budget in characters. There is no "
                        "single default: each storefront's real limit is in "
                        "AUDIENCES -- android 500 (fdroidserver char_limits"
                        "['whatsNew'] and Play's cap), windows 1500 (Partner "
                        "Center), apple 4000 (App Store Connect), linux an "
                        "editorial budget because AppStream sets no limit at all. "
                        "This overrides EVERY note in the run, so it is a testing "
                        "knob, not a per-store setting")
    p.add_argument("--full-limit", type=int, default=120000,
                   help="full changelog budget in characters (default: %(default)s -- "
                        "GitHub's release body cap is 125000 and run.sh prepends a header "
                        "and the VirusTotal table)")
    p.add_argument("--audience", choices=sorted(AUDIENCES), default="android",
                   help="which storefront --store-out is for; also picks the limit "
                        "and the format (default: %(default)s -- the F-Droid/Play "
                        "note all/run.sh copies into metadata/en-US/changelogs)")
    p.add_argument("--filters", default=DEFAULT_FILTERS,
                   help="comma-separated noise filters, or empty to keep every commit "
                        "(default: %(default)s; known: " + ",".join(sorted(FILTERS)) + ")")
    p.add_argument("--body", choices=("first-paragraph", "full", "none"),
                   default="first-paragraph", help="how much commit body to include "
                                                   "in the full changelog (default: %(default)s)")
    p.add_argument("--body-limit", type=int, default=500,
                   help="characters of body per commit (default: %(default)s)")
    p.add_argument("--lede", metavar="PATH",
                   help="a human-written headline to put above the generated store "
                        "bullets (metadata/en-US/changelogs/pending.txt); ignored when "
                        "it still holds the shipped placeholder text")
    p.add_argument("--no-store-path-check", dest="store_path_check",
                   action="store_false", default=True,
                   help="do not hold CI/test/docs/build-only commits out of the "
                        "store note (they are always in the full changelog)")
    p.add_argument("--no-store-boundary", dest="store_boundary",
                   action="store_false", default=True,
                   help="do not mark where this app's own changes end and the shared "
                        "sdk/connect tail begins. Reproduces, byte for byte, the "
                        "single-block note this generator produced before there was "
                        "more than one storefront")
    p.add_argument("--store-min-subject", type=int, default=STORE_MIN_SUBJECT,
                   metavar="N",
                   help="hold subjects shorter than N characters out of the store "
                        "note (default: %(default)s; 0 disables)")
    p.add_argument("--store-candidates", type=int, default=40, metavar="N",
                   help="how many store candidates to look up file paths for, per "
                        "note (default: %(default)s; one API request each, and a "
                        "commit another note already looked up costs nothing). A "
                        "span long enough to exhaust this renders the same either "
                        "way for the same command line, but a note rendered "
                        "alongside others may filter more of its shared tail than "
                        "the same note rendered on its own, because the others "
                        "warmed the cache")
    p.add_argument("--store-fallback", default="- Bug and performance fixes.",
                   help="what the store note says when no commit qualifies")
    p.add_argument("--self-test", action="store_true",
                   help="run the network-free checks and exit")
    args = p.parse_args(argv)

    if args.self_test:
        return self_test()

    args.lede_text = None
    if args.lede and os.path.exists(args.lede):
        raw = open(args.lede, encoding="utf-8").read()
        if " ".join(raw.split()).lower().strip() in PLACEHOLDER_LEDES:
            warn("%s still holds the shipped placeholder; ignoring it" % args.lede)
        elif raw.strip():
            args.lede_text = raw

    token = api_token()
    if not token:
        warn("no GITHUB_API_KEY/GITHUB_TOKEN; walking anonymously (every repo is public)")
        if args.store_path_check:
            # SAY THIS LOUDLY, because the degradation is silent otherwise and
            # it lands in the artifact a user reads. The store path check needs
            # one extra API call per candidate commit to see which files it
            # touched, and anonymous callers get 60 requests/hour -- so on any
            # real span those lookups start returning 403. commit_files() then
            # fails OPEN by design (a missed lookup must never delete somebody's
            # work), which means the commit is KEPT. The note stays truthful but
            # gets less relevant: measured on a one-week span, anonymous led with
            # "Tighten the job timeouts" and "Add a build-and-test workflow",
            # while the same span with a token held 6 CI/test/docs-only commits
            # back and led with the shipping changes instead.
            warn("  the store note holds CI/test/docs-only commits back by "
                 "looking up each commit's files, and that lookup is rate "
                 "limited to 60/hour without a token. Expect build-only commits "
                 "to survive into the note. Export GITHUB_API_KEY (run.sh "
                 "already does) or pass --no-store-path-check to skip the check "
                 "deliberately rather than by accident.")

    full, notes, sections, filtered, unwalkable, meta = build(args, token)
    selected = notes[args.audience]

    # EVERY artifact is fully rendered before ANY file is opened, so a failure
    # above leaves the tree exactly as it was and run.sh's fallback has something
    # to fall back to. That invariant is why the loop below only writes.
    def write_text(path, text):
        d = os.path.dirname(path)
        if d:
            os.makedirs(d, exist_ok=True)
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(text)

    if args.store_out:
        write_text(args.store_out, selected["text"])
    if args.notes_dir:
        # "v2026.8.21-1025763520" -> "1025763520". The version code alone is the
        # filename prefix: it sorts chronologically, it is what
        # metadata/en-US/changelogs/ already keys on, and it survives the tag
        # being re-cut under a different name. A label with no trailing code
        # (a branch, "worktree") is sanitised and used whole rather than
        # silently collapsing every run onto one set of filenames.
        prefix = args.notes_prefix
        if not prefix:
            label = meta["to_label"] or "release"
            m = re.search(r"-(\d+)$", label)
            prefix = m.group(1) if m else re.sub(r"[^A-Za-z0-9._-]", "_", label)
        for name in sorted(notes):
            if notes[name]["out"]:
                write_text(os.path.join(args.notes_dir,
                                        "%s_%s" % (prefix, notes[name]["out"])),
                           notes[name]["text"])
        # The full changelog goes in beside them, and is the file the release
        # body links to instead of inlining 100k of markdown.
        write_text(os.path.join(args.notes_dir, "%s_Full.md" % prefix), full)
    if args.full_out:
        write_text(args.full_out, full)
    if not args.store_out and not args.full_out and not args.notes_dir:
        sys.stdout.write(full)

    warn("%d component(s), %d commit(s), %d filtered as builder noise, %d unwalkable"
         % (len(sections), sum(len(s["commits"]) for s in sections),
            sum(filtered.values()), len(unwalkable)))
    for name in sorted(notes):
        n = notes[name]
        size = len(n["text"].rstrip("\n"))
        # An AppStream fragment is measured, not budgeted: the number below is
        # the file, tags and all, and there is no limit to compare it to. Saying
        # "1743/1800" for it would invent a cap the format does not have.
        if n["format"] == "appstream":
            warn("%s note %d chars of AppStream XML (no format limit; %d-char "
                 "editorial budget on the bullet text)%s"
                 % (name, size, n["limit"], " -> " + n["out"] if n["out"] else ""))
        else:
            warn("%s note %d/%d chars%s"
                 % (name, size, n["limit"], " -> " + n["out"] if n["out"] else ""))
        warn("  %d app bullet(s) then %d shared sdk/connect bullet(s) available; "
             "pasted into %s" % (n["app"], n["shared"], n["where"]))
    for reason, n in sorted(selected["held_back"].items()):
        warn("  held out of the store note (still in the full changelog): %d with %s"
             % (n, reason))
    return 0


if __name__ == "__main__":
    sys.exit(main())
