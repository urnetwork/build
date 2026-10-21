#!/usr/bin/env python3
"""Private, fail-closed state for one immutable release; never executes journal text.

The workspace lock protects the release process group; an independent reaper
also covers children that close inherited FDs. Detached VMs/containers require
read-only writer inventory after an abnormal exit, never stale-lock deletion.
No journal text is executed. Diagnostics omit command output and env values.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor

SCHEMA = 1
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+-[0-9]+\Z")
STAGE = re.compile(r"[a-zA-Z0-9_.+-]+\Z")
# Auth may be refreshed. Presence of optional publishers is an immutable build
# choice; their credential bytes are never included, even as hashes.
AUTH = ("NPM_TOKEN", "NODE_AUTH_TOKEN", "TWINE_PASSWORD", "PYPI_TOKEN", "NUGET_API_KEY",
        "GEM_HOST_API_KEY", "CARGO_REGISTRY_TOKEN", "MAVEN_CENTRAL_USERNAME",
        "MAVEN_CENTRAL_PASSWORD", "SDK_SWIFT_GIT_TOKEN", "COCOAPODS_TRUNK_TOKEN",
        "CONAN_LOGIN_USERNAME", "CONAN_PASSWORD", "SDK_VCPKG_GIT_TOKEN")
OPTIONS = ("BUILD_TEST", "WARP_SKIP_DEPLOY", "BUILD_URIO_CHANGELOG",
           "BUILD_RELEASE_BODY_CHANGELOG", "BUILD_SKIP_VULN_SCAN", "CONNECT_IP_UPDATE",
           "BUILD_APPLE_IDENTITY", "BUILD_OUT", "ARCHES", "ROLES", "ARCH",
           "WINDOWS_BUILD_ARCHITECTURES", "SDK_PACKAGE_CHANNEL", "SDK_GPG_KEY_ID",
           "SDK_CONAN_REMOTE_URL", "ARIN_RELAY_GEOFEEDS", "GEOIP_CONF_FILE",
           "ARIN_CREDENTIALS_FILE", "ARIN_RULES_FILE", "ARIN_SUBSCRIBER_CATALOG_FILE",
           "ARIN_PREVIOUS_DATABASE_FILE", "ARIN_CANARY_DECISION_FILE",
           "WARP_MAIN_DOMAIN", "WARP_MAIN_LAN_HOST", "FDROID_VERSION_CODE_OFFSETS",
           "STAGE_SECONDS",
           "SDKROOT", "DEVELOPER_DIR", "ANDROID_HOME", "ANDROID_NDK_HOME")
GO_ENV_KEYS = ("GOENV", "GOWORK", "GOTOOLCHAIN", "GOROOT", "GOFLAGS", "GOEXPERIMENT",
               "CC", "CXX", "CGO_ENABLED", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS",
               "CGO_LDFLAGS", "GOOS", "GOARCH", "GOAMD64", "GOARM", "GOARM64",
               "GOMIPS", "GOMIPS64", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY",
               "GONOSUMDB", "GOPATH", "GOMODCACHE", "GOCACHE", "GODEBUG")
TOOLS = (("go", "version"), ("java", "-version"), ("node", "--version"),
         ("npm", "--version"), ("xcodebuild", "-version"), ("git", "--version"),
         ("docker", "--version"), ("zig", "version"), ("python3", "--version"))
# This is the exact set tagged by immutable preparation in run.sh. gvisor and
# standalone candidate inputs are source-pinned but are not release-tagged.
TAGGED_COMPONENTS = ("glog", "connect", "userwireguard", "goidenticons", "proxy", "sdk",
                     "sn", "server", "android", "apple", "windows", "linux", "web",
                     "warp", "localizations", "extension")


class Refusal(Exception):
    pass


def digest(data):
    return hashlib.sha256(data).hexdigest()


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def run(argv, cwd=None, combined=False):
    result = subprocess.run(argv, cwd=cwd, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT if combined else subprocess.PIPE,
                            check=False, timeout=60)
    if result.returncode:
        raise Refusal("input inspection failed: " + argv[0] + " (output withheld)")
    return result.stdout


def private_dir(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    st = path.lstat()
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid() or st.st_mode & 0o077:
        raise Refusal("checkpoint directory must be owned by this user with mode 0700: " + str(path))


def atomic_json(path, value):
    if path.name == "manifest.json":
        value.pop("journal_sha256", None)
        value["journal_sha256"] = digest(encoded(value))
    fd, name = tempfile.mkstemp(prefix=".pending-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(encoded(value) + b"\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def state_root(workspace):
    root = Path(workspace).resolve() / ".release-checkpoints"
    private_dir(root)
    return root


def inherited_lock(workspace):
    try:
        fd = int(os.environ.get("RELEASE_LOCK_FD", "-1"))
        st = os.fstat(fd)
        expected = (state_root(workspace) / "workspace.lock").stat()
        if (st.st_dev, st.st_ino) != (expected.st_dev, expected.st_ino):
            return False
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return True
    except (OSError, ValueError):
        return False


def locked(workspace, command):
    if inherited_lock(workspace):
        os.execvpe(command[0], command, os.environ)
    root = state_root(workspace)
    fd = os.open(root / "workspace.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise Refusal("another release owns this workspace; wait for its process tree to exit (never delete the lock file)")
    owner_path = root / "lock-owner.json"
    previous = json.loads(owner_path.read_text()) if owner_path.exists() else None
    if previous and previous.get("state") != "finished":
        raise Refusal("previous release exit is uncertain (detached VM/container writers may survive); finish its cleanup, then run release-state.py recover-lock " + str(Path(workspace).resolve()) + "; never delete workspace.lock")
    if previous:
        inspect_workspace_writers(workspace, previous, uncertain=False)
    os.set_inheritable(fd, True)
    environment = dict(os.environ, RELEASE_LOCK_FD=str(fd))
    owner = {"pid": os.getpid(), "started": time.time(), "state": "active", "run": uuid.uuid4().hex}
    atomic_json(owner_path, owner)
    # The child owns ordinary 022 artifact permissions even if its caller keeps
    # audit logs private with 077. mkstemp/open modes protect this module's files.
    child = subprocess.Popen(command, env=environment, pass_fds=(fd,), umask=0o022,
                             start_new_session=True)
    owner["pgid"] = child.pid
    atomic_json(owner_path, owner)
    # Some tools close unfamiliar FDs in their own children. An independent
    # reaper retains the lock until the entire release process group is gone,
    # including when this supervisor or its direct child is SIGKILLed.
    reaper = subprocess.Popen([sys.executable, str(Path(__file__).resolve()),
                               "_lock-reaper", str(child.pid)],
                              env=environment, pass_fds=(fd,), start_new_session=True,
                              stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL)
    def forward(signum, _frame):
        if child.poll() is None:
            os.killpg(child.pid, signum)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, forward)
    code = child.wait()
    reaper.wait()
    if 0 <= code < 128:
        atomic_json(owner_path, dict(owner, state="finished", ended=time.time(), exit_code=code))
    os.close(fd)
    return code if code >= 0 else 128 - code


def inspect_workspace_writers(workspace, owner, uncertain=True):
    """Read-only conservative cleanup proof; never stops processes/containers."""
    workspace = Path(workspace).resolve()
    if owner.get("pgid"):
        try:
            os.killpg(int(owner["pgid"]), 0)
        except ProcessLookupError:
            pass
        else:
            raise Refusal("previous release process group still exists; finish its cleanup before recovering the lock")
    # This recovery command and its invocation ancestors may have workspace
    # CWDs without being stale writers. Other idle terminals are conservatively
    # refused with instructions to leave that directory, never killed.
    rows = run(["ps", "-axo", "pid=,ppid=,uid=,comm="]).decode().splitlines()
    processes = {}
    for row in rows:
        pid, parent, uid, command = row.split(None, 3)
        if int(uid) == os.getuid():
            processes[int(pid)] = (int(parent), command)
    ancestors = {os.getpid()}
    current = os.getpid()
    while current in processes and processes[current][0] not in ancestors:
        current = processes[current][0]
        ancestors.add(current)
    for pid, (_, command) in processes.items():
        if pid not in ancestors and "qemu-system" in Path(command).name:
            raise Refusal("a QEMU VM is still running for this user; verify/stop owned release VMs before lock recovery")
    # A verified normal exit has no evidence that an unrelated idle shell or
    # code host became a release writer. The broad CWD inventory is justified
    # only for explicit recovery of an uncertain exit, never every new build.
    if uncertain:
        listing = run(["lsof", "-n", "-a", "-u", str(os.getuid()), "-d", "cwd", "-Fpn"]).decode()
        pid = None
        for line in listing.splitlines():
            if line.startswith("p"):
                pid = int(line[1:])
            elif line.startswith("n") and pid not in ancestors:
                cwd = Path(line[1:]).resolve()
                if cwd == workspace or cwd.is_relative_to(workspace):
                    raise Refusal("another process has a working directory in this workspace (PID " + str(pid) + "); finish cleanup or move idle terminals outside it")
    endpoint = run(["docker", "context", "inspect", "desktop-linux", "--format", "{{.Endpoints.docker.Host}}"]).decode().strip()
    if not endpoint.startswith("unix://"):
        raise Refusal("writer inventory requires the explicit local desktop-linux Docker context")
    container_ids = run(["docker", "--context", "desktop-linux", "ps", "--quiet", "--no-trunc"]).decode().split()
    for container in container_ids:
        if not re.fullmatch(r"[0-9a-f]{12,64}", container):
            raise Refusal("invalid Docker writer inventory")
        mounts = json.loads(run(["docker", "--context", "desktop-linux", "inspect", "--format", "{{json .Mounts}}", container]))
        for mount in mounts:
            if mount.get("Type") != "bind" or not mount.get("RW"):
                continue
            source = Path(mount["Source"]).resolve()
            if source == workspace or source.is_relative_to(workspace) or workspace.is_relative_to(source):
                raise Refusal("running Docker container has a writable workspace bind mount; finish owned-container cleanup before lock recovery")


def recover_lock(workspace):
    root = state_root(workspace)
    fd = os.open(root / "workspace.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Refusal("release lock is still held; recovery cannot bypass an active writer") from None
        path = root / "lock-owner.json"
        owner = json.loads(path.read_text()) if path.exists() else {}
        inspect_workspace_writers(workspace, owner)
        atomic_json(path, dict(owner, state="finished", recovered=time.time()))
        print("verified no recorded process-group, workspace-CWD, QEMU or writable Docker-bind writer; advisory lock state recovered")
    finally:
        os.close(fd)


def artifact(path, boundary=None, ancestry=frozenset()):
    path = Path(path).absolute()
    root = Path(boundary).resolve() if boundary else path.resolve()
    if path.is_symlink():
        if boundary is None:
            raise Refusal("top-level output must not be a symlink: " + str(path))
        try:
            resolved = path.resolve(strict=True)
        except OSError:
            raise Refusal("missing/unreadable output symlink target: " + str(path)) from None
        if not resolved.is_relative_to(root):
            raise Refusal("output symlink escapes its verified directory: " + str(path))
        return {"path": str(path), "symlink": os.readlink(path),
                "target": artifact(resolved, root, ancestry)}
    if path.is_dir():
        resolved = path.resolve(strict=True)
        if resolved in ancestry:
            raise Refusal("output directory contains a symlink cycle: " + str(path))
        entries = [artifact(p, root, ancestry | {resolved}) for p in sorted(path.iterdir())]
        def byte_count(item):
            return item.get("size", item.get("bytes", byte_count(item["target"]) if "target" in item else 0))
        size = sum(byte_count(item) for item in entries)
        if boundary is None and not size:
            raise Refusal("empty output directory: " + str(path))
        return {"path": str(path), "entries": entries, "bytes": size,
                "mode": stat.S_IMODE(path.stat().st_mode)}
    try:
        st = path.stat()
        if not stat.S_ISREG(st.st_mode):
            raise Refusal("output is not a regular file: " + str(path))
        sha = hashlib.sha256()
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                sha.update(chunk)
        return {"path": str(path), "sha256": sha.hexdigest(), "size": st.st_size,
                "mode": stat.S_IMODE(st.st_mode)}
    except OSError:
        raise Refusal("missing/unreadable pinned output: " + str(path)) from None


def outputs(paths):
    result = [artifact(p) for p in paths]
    if not result or any(item.get("size") == 0 for item in result):
        raise Refusal("a completed stage requires explicit nonempty outputs")
    return result


def retained_outputs(directory, name, paths):
    """Artifact scratch can be cleaned by a later platform. Keep exact bytes.

    Original destinations are pinned by the stage call in run.sh, not trusted
    from a manifest. Reuse compares the declared list before restoring anything.
    """
    original = outputs(paths)
    parent = directory / "artifacts"
    private_dir(parent)
    target = Path(tempfile.mkdtemp(prefix=name + "-", dir=parent))
    copies = []
    for index, source in enumerate(paths):
        destination = target / str(index)
        if Path(source).is_dir():
            shutil.copytree(source, destination, symlinks=True)
        else:
            shutil.copy2(source, destination)
        copies.append(str(destination))
    # Reject concurrent changes during capture, even in generated output trees.
    if outputs(paths) != original:
        raise Refusal("stage outputs changed while retaining their provenance")
    retained = outputs(copies)
    def content(value):
        if isinstance(value, list):
            return [content(item) for item in value]
        if isinstance(value, dict):
            return {key: content(item) for key, item in value.items() if key != "path"}
        return value
    if content(retained) != content(original):
        raise Refusal("retained artifact copy does not match its verified source bytes/modes")
    return retained


def declared_paths(paths):
    # Do not resolve a changed symlink away before the safety checks below.
    return [os.path.abspath(p) for p in paths]


def restore_outputs(record, declared):
    declared = declared_paths(declared)
    if declared != record["destinations"]:
        raise Refusal("stage's declared output paths differ from the sealed contract")
    verify_outputs(record)
    for output, destination in zip(record["outputs"], declared):
        source = Path(output["path"])
        dest = Path(destination)
        if any(parent.is_symlink() for parent in dest.parents):
            raise Refusal("generated output has a symlink ancestor: " + destination)
        if dest.is_symlink():
            raise Refusal("generated output destination is now a symlink: " + destination)
        # Never follow an injected symlink in the existing generated tree.
        if dest.is_dir():
            for item in dest.rglob("*"):
                if item.is_symlink() and not item.resolve().is_relative_to(dest.resolve()):
                    raise Refusal("generated output tree contains an escaping symlink")
        dest.parent.mkdir(parents=True, exist_ok=True)
        # Replace the exact canonical generated output, not a merge: otherwise
        # stale extra files could leak into later publication globs. Stage in
        # the same filesystem, keeping the old output intact until copy/hash
        # verification succeeds. Never hardlink mutable build scratch.
        temporary = Path(tempfile.mkdtemp(prefix=".release-restore-", dir=dest.parent))
        staged = temporary / "new"
        displaced = temporary / "previous"
        try:
            if source.is_dir():
                shutil.copytree(source, staged, symlinks=True)
            else:
                shutil.copy2(source, staged)
            if artifact_content(artifact(staged)) != artifact_content(output):
                raise Refusal("restored artifact copy failed byte/mode verification")
            if dest.exists():
                os.replace(dest, displaced)
            try:
                os.replace(staged, dest)
            except OSError:
                if displaced.exists():
                    os.replace(displaced, dest)
                raise
            # Only this invocation's exact displaced generated output is
            # removed; immutable release caches are never pruned here.
            if displaced.is_dir():
                shutil.rmtree(displaced)
            elif displaced.exists():
                displaced.unlink()
            temporary.rmdir()
        except BaseException:
            # Leave any incomplete copy private/recoverable for diagnosis.
            raise


def artifact_content(value):
    if isinstance(value, list):
        return [artifact_content(item) for item in value]
    if isinstance(value, dict):
        return {key: artifact_content(item) for key, item in value.items() if key != "path"}
    return value


def git(repo, *args):
    return run(["git", "-C", str(repo), *args])


def repository_snapshot(repo, scope=None):
    # Git's worktree diff catches tracked edits without rehashing gigabytes of
    # clean LFS assets on every stage. Index and untracked source are separate
    # inputs. Submodules are independently pinned below, not inferred from a
    # parent gitlink. Ignored generated go.sum files are added explicitly.
    suffix = ["--", scope] if scope else []
    head = git(repo, "rev-parse", "HEAD").decode().strip()
    tracked = git(repo, "ls-files", "--stage", "-z", *suffix)
    diff = git(repo, "diff", "--no-ext-diff", "--binary", "--ignore-submodules=all", "HEAD", *suffix)
    other = git(repo, "ls-files", "--others", "--exclude-standard", "-z", *suffix).split(b"\0")
    untracked = []
    for name in other:
        if name:
            file = repo / os.fsdecode(name)
            if file.is_file() or file.is_symlink():
                untracked.append(artifact(file))
    return {"head": head, "index": digest(tracked), "diff": digest(diff), "untracked": untracked}


def repositories(workspace):
    workspace = Path(workspace).resolve()
    build = workspace / "build"
    repos = {"build": build}
    for name in ("config", "vault", "release", "localizations", "gvisor", "sdk", "mmm",
                 "goidenticons", "connect", "glog", "userwireguard", "examples", "xops"):
        repos[name] = workspace / name
    lines = git(build, "config", "--file", ".gitmodules", "--get-regexp", r"^submodule\..*\.path$")
    for line in lines.decode().splitlines():
        name = line.split(" ", 1)[1]
        repos["build/" + name] = build / name
    return repos


def source_snapshot(workspace):
    def inspect(item):
        name, repo = item
        return name, repository_snapshot(repo, "builder" if name == "xops" else None)
    with ThreadPoolExecutor(max_workers=4) as pool:
        result = dict(pool.map(inspect, repositories(workspace).items()))
    build = Path(workspace) / "build"
    graph = []
    for name in ("sdk/build/go.sum", "sdk/cgo/go.sum", "sdk/js/go.sum"):
        graph.append(artifact(build / name))
    result["generated-module-graph"] = graph
    result["deployment-inputs"] = deployment_snapshot(workspace)
    return result


def deployment_snapshot(workspace):
    workspace = Path(workspace).resolve()
    path = workspace / "warp.json"
    settings = json.loads(path.read_text()) if path.exists() else {}
    if not isinstance(settings, dict):
        raise Refusal("invalid host Warp settings")
    selected = {"dockerNamespace": settings.get("dockerNamespace")}
    # Warp JSON overrides the environment. Pin the effective selections and
    # refuse alternate source roots that repository_snapshot does not cover.
    # Docker login username/token are refreshable auth, never journaled.
    for key, variable, default in (("configHome", "WARP_CONFIG_HOME", "config"),
                                    ("vaultHome", "WARP_VAULT_HOME", "vault"),
                                    ("siteHome", "WARP_SITE_HOME", "site")):
        selected[key] = str(Path(settings.get(key) or os.environ.get(variable) or workspace / default).resolve())
        # Canonical workspace roots may be symlinks. The repository snapshot
        # follows them; hashing the resolved selection also pins their target.
        if default in ("config", "vault") and Path(selected[key]) != (workspace / default).resolve():
            raise Refusal("alternate " + default + " root is not covered by release provenance; select the canonical workspace root before a new release")
    version_home = Path(os.environ.get("WARP_VERSION_HOME") or workspace).resolve()
    selected["versionHome"] = str(version_home)
    version_file = version_home / "version.json"
    return {"selection_sha256": digest(encoded(selected)),
            "version_sha256": digest(version_file.read_bytes()) if version_file.exists() else None}


def environment_snapshot():
    # The canary inputs are explicit copies/pins outside the config tree. A
    # resume must not silently select different bytes at the same pathname.
    canary_inputs = {}
    for key in ("ARIN_PREVIOUS_DATABASE_FILE", "ARIN_CANARY_DECISION_FILE"):
        path = os.environ.get(key, "")
        if path:
            try:
                canary_inputs[key] = digest(Path(path).read_bytes())
            except OSError:
                # An unreadable decision intentionally pauses ARIN; preserve
                # that outcome while still detecting a newly readable copy.
                canary_inputs[key] = "unavailable"
    return {"canary_inputs": canary_inputs, "options": {key: os.environ.get(key, "") for key in OPTIONS},
            "publishers": {key: bool(os.environ.get(key, "").strip()) for key in AUTH}}


def go_environment_snapshot(workspace):
    workspace = Path(workspace).resolve()
    # The canonical directory avoids per-stage CWD differences. Hash all
    # values: proxy configuration may embed credentials and must not be saved.
    values = json.loads(run(["go", "env", "-json", *GO_ENV_KEYS], cwd=workspace / "build"))
    if set(values) != set(GO_ENV_KEYS) or any(not isinstance(v, str) for v in values.values()):
        raise Refusal("effective Go environment probe returned an incomplete identity")
    files = {}
    if values["GOENV"] not in ("", "off"):
        config = Path(values["GOENV"])
        files["GOENV"] = digest(config.read_bytes()) if config.exists() else None
    if values["GOWORK"] not in ("", "off"):
        work = Path(values["GOWORK"]).resolve()
        if not work.is_relative_to(workspace):
            raise Refusal("external GOWORK is unsupported for a sealed release; use GOWORK=off or a pinned workspace-local go.work before starting a new release")
        files["GOWORK"] = digest(work.read_bytes())
        sums = work.with_name(work.name + ".sum")
        files["GOWORK.sum"] = digest(sums.read_bytes()) if sums.exists() else None
    return {"effective_sha256": digest(encoded(values)), "files": files}


def tool_snapshot(workspace=None):
    result = {}
    for command in TOOLS:
        resolved = run(["/bin/sh", "-c", 'command -v "$1"', "release-tools", command[0]]).decode().strip()
        version = run(list(command), combined=True)
        if not version.strip():
            raise Refusal("tool version probe returned no identity: " + command[0])
        result[command[0]] = {"path": resolved, "version": digest(version),
                              "executable": artifact(Path(resolved).resolve())}
    # The NDK's revision and SDK metadata are inputs, never installed on resume.
    ndk = os.environ.get("ANDROID_NDK_HOME")
    if ndk:
        result["ndk"] = artifact(Path(ndk) / "source.properties")
    if any(command[0] == "go" for command in TOOLS):
        result["go-environment"] = go_environment_snapshot(workspace or os.environ["WARP_HOME"])
    return result


def candidate_gate_snapshot(workspace, version):
    result = {}
    directory = state_root(workspace) / version
    declared = {
        "web": [str(Path(workspace).resolve() / "build/web/web/build/preview.ur.xyz")],
        "mmm": [str(Path(workspace).resolve() / "mmm/mmm/build/preview.ur.io"),
                str(Path(workspace).resolve() / "mmm/mmm/build/lag.dev")],
    }
    for name, paths in declared.items():
        receipt = json.loads((directory / (name + "-candidate.json")).read_text())
        if receipt.get("gate") != name + " make check (WEB_BUILD_CHECKS=1)" or not receipt.get("sources"):
            raise Refusal("missing or invalid passed " + name + " candidate-gate evidence; preparation is not resumable")
        if [item["path"] for item in receipt.get("outputs", [])] != paths:
            raise Refusal("candidate-gate outputs differ from the canonical site contract")
        verify_outputs(receipt)
        result[name] = {"outputs": retained_outputs(directory, "candidate-" + name, paths),
                        "receipt_sha256": digest(encoded(receipt))}
    return result


def published_refs_snapshot(workspace, version):
    build = Path(workspace).resolve() / "build"
    result = {}
    variants = [version.rsplit("-", 1)[0] + "-" + str(int(version.rsplit("-", 1)[1]) + offset)
                for offset in (0, 2, 3)]
    for component in ("", *TAGGED_COMPONENTS):
        repo = build / component
        refs = {}
        for variant in variants if not component else [version]:
            tag = "refs/tags/v" + variant
            commit = git(repo, "rev-parse", "--verify", tag + "^{commit}").decode().strip()
            # SDK intentionally commits generated nested lockfiles after its
            # immutable root-module tag. It must still descend from that tag.
            git(repo, "merge-base", "--is-ancestor", commit, "HEAD")
            refs[tag] = commit
        result["build" + ("/" + component if component else "")] = {
            "remote": digest(git(repo, "remote", "get-url", "origin")), "refs": refs}
    verify_published_refs(workspace, result)
    return result


def verify_published_refs(workspace, published):
    def verify(item):
        name, expected = item
        repo = Path(workspace).resolve() / name
        if digest(git(repo, "remote", "get-url", "origin")) != expected["remote"]:
            raise Refusal("release remote changed: " + name)
        wanted = [ref for tag in expected["refs"] for ref in (tag, tag + "^{}")]
        data = git(repo, "ls-remote", "--exit-code", "--tags", "origin", *wanted)
        actual = dict(line.split()[::-1] for line in data.decode().splitlines())
        for tag, commit in expected["refs"].items():
            if actual.get(tag + "^{}", actual.get(tag)) != commit:
                raise Refusal("published release tag changed or disappeared: " + name + " " + tag +
                              "; do not reuse this release or overwrite the remote tag")
    # Read-only exact refs, bounded per Git request, no fetch/checkout/history
    # walks. Fresh seal/resume validates once; stage commands reuse that pass.
    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(verify, published.items()))


def load_manifest(workspace, version):
    if not VERSION.fullmatch(version):
        raise Refusal("resume requires the exact external release version (YYYY.M.D-code)")
    directory = state_root(workspace) / version
    private_dir(directory)
    path = directory / "manifest.json"
    try:
        st = path.lstat()
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or st.st_mode & 0o077:
            raise Refusal("manifest must be a private regular file owned by this user")
        value = json.loads(path.read_text())
    except (OSError, ValueError):
        raise Refusal("no valid sealed manifest for " + version + "; start a new full release") from None
    if value.get("schema") != SCHEMA or value.get("version") != version or value.get("workspace") != str(Path(workspace).resolve()):
        raise Refusal("checkpoint schema, version, or workspace mismatch; start a new full release")
    checksum = value.pop("journal_sha256", None)
    if checksum != digest(encoded(value)):
        raise Refusal("checkpoint journal checksum mismatch; preserve the journal and start a new release")
    value["journal_sha256"] = checksum
    cache = (directory / "artifacts").resolve()
    for record in [value["candidate_evidence"], *[gate["outputs"] for gate in value["candidate_gates"].values()], *[stage.get("outputs", []) for stage in value["stages"].values()]]:
        for item in record:
            if not Path(item["path"]).resolve().is_relative_to(cache):
                raise Refusal("checkpoint artifact path escapes this release's private cache")
    return path, value


def validate_inputs(workspace, value, tools=False):
    if environment_snapshot() != value["environment"]:
        raise Refusal("release options/publisher selection changed; restore the original options or start a new full release")
    actual = source_snapshot(workspace)
    changed = sorted(key for key in set(actual) | set(value["sources"]) if actual.get(key) != value["sources"].get(key))
    if changed:
        raise Refusal("pinned source/config/vault/module graph changed: " + ", ".join(changed) +
                      "; preserve edits and start a new full release; unjournaled mutations are never adopted")
    if tools and tool_snapshot(workspace) != value["toolchain"]:
        raise Refusal("pinned toolchain changed; restore it or start a new full release")
    return actual


def verify_outputs(record):
    expected = record["outputs"]
    if outputs([item["path"] for item in expected]) != expected:
        raise Refusal("completed output changed or is missing; restore the exact bytes/modes or start a new full release")


def allowed_transition(workspace, before, after, transition, version, origin):
    if transition == "ip-databases":
        allowed = {"config": ["all/mmdb/" + version.replace("-", "+", 1),
                              "all/arindb/" + version.replace("-", "+", 1)]}
    elif transition == "onboarding":
        allowed = {"config": ["main/onboarding.yml"]}
    elif transition in ("fdroid-prepare", "fdroid-restore"):
        allowed = {"build": ["android"], "build/android": ["app/settings.gradle", "app/app/build.gradle"]}
    else:
        raise Refusal("unknown source transition")
    for name in set(before) | set(after):
        if before.get(name) == after.get(name):
            continue
        if name not in allowed:
            raise Refusal("unexpected source mutation during " + transition + ": " + name)
        repo = Path(workspace) / name
        if before[name]["diff"] != digest(b""):
            raise Refusal("source transition started with tracked edits: " + name)
        if after[name]["diff"] != digest(b"") or after[name]["untracked"] != before[name]["untracked"]:
            raise Refusal("source transition left uncommitted or untracked changes: " + name)
        if name != "build/android":
            parent = git(repo, "rev-parse", after[name]["head"] + "^").decode().strip()
            if parent != before[name]["head"]:
                raise Refusal("source transition incorporated an unexpected commit/rebase: " + name)
        paths = git(repo, "diff", "--name-only", "--no-renames", before[name]["head"], after[name]["head"]).decode().splitlines()
        for path in paths:
            if not any(path == prefix or path.startswith(prefix + "/") for prefix in allowed[name]):
                raise Refusal("source transition changed an unrelated path: " + name + "/" + path)
    if transition == "fdroid-prepare":
        repo = Path(workspace) / "build/android"
        for name in allowed["build/android"]:
            original = git(repo, "show", before["build/android"]["head"] + ":" + name)
            expected = re.sub(rb".*/\* *build: *google *\*/.*", b"/*ungoogled*/", original)
            actual = git(repo, "show", after["build/android"]["head"] + ":" + name)
            if expected != actual:
                raise Refusal("F-Droid transition is not the exact reviewed Google-dependency transformation")
    if transition == "fdroid-restore" and after["build/android"]["head"] != origin["build/android"]["head"]:
        raise Refusal("F-Droid restore did not return to the original sealed Android commit")
    if transition.startswith("fdroid-"):
        pin = git(Path(workspace) / "build", "rev-parse", after["build"]["head"] + ":android").decode().strip()
        if pin != after["build/android"]["head"]:
            raise Refusal("build Android gitlink does not match the observed release transition")
    if transition == "onboarding":
        repo = Path(workspace) / "config"
        previous = git(repo, "show", before["config"]["head"] + ":main/onboarding.yml").decode()
        current = git(repo, "show", after["config"]["head"] + ":main/onboarding.yml")
        # Use the pinned generator's own pure merge instead of maintaining a
        # second YAML/formatting policy or accepting arbitrary file edits.
        script = '''import fs from "node:fs"; import {pathToFileURL} from "node:url";
const {mergeTemplates}=await import(pathToFileURL(process.argv[1]));
const {before,templates}=JSON.parse(fs.readFileSync(0,"utf8"));
process.stdout.write(mergeTemplates(before,templates));'''
        templates = json.loads((Path(workspace) / "mmm/onboarding/dist/brevo-templates.json").read_text())
        result = subprocess.run(["node", "--input-type=module", "-e", script,
                                 str(Path(workspace) / "mmm/onboarding/lib/config.mjs")],
                                input=encoded({"before": previous, "templates": templates}),
                                capture_output=True, timeout=30)
        if result.returncode or result.stdout != current:
            raise Refusal("onboarding config differs from the exact pinned template-ID merge")


def failure_hint(workspace, value):
    """Describe recorded intent without probing providers or changing state."""
    uncertain = []
    for name, record in value["effects"].items():
        if not STAGE.fullmatch(name) or record.get("state") not in ("uncertain", "complete"):
            raise Refusal("invalid external-effect record; preserve the release journal")
        if record["state"] == "uncertain":
            uncertain.append(name)
    version = value["version"]
    if uncertain:
        return ("Release " + version + " stopped with unresolved publication intent: " +
                ", ".join(sorted(uncertain)) + ".\n"
                "Preserve logs and receipts. Investigate the exact version with the owning provider/operator; "
                "do not retry publication, clear intent or mark success. A stopped host or absent tag does not "
                "establish that a daemon-owned job cannot publish later.\n"
                "If authoritative reconciliation is unavailable, start a new full release after owned cleanup.")
    return ("Release " + version + " stopped. If the failure was an external dependency and the sealed inputs "
            "are unchanged, restore that dependency, finish owned cleanup, then run: " +
            str(Path(workspace).resolve() / "xops/builder/build.sh") + " --resume " + version + "\n"
            "Resume verifies sealed source/config/vault, options, toolchain and every retained artifact. "
            "Product source changes require a new full release.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("locked", "recover-lock", "check-lock", "check-inputs", "candidate", "seal", "validate", "restore-prepared", "sdk-outputs", "begin", "complete", "transition", "status", "stage-state", "failure-hint", "effect-begin", "effect-complete"))
    parser.add_argument("workspace")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    rest = args.arguments
    if args.command == "recover-lock":
        recover_lock(args.workspace)
        return 0
    if args.command == "locked":
        if rest[:1] == ["--"]:
            rest = rest[1:]
        return locked(args.workspace, rest)
    if args.command == "check-lock":
        return 0 if inherited_lock(args.workspace) else 1
    if not inherited_lock(args.workspace):
        raise Refusal("checkpoint access requires the release workspace lock")
    if args.command == "check-inputs":
        go_environment_snapshot(args.workspace)
        deployment_snapshot(args.workspace)
        return 0
    if args.command == "candidate":
        name = rest.pop(0)
        if name not in ("web", "mmm"):
            raise Refusal("unknown candidate gate")
        root = state_root(args.workspace)
        receipt = root / ("candidate-" + name + "-" + str(time.time_ns()) + ".json")
        atomic_json(receipt, {"gate": name + " make check (WEB_BUILD_CHECKS=1)",
                             "completed": time.time(), "outputs": outputs(rest),
                             "sources": {key: repository_snapshot(repo, "builder" if key == "xops" else None)
                                         for key, repo in repositories(args.workspace).items()}})
        print(receipt)
        return 0
    if not rest:
        raise Refusal("exact release version required")
    version = rest.pop(0)
    if not VERSION.fullmatch(version):
        raise Refusal("invalid exact release version")
    if args.command == "seal":
        directory = state_root(args.workspace) / version
        private_dir(directory)
        path = directory / "manifest.json"
        if path.exists():
            raise Refusal("release already has a seal; use --resume " + version)
        evidence = retained_outputs(directory, "prepared", rest)
        value = {"schema": SCHEMA, "version": version, "workspace": str(Path(args.workspace).resolve()),
                 "sealed": time.time(), "sources": source_snapshot(args.workspace),
                 "environment": environment_snapshot(), "toolchain": tool_snapshot(args.workspace),
                 "candidate_evidence": evidence, "stages": {}, "effects": {}, "transitions": []}
        value["published_refs"] = published_refs_snapshot(args.workspace, version)
        value["candidate_gates"] = candidate_gate_snapshot(args.workspace, version)
        value["origin_sources"] = value["sources"]
        value["prepared_destinations"] = declared_paths(rest)
        value["seal"] = digest(encoded(value))
        atomic_json(path, value)
        print("release checkpoint sealed: " + version)
        return 0
    path, value = load_manifest(args.workspace, version)
    if args.command == "status":
        print(json.dumps({key: value[key] for key in ("version", "sealed", "stages", "effects")}, indent=2))
        return 0
    if args.command == "failure-hint":
        print(failure_hint(args.workspace, value))
        return 0
    if args.command == "stage-state":
        if len(rest) != 1 or not STAGE.fullmatch(rest[0]):
            raise Refusal("one valid stage name is required")
        observed = value["stages"].get(rest[0], {}).get("state", "pending")
        if observed not in ("pending", "running", "complete"):
            raise Refusal("invalid stage state")
        print(observed)
        return 0
    if args.command == "validate":
        validate_inputs(args.workspace, value, tools=True)
        verify_published_refs(args.workspace, value["published_refs"])
        verify_outputs({"outputs": value["candidate_evidence"]})
        for gate in value["candidate_gates"].values():
            verify_outputs(gate)
        for record in value["stages"].values():
            if record["state"] == "complete":
                verify_outputs(record)
        print("verified immutable release " + version)
        return 0
    if args.command == "restore-prepared":
        validate_inputs(args.workspace, value, tools=True)
        restore_outputs({"outputs": value["candidate_evidence"], "destinations": value["prepared_destinations"]}, rest)
        return 0
    if args.command == "sdk-outputs":
        sdk = Path(args.workspace).resolve() / "build/sdk"
        plan_path = sdk / "packaging/release/plan.json"
        index = value["prepared_destinations"].index(str(plan_path))
        if artifact_content(artifact(plan_path)) != artifact_content(value["candidate_evidence"][index]):
            raise Refusal("SDK publisher plan differs from the sealed plan")
        plan = json.loads(plan_path.read_text())
        if plan.get("version") != version or not isinstance(plan.get("registries"), dict):
            raise Refusal("invalid exact-version SDK publisher plan")
        enabled = plan["registries"]
        selected = [plan_path]
        if rest == ["mobile"]:
            if enabled.get("swift") or enabled.get("cocoapods"):
                selected += [sdk / "swift/dist"]
        elif rest == ["desktop"]:
            languages = {"pypi": "python", "nuget": "csharp", "rubygems": "ruby", "maven": "java", "crates": "rust", "conan": "cgo", "vcpkg": "cgo"}
            active = sorted({language for registry, language in languages.items() if enabled.get(registry)})
            if active:
                selected += [sdk / "packaging/release/native.json", sdk / "cgo/build/darwin", sdk / "cgo/build/windows"]
                selected += [sdk / language / "dist" for language in active]
            if "cgo" in active or "rust" in active:
                selected += [sdk / "packaging/release/assets"]
        else:
            raise Refusal("invalid SDK checkpoint phase")
        print("\n".join(map(str, selected)))
        return 0
    if not rest or not STAGE.fullmatch(rest[0]):
        raise Refusal("invalid stage identifier")
    name = rest.pop(0)
    if args.command == "transition":
        actual = source_snapshot(args.workspace)
        if environment_snapshot() != value["environment"]:
            raise Refusal("release options changed during source transition")
        allowed_transition(args.workspace, value["sources"], actual, name, version, value["origin_sources"])
        value["transitions"].append({"name": name, "time": time.time(), "before": digest(encoded(value["sources"])), "after": digest(encoded(actual))})
        value["sources"] = actual
        atomic_json(path, value)
        return 0
    validate_inputs(args.workspace, value, tools=args.command in ("begin", "complete"))
    if args.command.startswith("effect-"):
        record = value["effects"].get(name)
        if args.command == "effect-begin":
            if record:
                print(record["state"])
                return 0
            value["effects"][name] = {"state": "uncertain", "started": time.time()}
        else:
            if not record:
                raise Refusal("external effect completed without durable intent")
            record.update(state="complete", completed=time.time())
        atomic_json(path, value)
        return 0
    record = value["stages"].get(name)
    if args.command == "begin":
        if record and record["state"] == "complete":
            restore_outputs(record, rest)
            print("reuse " + name + ": verified hashes, modes and sealed inputs", file=sys.stderr)
            return 10
        if not rest:
            raise Refusal("stage must declare concrete output paths before it starts")
        value["stages"][name] = {"state": "running", "started": time.time(), "seal": value["seal"],
                                  "destinations": declared_paths(rest)}
        atomic_json(path, value)
        print("stage " + name + ": started", file=sys.stderr)
        return 0
    if not record or record["state"] != "running":
        raise Refusal("stage completion without a durable start: " + name)
    if declared_paths(rest) != record["destinations"]:
        raise Refusal("completed stage changed its declared output contract")
    record.update(state="complete", completed=time.time(), outputs=retained_outputs(path.parent, name, rest))
    atomic_json(path, value)
    print("stage " + name + ": complete (" + str(round(record["completed"] - record["started"])) + "s)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "_lock-reaper":
        while True:
            try:
                os.killpg(int(sys.argv[2]), 0)
            except ProcessLookupError:
                sys.exit(0)
            time.sleep(0.25)
    try:
        sys.exit(main())
    except (Refusal, KeyError, TypeError, ValueError, subprocess.TimeoutExpired, OSError, RuntimeError) as error:
        print("release checkpoint refused: " + str(error), file=sys.stderr)
        sys.exit(65)
