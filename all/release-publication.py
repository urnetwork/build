#!/usr/bin/env python3
"""Reconcile exact release publications only; no metadata/changelog walks."""
import base64
import hashlib
import json
import importlib.util
import math
import os
from pathlib import Path
import re
import sys
import subprocess
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request

# This helper also runs after the source seal, without Python's -B flag. Its
# local import must not create untracked source inputs under build/all.
sys.dont_write_bytecode = True

API = "https://api.github.com/repos/urnetwork/build"
GET_MAX_ATTEMPTS = 4
GET_RETRY_SECONDS = 3700
_spec = importlib.util.spec_from_file_location("release_state", Path(__file__).with_name("release-state.py"))
state = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(state)


class Conflict(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


def rate_limit_delay(error, attempt):
    """Classify an explicit GitHub limit without disclosing response content."""
    if error.code not in (403, 429):
        return None
    headers = error.headers or {}
    primary = headers.get("X-RateLimit-Remaining") == "0"
    retry_after = headers.get("Retry-After")
    limited = primary or error.code == 429 or retry_after is not None
    if not limited:
        # Secondary limits can omit headers. Read only a bounded JSON message
        # to classify GitHub's documented response; never log its body/reason.
        try:
            body = json.loads(error.read(65537))
            message = body.get("message", "") if isinstance(body, dict) else ""
            limited = isinstance(message, str) and any(
                phrase in message.lower() for phrase in ("secondary rate limit", "api rate limit exceeded"))
        except (OSError, ValueError):
            return None
    if not limited:
        return None
    delays = []
    if retry_after is not None:
        if not re.fullmatch(r"[0-9]{1,12}", retry_after):
            raise Conflict("publication GET rate limit has invalid Retry-After; no retry was sent")
        delays.append(max(1, int(retry_after)))
    if primary:
        reset = headers.get("X-RateLimit-Reset", "")
        if not re.fullmatch(r"[0-9]{1,12}", reset):
            raise Conflict("publication GET primary rate limit lacks a valid reset; no retry was sent")
        delays.append(max(1, math.ceil(int(reset) - time.time() + 1)))
    # Honor both headers when present, never retry before either deadline.
    return max(delays) if delays else 60 * (2 ** (attempt - 1))


def request(url, method="GET", data=None, github=True, missing=False):
    headers = {"Accept": "application/vnd.github+json" if github else "application/json"}
    if github:
        if not url.startswith(API + "/"):
            raise Conflict("unexpected authenticated publication endpoint")
        headers.update(Authorization="Bearer " + os.environ.get("GITHUB_API_KEY", ""),
                       **{"X-GitHub-Api-Version": "2022-11-28"})
    body = None if data is None else json.dumps(data).encode()
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    deadline = time.monotonic() + GET_RETRY_SECONDS
    for attempt in range(1, GET_MAX_ATTEMPTS + 1):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise Conflict("publication GET rate-limit retry budget exhausted")
        try:
            with urllib.request.build_opener(NoRedirect).open(req, timeout=min(45, remaining)) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            try:
                if missing and error.code == 404:
                    return None
                # Only exact read-only GitHub reconciliation requests retry.
                # POST/PATCH, other registries, auth failures and ambiguous
                # transport results retain their existing fail-closed behavior.
                delay = rate_limit_delay(error, attempt) if github and method == "GET" and data is None else None
                if delay is None:
                    raise Conflict("publication " + method + " returned HTTP " + str(error.code) + "; response body withheld") from None
                if attempt == GET_MAX_ATTEMPTS:
                    raise Conflict("publication GET rate-limit retry limit reached") from None
                if delay >= deadline - time.monotonic():
                    raise Conflict("publication GET rate-limit wait exceeds remaining retry budget") from None
                print(f"publication GET rate-limited (HTTP {error.code}); retrying in {delay}s ({attempt}/{GET_MAX_ATTEMPTS})", file=sys.stderr, flush=True)
            finally:
                error.close()
            # Keep waits interruptible and bounded; the next attempt retains
            # exactly the URL, authorization and JSON validation contract.
            while delay > 0:
                interval = min(60, delay)
                time.sleep(interval)
                delay -= interval
        except (OSError, ValueError):
            raise Conflict("publication " + method + " failed or returned invalid JSON; reconcile before retrying") from None


def verify_tag(version, commit):
    tag = "refs/tags/v" + version
    try:
        result = subprocess.run(["git", "-C", os.environ["BUILD_HOME"], "ls-remote", "--exit-code", "--tags", "origin", tag, tag + "^{}"],
                                capture_output=True, timeout=45)
    except (OSError, subprocess.TimeoutExpired):
        raise Conflict("could not read exact remote release tag") from None
    if result.returncode:
        raise Conflict("exact remote release tag is missing or unreadable")
    refs = dict(line.split()[::-1] for line in result.stdout.decode().splitlines())
    if refs.get(tag + "^{}", refs.get(tag)) != commit:
        raise Conflict("actual published release tag points at a different commit; no publication was changed")


def release(version, commit, prerelease):
    workspace = os.environ["WARP_HOME"]
    if not state.inherited_lock(workspace):
        raise Conflict("release publication requires the workspace lock")
    base = os.environ.get("RELEASE_STATE_VERSION", version)
    _, manifest = state.load_manifest(workspace, base)
    if version not in (base, base.rsplit("-", 1)[0] + "-" + str(int(base.rsplit("-", 1)[1]) + 2),
                       base.rsplit("-", 1)[0] + "-" + str(int(base.rsplit("-", 1)[1]) + 3)):
        raise Conflict("release is not a sealed version or its declared ABI variant")
    receipt = state.state_root(workspace) / base / ("github-" + version + ".json")
    verify_tag(version, commit)
    tag = "v" + version
    identity = {"tag": tag, "commit": commit, "prerelease": prerelease, "seal": manifest["seal"]}
    recorded = json.loads(receipt.read_text()) if receipt.exists() else None
    if recorded is not None and any(recorded.get(key) != value for key, value in identity.items()):
        raise Conflict("saved GitHub release identity conflicts with this seal")
    if recorded and recorded.get("id"):
        existing = request(API + "/releases/" + str(recorded["id"]))
    else:
        # This endpoint finds PUBLISHED releases only. A missing response after
        # an uncertain draft POST is never permission to create another draft.
        existing = request(API + "/releases/tags/" + urllib.parse.quote(tag, safe=""), missing=True)
    if existing is None and recorded is not None:
        raise Conflict("draft creation outcome is uncertain and no ID was observed; inspect this exact draft before continuing, do not repeat POST")
    if existing is None:
        state.atomic_json(receipt, dict(identity, state="uncertain"))
        existing = request(API + "/releases", "POST", {
            "tag_name": tag, "target_commitish": commit, "name": tag, "body": tag,
            "draft": True, "prerelease": prerelease, "generate_release_notes": False})
    # target_commitish is ignored by GitHub when a tag exists and may be a
    # branch name. verify_tag above proves the actual immutable target instead.
    if (existing.get("tag_name") != tag
            or existing.get("prerelease") != prerelease or not isinstance(existing.get("id"), int)):
        raise Conflict("release identity conflicts with the sealed tag/commit/channel; no release was overwritten")
    state.atomic_json(receipt, dict(identity, state="observed", id=existing["id"]))
    return existing


def asset(release_id, name, path):
    size = Path(path).stat().st_size
    if size == 0:
        raise Conflict("empty publication artifact")
    sha = hashlib.sha256()
    with open(path, "rb") as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b""):
            sha.update(data)
    wanted = "sha256:" + sha.hexdigest()
    found = []
    for page in range(1, 101):
        listing = request(API + f"/releases/{release_id}/assets?per_page=100&page={page}")
        if not isinstance(listing, list):
            raise Conflict("invalid release asset response")
        found += [item for item in listing if item.get("name") == name]
        if len(listing) < 100:
            break
    else:
        raise Conflict("exact release asset listing exceeded its bound")
    if not found:
        return None
    if len(found) != 1 or found[0].get("size") != size or found[0].get("digest") != wanted or found[0].get("state") != "uploaded":
        raise Conflict("existing release asset " + name + " has conflicting/unverifiable content; never delete or replace it")
    return found[0]


def npm_archive(path, expected_version):
    with tarfile.open(path, "r:gz") as archive:
        member = archive.extractfile("package/package.json")
        if member is None:
            raise Conflict("npm tarball has no package identity")
        package = json.load(member)
    if package.get("version") != expected_version:
        raise Conflict("npm tarball version differs from the sealed release")
    sha = hashlib.sha512()
    with open(path, "rb") as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b""):
            sha.update(data)
    integrity = "sha512-" + base64.b64encode(sha.digest()).decode()
    spec = urllib.parse.quote(package["name"], safe="") + "/" + urllib.parse.quote(expected_version, safe="")
    remote = request("https://registry.npmjs.org/" + spec, github=False)
    if remote.get("name") != package["name"] or remote.get("version") != expected_version or remote.get("dist", {}).get("integrity") != integrity:
        raise Conflict("published npm package differs from the sealed artifact: " + package["name"])


def main():
    action, *args = sys.argv[1:]
    if action == "release" and len(args) == 3:
        version, commit, prerelease = args
        if not re.fullmatch(r"\d+\.\d+\.\d+-\d+", version) or not re.fullmatch(r"[0-9a-f]{40,64}", commit):
            raise Conflict("invalid exact release identity")
        print(json.dumps(release(version, commit, prerelease == "true")))
    elif action == "asset" and len(args) == 3:
        release_id, name, path = args
        if not release_id.isdigit():
            raise Conflict("invalid release ID")
        existing = asset(release_id, name, path)
        if existing is None:
            return 10
        print(json.dumps(existing))
    elif action == "npm" and len(args) >= 2:
        version, *archives = args
        for path in archives:
            npm_archive(path, version)
        print("verified exact published npm tarballs")
    elif action == "finalized" and len(args) == 2:
        release_id, version = args
        existing = request(API + "/releases/" + str(int(release_id)))
        if existing.get("tag_name") != "v" + version:
            raise Conflict("release ID/tag changed before finalization")
        return 10 if existing.get("draft") is False else 0
    else:
        raise Conflict("invalid publication reconciliation arguments")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (Conflict, state.Refusal, OSError, ValueError, KeyError, tarfile.TarError) as error:
        print("release publication refused: " + str(error), file=sys.stderr)
        sys.exit(65)
