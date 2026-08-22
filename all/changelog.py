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
#     repo's path today (run.sh:1489 is still `# FIXME android play release`, the
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

# What actually ships inside the Android artifact the stores serve. apple, linux,
# windows, extension, web, server, proxy and sn changes are real work and they
# are all in the full changelog -- but they are not in this .aab, and the store
# note has 500 characters for the entire release. Spending them describing the
# macOS app to an F-Droid user is the wrong trade.
#
# warp is warpctl, the build tool: it produces the artifact, it is not in it.
# glog and goidenticons are compiled into the sdk, so they are.
AUDIENCES = {
    "android": ["android", "sdk", "connect", "glog", "goidenticons"],
    "all": None,   # every component, in COMPONENT_ORDER
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
STORE_SKIP = re.compile(
    r"^(?:test|tests|ci|chore|build|docs?|refactor|style|lint|deps|bump|release)\b\s*(?:\([^)]*\))?\s*:"
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
STORE_TYPE_PREFIX = re.compile(r"^(?:fix|feat|feature|perf|improve|add|update)\s*"
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


def commit_files(slug, sha, token):
    """The paths one commit touched.

    /compare returns a `files` array for the WHOLE range, never per commit, so
    this is a separate request per candidate -- which is why only the handful of
    commits that could actually fit in the store note are ever looked up
    (--store-candidates), and why the full changelog never uses this."""
    d = api_get("/repos/%s/commits/%s" % (slug, sha), token)
    return [f["filename"] for f in (d.get("files") or [])]


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


def store_bullets(sections, audience_names, token=None, path_check=False,
                  limit=500, max_candidates=40, min_subject=STORE_MIN_SUBJECT):
    """The lines the store note may draw from, in priority order.

    Priority is the audience list order (the app first, then what the app is
    built from) and newest commit first inside each component, so if the budget
    only fits three bullets they are the three most recent, most user-facing
    ones.

    Returns (lines, held_back) where held_back maps a reason to the number of
    commits it held out of the note. Every one of them is still in the full
    changelog, and the reasons are printed -- nothing here drops silently."""
    by_name = {s["name"]: s for s in sections}
    names = audience_names if audience_names is not None else [s["name"] for s in sections]
    seen, lines, looked_up = set(), [], 0
    held_back = {}
    budget = 0
    for name in names:
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
            if path_check and looked_up < max_candidates:
                looked_up += 1
                try:
                    files = commit_files(s["slug"], c["sha"], token)
                except ApiError as e:
                    # Fail OPEN. A missed lookup must not delete somebody's work
                    # from the release notes; at worst the note is less relevant.
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
                return lines, held_back
    return lines, held_back


def polish(subject):
    """Commit subject -> a line a store reader can read."""
    s = " ".join(subject.split())
    s = re.sub(r"^\[[^\]]*\]\s*", "", s)          # "[android] foo"
    s = STORE_TYPE_PREFIX.sub("", s)
    s = s.rstrip(" .")
    if s and s[0].islower():
        s = s[0].upper() + s[1:]
    return s


def render_store(lines, limit, lede, fallback):
    """Pack bullets into `limit` characters, cutting only at bullet boundaries.

    The budget is `limit` characters of CONTENT, excluding the single trailing
    newline: F-Droid slices [:500] on a read that includes the newline, so a
    500-character body plus "\\n" survives intact -- reserving a character for the
    newline would throw away a character of real changelog for nothing.

    Characters, not bytes. Every changelog file in this repo today is ASCII so
    `wc -c` happens to agree, but subjects pulled from 17 submodules carry em
    dashes and non-ASCII names, and both stores count code points."""
    out, used = [], 0
    if lede:
        for l in lede.strip().splitlines():
            l = l.rstrip()
            if not l:
                continue
            cost = len(l) + (1 if out else 0)
            if used + cost > limit:
                break
            out.append(l)
            used += cost
    for text in lines:
        line = "- " + text
        cost = len(line) + (1 if out else 0)
        if used + cost > limit:
            # Skip this bullet and try the next; a shorter one may still fit.
            # Never truncate mid-subject -- a half-sentence is what we are here to
            # stop the stores from producing.
            continue
        out.append(line)
        used += cost
    if not out:
        # Never emit an empty store file. An empty <versionCode>.txt would make
        # F-Droid show a blank "What's New" -- strictly worse than the placeholder
        # it replaces.
        return (fallback or "- Bug and performance fixes.").strip() + "\n"
    return "\n".join(out) + "\n"


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
    lines, held_back = store_bullets(sections, AUDIENCES[args.audience], token,
                                     path_check=args.store_path_check,
                                     limit=args.store_limit,
                                     max_candidates=args.store_candidates,
                                     min_subject=args.store_min_subject)
    store = render_store(lines, args.store_limit, args.lede_text, args.store_fallback)
    return full, store, sections, filtered_counts, unwalkable, held_back


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
    p.add_argument("--store-out", metavar="PATH", help="write the store note here")
    p.add_argument("--full-out", metavar="PATH", help="write the full changelog here")
    p.add_argument("--store-limit", type=int, default=500,
                   help="store note budget in characters (default: %(default)s -- "
                        "fdroidserver char_limits['whatsNew'] and Play's cap)")
    p.add_argument("--full-limit", type=int, default=120000,
                   help="full changelog budget in characters (default: %(default)s -- "
                        "GitHub's release body cap is 125000 and run.sh prepends a header "
                        "and the VirusTotal table)")
    p.add_argument("--audience", choices=sorted(AUDIENCES), default="android",
                   help="which components the STORE note draws from (default: %(default)s)")
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
    p.add_argument("--store-min-subject", type=int, default=STORE_MIN_SUBJECT,
                   metavar="N",
                   help="hold subjects shorter than N characters out of the store "
                        "note (default: %(default)s; 0 disables)")
    p.add_argument("--store-candidates", type=int, default=40, metavar="N",
                   help="how many store candidates to look up file paths for "
                        "(default: %(default)s; one API request each)")
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

    full, store, sections, filtered, unwalkable, held_back = build(args, token)

    # Both artifacts are fully rendered before either file is opened, so a
    # failure above leaves the tree exactly as it was and run.sh's fallback has
    # something to fall back to.
    if args.store_out:
        with open(args.store_out, "w", encoding="utf-8", newline="\n") as f:
            f.write(store)
    if args.full_out:
        with open(args.full_out, "w", encoding="utf-8", newline="\n") as f:
            f.write(full)
    if not args.store_out and not args.full_out:
        sys.stdout.write(full)

    warn("%d component(s), %d commit(s), %d filtered as builder noise, %d unwalkable"
         % (len(sections), sum(len(s["commits"]) for s in sections),
            sum(filtered.values()), len(unwalkable)))
    warn("store note %d/%d chars" % (len(store.rstrip("\n")), args.store_limit))
    for reason, n in sorted(held_back.items()):
        warn("  held out of the store note (still in the full changelog): %d with %s"
             % (n, reason))
    return 0


if __name__ == "__main__":
    sys.exit(main())
