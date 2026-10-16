"""Hermetic release recovery tests: filesystem/Git are real; providers are fake.

Run: python3 -m unittest -v test_release_recovery.py
No test reads the builder's credentials or contacts a publication provider.
"""
import contextlib
import fcntl
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import signal
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest import mock


HERE = Path(__file__).resolve().parent


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


state = module("state_under_test", "release-state.py")
publication = module("publication_under_test", "release-publication.py")
VERSION = "2026.10.15-123456789"


class Checkpoints(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release-state-test-")
        self.addCleanup(self.temporary.cleanup)
        self.workspace = Path(self.temporary.name).resolve()
        self.sources = {"build": {"head": "a" * 40}, "mmm": {"head": "b" * 40}}
        self.env = mock.patch.dict(os.environ, {"PATH": os.environ["PATH"]}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.source_mock = mock.patch.object(state, "source_snapshot", side_effect=lambda _: self.sources)
        self.source_mock.start()
        self.addCleanup(self.source_mock.stop)
        tool_patch = mock.patch.object(state, "tool_snapshot", return_value={"java": "21"})
        self.tool_mock = tool_patch.start()
        self.addCleanup(tool_patch.stop)
        remote_patch = mock.patch.object(state, "published_refs_snapshot", return_value={})
        remote_patch.start()
        self.addCleanup(remote_patch.stop)
        candidate_patch = mock.patch.object(state, "candidate_gate_snapshot", return_value={})
        candidate_patch.start()
        self.addCleanup(candidate_patch.stop)
        root = state.state_root(self.workspace)
        self.fd = os.open(root / "workspace.lock", os.O_CREAT | os.O_RDWR, 0o600)
        self.addCleanup(os.close, self.fd)
        fcntl.flock(self.fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        os.environ["RELEASE_LOCK_FD"] = str(self.fd)
        self.evidence = self.workspace / "gate.json"
        self.evidence.write_text('{"passed":true}')
        self.invoke("seal", self.evidence)

    def invoke(self, command, *arguments):
        with mock.patch.object(sys, "argv", ["release-state", command, str(self.workspace), VERSION, *map(str, arguments)]), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return state.main()

    def manifest(self):
        return state.load_manifest(self.workspace, VERSION)[1]

    def completed(self):
        target = self.workspace / "generated" / "signed.app"
        self.assertEqual(self.invoke("begin", "apple", target), 0)
        target.mkdir(parents=True)
        (target / "CodeResources").write_bytes(b"signed resources")
        (target / "CodeResources").chmod(0o644)
        (target / "binary").write_bytes(b"executable")
        (target / "binary").chmod(0o755)
        (target / "current").symlink_to("binary")
        self.assertEqual(self.invoke("complete", "apple", target), 0)
        return target

    def checkpoint_boundary(self):
        # Execute the actual canonical image-stage block and shell helpers.
        # Git/toolchain inspection is the only state-manager fake here; the
        # inherited kernel lock, effect journal, atomic writes and caches are
        # real. Existing real-source/entrypoint fixtures cover input inspection.
        boundary = self.workspace / "checkpoint-boundary.py"
        boundary.write_text('''import importlib.util,json,sys
from pathlib import Path
spec=importlib.util.spec_from_file_location("image_state",sys.argv[1])
state=importlib.util.module_from_spec(spec)
spec.loader.exec_module(state)
expected=json.loads((Path(sys.argv[3])/".release-checkpoints"/sys.argv[4]/"manifest.json").read_text())
state.source_snapshot=lambda _:expected["sources"]
state.environment_snapshot=lambda:expected["environment"]
state.tool_snapshot=lambda *args:expected["toolchain"]
sys.argv=sys.argv[1:]
sys.exit(state.main())
''')
        return boundary

    def run_image_stage(self, service, outcome):
        self.checkpoint_boundary()
        source = (HERE / "run.sh").read_text()
        match = re.search(r'(?ms)^if release_build image-' + re.escape(service) + r' .*?^builder_message "service ' + re.escape(service) + r' .*?\n', source)
        self.assertIsNotNone(match, service)
        body = r'''
source "$BUILD_HOME/all/release-checkpoints.zsh"
python3() { "$FIXTURE_PYTHON" -B "$WARP_HOME/checkpoint-boundary.py" "$@"; }
builder_message() { return 0; }
error_trap() { local result=$?; if (( result != 0 )); then exit "$result"; fi; }
docker() {
    print -r -- "inspect-$FIXTURE_SERVICE" >> "$WARP_HOME/image-events"
    if [[ -f "$WARP_HOME/provider-$FIXTURE_SERVICE.json" ]]; then
        /bin/cat "$WARP_HOME/provider-$FIXTURE_SERVICE.json"
    else
        print -u2 -- 'manifest unknown'
        return 1
    fi
}
warpctl() {
    [[ "$1" = build || "$1" = import ]] || return 99
    print -r -- "publish-$FIXTURE_SERVICE" >> "$WARP_HOME/image-events"
    if [[ "$FIXTURE_OUTCOME" = killed ]]; then
        # The host runner dies after submitting work; the fake remote job has
        # not made its tag visible yet and can still finish later.
        kill -KILL $$
        return 137
    fi
    print -r -- '{"manifest":"verified-original"}' > "$WARP_HOME/provider-$FIXTURE_SERVICE.json"
}
'''
        environment = dict(os.environ, WARP_HOME=str(self.workspace), BUILD_HOME=str(HERE.parent),
                           EXTERNAL_WARP_VERSION=VERSION, BUILD_ENV="main", GO_MOD_SUFFIX="",
                           FIXTURE_PYTHON=sys.executable, FIXTURE_SERVICE=service, FIXTURE_OUTCOME=outcome)
        return subprocess.run(["/bin/zsh", "-c", body + match[0]], env=environment,
                              pass_fds=(self.fd,), capture_output=True, text=True, timeout=15)

    def test_canary_pin_and_decision_are_immutable_resume_inputs(self):
        for key in ("ARIN_PREVIOUS_DATABASE_FILE", "ARIN_CANARY_DECISION_FILE"):
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: str(self.workspace / key)}):
                with self.assertRaises(state.Refusal):
                    self.invoke("validate")
                unavailable = state.environment_snapshot()
                path = Path(os.environ[key])
                path.write_bytes(b"first canary input")
                first = state.environment_snapshot()
                self.assertNotEqual(first, unavailable)
                path.write_bytes(b"replacement canary input")
                self.assertNotEqual(state.environment_snapshot(), first)
                self.assertNotIn("replacement canary input", json.dumps(state.environment_snapshot()))

    def test_paused_arin_canonical_stage_completes_and_resumes_geolite_alone(self):
        self.checkpoint_boundary()
        source = (HERE / "run.sh").read_text()
        stage = re.search(r'(?ms)^if release_build ip-databases .*?^fi\n', source)
        self.assertIsNotNone(stage)
        body = r'''
source "$BUILD_HOME/all/release-checkpoints.zsh"
python3() { "$FIXTURE_PYTHON" -B "$WARP_HOME/checkpoint-boundary.py" "$@"; }
error_trap() { local result=$?; if (( result != 0 )); then exit "$result"; fi; }
refresh_ip_databases() {
    print refreshed >> "$WARP_HOME/refresh-events"
    mkdir -p "$BUILD_OUT/ip-databases/release-$WARP_VERSION/mmdb"
    print geolite > "$BUILD_OUT/ip-databases/release-$WARP_VERSION/mmdb/GeoLite2-City.mmdb"
    print 'ARIN paused: no current baseline' > "$BUILD_OUT/ip-databases/release-$WARP_VERSION/summary.txt"
}
'''
        environment = dict(os.environ, WARP_HOME=str(self.workspace), BUILD_HOME=str(HERE.parent),
                           BUILD_OUT=str(self.workspace / "out"), WARP_VERSION=VERSION,
                           EXTERNAL_WARP_VERSION=VERSION, FIXTURE_PYTHON=sys.executable)
        def run():
            return subprocess.run(["/bin/zsh", "-c", body + stage[0]], env=environment,
                                  pass_fds=(self.fd,), capture_output=True, text=True, timeout=15)
        first = run()
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(self.manifest()["stages"]["ip-databases"]["state"], "complete")
        artifact = self.workspace / "out" / "ip-databases" / ("release-" + VERSION)
        self.assertFalse((artifact / "arindb").exists())
        shutil.rmtree(artifact)
        resumed = run()
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        self.assertIn("reuse ip-databases: verified", resumed.stderr)
        self.assertEqual((artifact / "mmdb" / "GeoLite2-City.mmdb").read_text(), "geolite\n")
        self.assertEqual((self.workspace / "refresh-events").read_text().splitlines(), ["refreshed"])

    def test_interrupted_canonical_image_stage_never_retries_an_absent_pending_push(self):
        first = self.run_image_stage("api", "killed")
        self.assertEqual(first.returncode, -signal.SIGKILL, first.stdout + first.stderr)
        self.assertEqual(self.manifest()["effects"]["image-api-publication"]["state"], "uncertain")
        self.assertEqual(self.manifest()["stages"]["image-api"]["state"], "running")
        self.assertFalse((self.workspace / "provider-api.json").exists())
        retry = self.run_image_stage("api", "success")
        self.assertEqual(retry.returncode, 65, retry.stdout + retry.stderr)
        self.assertIn("BuildKit job may still publish", retry.stderr)
        events = (self.workspace / "image-events").read_text().splitlines()
        self.assertEqual(events.count("inspect-api"), 2)  # Both reads observed absence.
        self.assertEqual(events.count("publish-api"), 1)  # Never a second submission.
        # A later remote completion must not become permission to overwrite it.
        (self.workspace / "provider-api.json").write_text('{"manifest":"delayed-old-job"}\n')
        late = self.run_image_stage("api", "success")
        self.assertEqual(late.returncode, 65, late.stdout + late.stderr)
        self.assertEqual((self.workspace / "image-events").read_text().splitlines().count("publish-api"), 1)
        self.assertEqual(self.manifest()["effects"]["image-api-publication"]["state"], "uncertain")

    def test_successful_canonical_image_import_reuses_checkpoint_and_verifies_digest(self):
        first = self.run_image_stage("app", "success")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(self.manifest()["effects"]["image-app-publication"]["state"], "complete")
        self.assertEqual(self.manifest()["stages"]["image-app"]["state"], "complete")
        again = self.run_image_stage("app", "success")
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("reuse image-app: verified", again.stderr)
        self.assertEqual((self.workspace / "image-events").read_text().splitlines().count("publish-app"), 1)
        (self.workspace / "provider-app.json").write_text('{"manifest":"conflicting"}\n')
        changed = self.run_image_stage("app", "success")
        self.assertEqual(changed.returncode, 65, changed.stdout + changed.stderr)
        self.assertIn("Versioned image changed", changed.stderr)
        self.assertEqual((self.workspace / "image-events").read_text().splitlines().count("publish-app"), 1)

    def test_interrupted_stage_retries_completed_stage_reuses_exact_bytes_modes(self):
        target = self.completed()
        scratch = self.workspace / "windows.zip"
        self.invoke("begin", "windows", scratch)
        scratch.write_bytes(b"interrupted partial")
        # A later tool may legitimately clean earlier generated scratch.
        (target / "CodeResources").unlink()
        (target / "stale-extra.pkg").write_bytes(b"must never be published")
        self.assertEqual(self.invoke("validate"), 0)
        self.assertEqual(self.invoke("begin", "apple", target), 10)
        self.assertEqual((target / "CodeResources").read_bytes(), b"signed resources")
        self.assertEqual(stat.S_IMODE((target / "CodeResources").stat().st_mode), 0o644)
        self.assertEqual(stat.S_IMODE((target / "binary").stat().st_mode), 0o755)
        self.assertEqual(os.readlink(target / "current"), "binary")
        self.assertFalse((target / "stale-extra.pkg").exists())
        self.assertEqual(self.invoke("begin", "windows", scratch), 0)
        scratch.write_bytes(b"finished after retry")
        self.invoke("complete", "windows", scratch)
        self.assertEqual(self.invoke("begin", "windows", scratch), 10)

    def test_corrupt_cache_missing_cache_and_modes_are_not_rebuilt(self):
        target = self.completed()
        cached = Path(self.manifest()["stages"]["apple"]["outputs"][0]["path"])
        original = (cached / "binary").read_bytes()
        (cached / "binary").write_bytes(b"changed")
        with self.assertRaises(state.Refusal):
            self.invoke("begin", "apple", target)
        (cached / "binary").write_bytes(original)
        (cached / "binary").chmod(0o600)
        with self.assertRaises(state.Refusal):
            self.invoke("validate")
        (cached / "binary").unlink()
        with self.assertRaises(state.Refusal):
            self.invoke("validate")

    def test_source_option_toolchain_and_candidate_evidence_mismatch(self):
        self.sources = dict(self.sources, mmm={"head": "c" * 40})
        with self.assertRaisesRegex(state.Refusal, "mmm"):
            self.invoke("validate")
        self.sources["mmm"] = {"head": "b" * 40}
        os.environ["WARP_SKIP_DEPLOY"] = "1"
        with self.assertRaisesRegex(state.Refusal, "options"):
            self.invoke("validate")
        os.environ.pop("WARP_SKIP_DEPLOY")
        self.tool_mock.return_value = {"java": "22"}
        with self.assertRaisesRegex(state.Refusal, "toolchain"):
            self.invoke("validate")
        self.tool_mock.return_value = {"java": "21"}
        Path(self.manifest()["candidate_evidence"][0]["path"]).unlink()
        with self.assertRaises(state.Refusal):
            self.invoke("validate")

    def test_auth_refresh_does_not_record_secret_bytes(self):
        os.environ["GITHUB_API_KEY"] = "rotated-auth-must-not-be-saved"
        self.assertEqual(self.invoke("validate"), 0)
        path, manifest = state.load_manifest(self.workspace, VERSION)
        self.assertNotIn("rotated-auth", path.read_text())
        os.environ["NUGET_API_KEY"] = "new-publisher-choice"
        with self.assertRaisesRegex(state.Refusal, "publisher"):
            self.invoke("validate")

    def test_private_atomic_journal_no_partial_completion_or_secret_sourcing(self):
        target = self.workspace / "empty"
        target.touch()
        self.invoke("begin", "failed", target)
        with self.assertRaises(state.Refusal):
            self.invoke("complete", "failed", target)
        self.assertEqual(self.manifest()["stages"]["failed"]["state"], "running")
        path, manifest = state.load_manifest(self.workspace, VERSION)
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(path.parent.stat().st_mode), 0o700)
        manifest["sources"]["build"]["head"] = "$(touch should-not-execute)"
        path.write_text(json.dumps(manifest))  # Deliberately no atomic/checksum update.
        with self.assertRaisesRegex(state.Refusal, "checksum"):
            self.invoke("validate")
        self.assertFalse((self.workspace / "should-not-execute").exists())

    def test_symlink_escape_empty_output_and_declared_destination_change_refused(self):
        target = self.completed()
        with self.assertRaisesRegex(state.Refusal, "declared output"):
            self.invoke("begin", "apple", self.workspace / "source.txt")
        external = self.workspace / "external"
        external.write_bytes(b"immutable only in appearance")
        top = self.workspace / "top-link"
        top.symlink_to(external)
        with self.assertRaisesRegex(state.Refusal, "top-level"):
            state.outputs([top])
        (target / "outside").symlink_to(external)
        with self.assertRaisesRegex(state.Refusal, "escaping symlink"):
            self.invoke("begin", "apple", target)
        (target / "outside").unlink()
        (target / "cycle").symlink_to(".")
        with self.assertRaisesRegex(state.Refusal, "cycle"):
            state.outputs([target])

    def test_external_intent_is_uncertain_until_observed_success(self):
        self.invoke("effect-begin", "apple-ios-upload")
        self.assertEqual(self.manifest()["effects"]["apple-ios-upload"]["state"], "uncertain")
        self.invoke("effect-begin", "apple-ios-upload")
        self.assertEqual(self.manifest()["effects"]["apple-ios-upload"]["state"], "uncertain")
        self.invoke("effect-complete", "apple-ios-upload")
        self.assertEqual(self.manifest()["effects"]["apple-ios-upload"]["state"], "complete")
        with self.assertRaisesRegex(state.Refusal, "durable intent"):
            self.invoke("effect-complete", "unobserved")

    def test_copy_failure_preserves_previous_completed_cache(self):
        target = self.completed()
        original = self.manifest()
        other = self.workspace / "other.zip"
        self.invoke("begin", "other", other)
        other.write_bytes(b"large output")
        with mock.patch.object(state.shutil, "copy2", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                self.invoke("complete", "other", other)
        self.assertEqual(self.manifest()["stages"]["apple"], original["stages"]["apple"])
        self.assertEqual(self.invoke("begin", "apple", target), 10)

    def test_sdk_output_contract_uses_sealed_registry_plan_and_native_assets(self):
        sdk = self.workspace / "build/sdk"
        plan = sdk / "packaging/release/plan.json"
        plan.parent.mkdir(parents=True)
        path, value = state.load_manifest(self.workspace, VERSION)
        for selected, phase, expected in (
            ({}, "mobile", [plan]),
            ({"swift":True}, "mobile", [plan, sdk/"swift/dist"]),
            ({"pypi":True}, "desktop", [plan, sdk/"packaging/release/native.json", sdk/"cgo/build/darwin", sdk/"cgo/build/windows", sdk/"python/dist"]),
            ({"conan":True,"crates":True}, "desktop", [plan, sdk/"packaging/release/native.json", sdk/"cgo/build/darwin", sdk/"cgo/build/windows", sdk/"cgo/dist", sdk/"rust/dist", sdk/"packaging/release/assets"]),
        ):
            with self.subTest(selected=selected):
                plan.write_text(json.dumps({"version":VERSION,"registries":selected}))
                value["prepared_destinations"] = [str(plan)]
                value["candidate_evidence"] = state.retained_outputs(path.parent, "fixture-plan", [plan])
                state.atomic_json(path, value)
                output = io.StringIO()
                with mock.patch.object(sys,"argv",["release-state","sdk-outputs",str(self.workspace),VERSION,phase]),contextlib.redirect_stdout(output):
                    self.assertEqual(state.main(),0)
                self.assertEqual(output.getvalue().splitlines(),list(map(str,expected)))
        plan.write_text(json.dumps({"version":VERSION,"registries":{"pypi":True}}))
        with self.assertRaisesRegex(state.Refusal,"differs from the sealed plan"):
            self.invoke("sdk-outputs","desktop")

    def test_candidate_receipts_require_passed_exact_gate_and_nonempty_checked_outputs(self):
        directory = state.state_root(self.workspace) / VERSION
        paths = {"web":[self.workspace/"build/web/web/build/preview.ur.xyz"],
                 "mmm":[self.workspace/"mmm/mmm/build/preview.ur.io",self.workspace/"mmm/mmm/build/lag.dev"]}
        receipts = {}
        for name, outputs in paths.items():
            for target in outputs:
                target.mkdir(parents=True)
                (target/"index.html").write_text("checked candidate")
            receipts[name] = {"gate":name+" make check (WEB_BUILD_CHECKS=1)","outputs":state.outputs(outputs),"sources":{"pinned":"source"}}
            state.atomic_json(directory/(name+"-candidate.json"),receipts[name])
        # setUp mocks candidate collection for unrelated unit tests; exercise
        # the real implementation here through its unpatched module copy.
        checked = publication.state.candidate_gate_snapshot(self.workspace, VERSION)
        self.assertEqual(set(checked),{"web","mmm"})
        state.atomic_json(directory/"web-candidate.json",dict(receipts["web"],gate="web build without checks"))
        with self.assertRaisesRegex(publication.state.Refusal,"passed web"):
            publication.state.candidate_gate_snapshot(self.workspace,VERSION)
        state.atomic_json(directory/"web-candidate.json",receipts["web"])
        (paths["web"][0]/"index.html").write_text("")
        with self.assertRaises(publication.state.Refusal):
            publication.state.candidate_gate_snapshot(self.workspace,VERSION)
        (directory/"web-candidate.json").unlink()
        with self.assertRaises(OSError):
            publication.state.candidate_gate_snapshot(self.workspace,VERSION)


class HostBoundaries(unittest.TestCase):
    def test_publication_helper_without_python_B_does_not_modify_clean_source(self):
        with tempfile.TemporaryDirectory(prefix="release-bytecode-test-") as directory:
            root = Path(directory)
            for name in ("release-state.py", "release-publication.py"):
                shutil.copy2(HERE / name, root / name)
            for args in (("init", "-q"), ("config", "user.name", "Fixture"), ("config", "user.email", "fixture@example.invalid"), ("add", "."), ("commit", "-qm", "clean helpers")):
                subprocess.run(["git", "-C", directory, *args], check=True, capture_output=True)
            # -E ignores any inherited PYTHONDONTWRITEBYTECODE; no -B is used.
            result = subprocess.run([sys.executable, "-E", str(root / "release-publication.py"), "invalid-no-provider-action"], capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 65, result.stderr.decode())
            self.assertIn(b"invalid publication reconciliation arguments", result.stderr)
            self.assertFalse((root / "__pycache__").exists())
            self.assertEqual(subprocess.check_output(["git", "-C", directory, "status", "--porcelain"]), b"")

    def test_effective_go_environment_and_persisted_config_are_hash_only_inputs(self):
        with tempfile.TemporaryDirectory(prefix="release-goenv-test-") as directory:
            root = Path(directory).resolve()
            workspace = root / "workspace"
            config = root / "go.env"
            secret = "https://fixture-secret@example.invalid/proxy"
            config.write_text("GOPROXY=" + secret + "\n")
            values = {key: "" for key in state.GO_ENV_KEYS}
            values.update(GOENV=str(config), GOWORK="off", GOTOOLCHAIN="auto", GOPROXY=secret)
            with mock.patch.object(state, "run", side_effect=lambda *a, **kw: json.dumps(values).encode()) as command:
                original = state.go_environment_snapshot(workspace)
                self.assertNotIn("fixture-secret", json.dumps(original))
                self.assertEqual(command.call_args.kwargs["cwd"], workspace / "build")
                for key in ("GOFLAGS", "GOEXPERIMENT", "CC", "CXX", "CGO_ENABLED", "GOOS", "GOARCH", "GOROOT", "GOTOOLCHAIN"):
                    previous = values[key]
                    values[key] = "changed-effective-input"
                    self.assertNotEqual(state.go_environment_snapshot(workspace), original, key)
                    values[key] = previous
                config.write_text("GOFLAGS=-tags=changed-persisted-config\n")
                self.assertNotEqual(state.go_environment_snapshot(workspace), original)
                values["GOWORK"] = str(root / "external.go.work")
                with self.assertRaisesRegex(state.Refusal, "external GOWORK"):
                    state.go_environment_snapshot(workspace)
                workspace.mkdir()
                work = workspace / "go.work"
                work.write_text("go 1.27.2\nuse ./build/sdk\n")
                values["GOWORK"] = str(work)
                before = state.go_environment_snapshot(workspace)
                work.write_text("go 1.27.2\nuse ./changed-module\n")
                self.assertNotEqual(state.go_environment_snapshot(workspace), before)

    def test_java_version_uses_stderr_and_checks_timeout(self):
        with mock.patch.object(state, "TOOLS", (("java", "-version"),)), mock.patch.object(state, "artifact", return_value={"sha256":"executable"}), mock.patch.object(state, "run", side_effect=[b"/fake/java\n", b'openjdk version "21.0.9"\n']) as command, mock.patch.dict(os.environ, {"ANDROID_NDK_HOME": ""}):
            snapshot = state.tool_snapshot()
            self.assertEqual(snapshot["java"]["version"], state.digest(b'openjdk version "21.0.9"\n'))
            self.assertTrue(command.call_args.kwargs["combined"])
        with mock.patch.object(state.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"ok")) as command:
            state.run(["synthetic-tool"])
            self.assertEqual(command.call_args.kwargs["timeout"], 60)

    def test_supervisor_kill_keeps_lock_while_fd_closing_grandchild_runs(self):
        with tempfile.TemporaryDirectory(prefix="release-lock-test-") as directory:
            root = Path(directory)
            ready = root / "ready"
            # Close the inherited FD before spawning a grandchild; the separate
            # process-group reaper, not incidental FD inheritance, must protect it.
            code = "import os,subprocess,sys,time; os.close(int(os.environ['RELEASE_LOCK_FD'])); p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(4)'],close_fds=True); open(sys.argv[1],'w').write(str(os.getpid())); time.sleep(30)"
            supervisor = subprocess.Popen([sys.executable, str(HERE / "release-state.py"), "locked", directory, "--", sys.executable, "-c", code, str(ready)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                deadline = time.monotonic() + 5
                while not ready.exists() and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(ready.exists())
                group = int(ready.read_text())
                supervisor.kill()
                supervisor.wait(timeout=3)
                # Killing just the direct child must not unlock its grandchild.
                os.kill(group, signal.SIGKILL)
                probe = subprocess.run([sys.executable, str(HERE / "release-state.py"), "locked", directory, "--", sys.executable, "-c", "pass"], capture_output=True, timeout=3)
                self.assertEqual(probe.returncode, 65, probe.stderr.decode())
                self.assertIn(b"another release owns", probe.stderr)
                time.sleep(4.3)
                # A drained PGID is insufficient proof for detached containers.
                probe = subprocess.run([sys.executable, str(HERE / "release-state.py"), "locked", directory, "--", sys.executable, "-c", "import os; assert os.umask(0o022)==0o022"], capture_output=True, timeout=5)
                self.assertEqual(probe.returncode, 65, probe.stderr.decode())
                self.assertIn(b"previous release exit is uncertain", probe.stderr)
                fake = root / "fake-inventory"
                fake.mkdir()
                for name, body in {"ps":"", "lsof":"", "docker":'if [ "$1" = context ]; then printf "unix:///fixture/docker.sock\\n"; fi'}.items():
                    command = fake/name
                    command.write_text("#!/bin/sh\n"+body+"\nexit 0\n")
                    command.chmod(0o755)
                environment=dict(os.environ,PATH=str(fake)+":"+os.environ["PATH"])
                recovered=subprocess.run([sys.executable,str(HERE/"release-state.py"),"recover-lock",directory],env=environment,capture_output=True,timeout=5)
                self.assertEqual(recovered.returncode,0,recovered.stderr.decode())
                # Finished-owner normal entry must not reject unrelated idle
                # terminals/code hosts merely because they share a CWD.
                (fake/"lsof").write_text("#!/bin/sh\nprintf 'p999999\\nn"+directory+"/build\\n'\n")
                probe=subprocess.run([sys.executable,str(HERE/"release-state.py"),"locked",directory,"--",sys.executable,"-c","import os; assert os.umask(0o022)==0o022"],env=environment,capture_output=True,timeout=5,umask=0o077)
                self.assertEqual(probe.returncode,0,probe.stderr.decode())
            finally:
                if supervisor.poll() is None:
                    supervisor.kill()
                    supervisor.wait(timeout=3)

    def test_cleanup_inventory_refuses_vm_workspace_writer_mount_and_probe_errors(self):
        with tempfile.TemporaryDirectory(prefix="release-cleanup-test-") as directory:
            root=Path(directory).resolve()
            pid=os.getpid()
            rows=f"{pid} 1 {os.getuid()} python\n1 0 {os.getuid()} shell\n"
            def inspect(responses):
                with mock.patch.object(state,"run",side_effect=responses),mock.patch.object(state.os,"killpg",side_effect=ProcessLookupError):
                    state.inspect_workspace_writers(root,{"pgid":123456})
            inspect([rows.encode(),f"p{pid}\nn{root}\np1\nn{root}\n".encode(),b"unix:///fixture/docker.sock\n",b""])
            for label,responses in {
                "VM":[(rows+f"999 1 {os.getuid()} /fixture/qemu-system-aarch64\n").encode()],
                "CWD":[rows.encode(),f"p999\nn{root}/build\n".encode()],
                "remote Docker":[rows.encode(),b"",b"tcp://untrusted-host\n"],
                "workspace mount":[rows.encode(),b"",b"unix:///fixture/docker.sock\n",b"abcdef123456\n",json.dumps([{"Type":"bind","RW":True,"Source":str(root/"build")}]).encode()],
                "ancestor mount":[rows.encode(),b"",b"unix:///fixture/docker.sock\n",b"abcdef123456\n",json.dumps([{"Type":"bind","RW":True,"Source":str(root.parent)}]).encode()],
                "probe failure":[state.Refusal("inventory unavailable")],
            }.items():
                with self.subTest(label=label),self.assertRaises(state.Refusal): inspect(responses)
            inspect([rows.encode(),b"",b"unix:///fixture/docker.sock\n",b"abcdef123456\n",json.dumps([{"Type":"bind","RW":True,"Source":"/unrelated"},{"Type":"bind","RW":False,"Source":str(root)}]).encode()])
            with mock.patch.object(state.os,"killpg",return_value=None),self.assertRaisesRegex(state.Refusal,"group still exists"):
                state.inspect_workspace_writers(root,{"pgid":123456})

    def test_finished_owner_inventory_skips_idle_readers_but_keeps_writer_checks(self):
        with tempfile.TemporaryDirectory(prefix="release-finished-test-") as directory:
            rows = f"999991 1 {os.getuid()} -zsh\n999992 1 {os.getuid()} code-mode-host\n".encode()
            with mock.patch.object(state, "run", side_effect=[rows, b"unix:///fixture/docker.sock\n", b""]) as command:
                state.inspect_workspace_writers(directory, {"state":"finished"}, uncertain=False)
                self.assertFalse(any(call.args[0][0] == "lsof" for call in command.call_args_list))
            responses = [rows, b"unix:///fixture/docker.sock\n", b"abcdef123456\n", json.dumps([{"Type":"bind","RW":True,"Source":directory}]).encode()]
            with mock.patch.object(state,"run",side_effect=responses),self.assertRaisesRegex(state.Refusal,"writable workspace"):
                state.inspect_workspace_writers(directory,{"state":"finished"},uncertain=False)


class Publications(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="release-publication-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        directory = publication.state.state_root(self.root) / VERSION
        directory.mkdir(mode=0o700)
        self.receipt = directory / ("github-" + VERSION + ".json")
        self.commit = "a" * 40
        self.release = {"id": 42, "tag_name": "v" + VERSION, "target_commitish": "main", "prerelease": False, "draft": True}
        for patch in (mock.patch.dict(os.environ, {"WARP_HOME": str(self.root), "BUILD_HOME": str(self.root / "build"), "RELEASE_STATE_VERSION": VERSION}),
                      mock.patch.object(publication.state, "inherited_lock", return_value=True),
                      mock.patch.object(publication.state, "load_manifest", return_value=(None, {"seal": "sealed"}))):
            patch.start()
            self.addCleanup(patch.stop)
        self.tag = mock.patch.object(publication, "verify_tag")
        self.tag.start()
        self.addCleanup(self.tag.stop)

    def test_draft_get_by_tag_404_then_resume_get_by_observed_id(self):
        with mock.patch.object(publication, "request", side_effect=[None, self.release, self.release]) as request:
            self.assertEqual(publication.release(VERSION, self.commit, False)["id"], 42)
            self.assertEqual(publication.release(VERSION, self.commit, False)["id"], 42)
            self.assertEqual([call.args[0].split(publication.API)[1] for call in request.call_args_list], ["/releases/tags/v" + VERSION, "/releases", "/releases/42"])
            self.assertEqual(sum(call.args[1:2] == ("POST",) for call in request.call_args_list), 1)
        self.assertEqual(stat.S_IMODE(self.receipt.stat().st_mode), 0o600)
        self.assertEqual(json.loads(self.receipt.read_text())["id"], 42)

    def test_lost_post_response_never_creates_second_draft(self):
        with mock.patch.object(publication, "request", side_effect=[None, publication.Conflict("lost POST response"), None]) as request:
            with self.assertRaisesRegex(publication.Conflict, "lost POST"):
                publication.release(VERSION, self.commit, False)
            self.assertEqual(json.loads(self.receipt.read_text())["state"], "uncertain")
            with self.assertRaisesRegex(publication.Conflict, "uncertain"):
                publication.release(VERSION, self.commit, False)
            self.assertEqual(sum(call.args[1:2] == ("POST",) for call in request.call_args_list), 1)

    def test_conflicting_channel_and_receipt_rejected(self):
        with mock.patch.object(publication, "request", return_value=dict(self.release, prerelease=True)):
            with self.assertRaisesRegex(publication.Conflict, "identity"):
                publication.release(VERSION, self.commit, False)
        with mock.patch.object(publication, "request", side_effect=[None, self.release]):
            publication.release(VERSION, self.commit, False)
        with mock.patch.object(publication, "request") as request:
            with self.assertRaisesRegex(publication.Conflict, "saved"):
                publication.release(VERSION, "b" * 40, False)
            request.assert_not_called()

    def test_remote_peeled_tag_not_target_commitish_proves_identity(self):
        self.tag.stop()
        tag = "refs/tags/v" + VERSION
        result = subprocess.CompletedProcess([], 0, ("f" * 40 + "\t" + tag + "\n" + self.commit + "\t" + tag + "^{}\n").encode())
        with mock.patch.object(publication.subprocess, "run", return_value=result), mock.patch.object(publication, "request", return_value=self.release):
            publication.release(VERSION, self.commit, False)  # target_commitish=main is valid.
            with self.assertRaisesRegex(publication.Conflict, "different commit"):
                publication.release(VERSION, "b" * 40, False)

    def test_assets_require_exact_digest_size_and_single_observed_upload(self):
        path = self.root / "sdk.zip"
        path.write_bytes(b"verified bytes")
        asset = {"name": path.name, "id": 4, "size": path.stat().st_size, "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(), "state": "uploaded"}
        for listing, accepted in (([], True), ([asset], True), ([asset, asset], False), ([dict(asset, digest=None)], False), ([dict(asset, size=1)], False), ([dict(asset, state="starter")], False)):
            with self.subTest(listing=listing), mock.patch.object(publication, "request", return_value=listing):
                if accepted:
                    self.assertEqual(publication.asset("42", path.name, path), asset if listing else None)
                else:
                    with self.assertRaises(publication.Conflict):
                        publication.asset("42", path.name, path)

    def test_npm_checks_exact_tarball_not_just_version(self):
        path = self.root / "sdk.tgz"
        package = {"name": "@urnetwork/sdk", "version": VERSION}
        body = json.dumps(package).encode()
        with tarfile.open(path, "w:gz") as archive:
            info = tarfile.TarInfo("package/package.json")
            info.size = len(body)
            archive.addfile(info, io.BytesIO(body))
        import base64
        integrity = "sha512-" + base64.b64encode(hashlib.sha512(path.read_bytes()).digest()).decode()
        with mock.patch.object(publication, "request", return_value=dict(package, dist={"integrity": integrity})) as request:
            publication.npm_archive(path, VERSION)
            self.assertFalse(request.call_args.kwargs["github"])
        with mock.patch.object(publication, "request", return_value=dict(package, dist={"integrity": "wrong"})):
            with self.assertRaisesRegex(publication.Conflict, "differs"):
                publication.npm_archive(path, VERSION)


class ShellSideEffectGuards(unittest.TestCase):
    def shell(self, root, body, **extra):
        environment = {"PATH":os.environ["PATH"],"WARP_HOME":str(root),"BUILD_HOME":str(HERE.parent),
                       "BUILD_ENV":"main","EXTERNAL_WARP_VERSION":VERSION,"APPLE_API_KEY":"fixture","APPLE_API_ISSUER":"fixture",**extra}
        return subprocess.run(["/bin/zsh","-c",'source "$BUILD_HOME/all/release-checkpoints.zsh"\n'+body],env=environment,capture_output=True,text=True,timeout=5)

    def test_all_canonical_image_builds_and_imports_bracket_publication_intent(self):
        source = (HERE / "run.sh").read_text()
        stages = list(re.finditer(r'(?m)^\s*if release_build image-([a-z-]+) ', source))
        self.assertEqual({stage[1] for stage in stages}, {"config-updater","grafana","lb","taskworker","api","connect","mcp","gossip","alt","proxy","web","mmm","manager","app"})
        for stage in stages:
            name = stage[1]
            end = source.index("release_complete image-" + name + " ", stage.end())
            block = source[stage.start():end]
            begin = block.index("release_image_publication_begin " + name)
            operation = block.index("warpctl import " if name == "app" else "warpctl build ")
            observed = block.index("release_image_publication_complete " + name)
            self.assertLess(begin, operation, name)
            self.assertLess(operation, observed, name)

    def test_actual_apple_upload_helper_stops_uncertain_and_skips_observed_complete(self):
        with tempfile.TemporaryDirectory(prefix="release-apple-guard-") as directory:
            root = Path(directory)
            body = r'''
release_state() {
    case "$1" in
        effect-begin)
            if [[ -f "$WARP_HOME/effect" ]]; then /bin/cat "$WARP_HOME/effect";
            else print uncertain > "$WARP_HOME/effect"; fi ;;
        effect-complete) print complete > "$WARP_HOME/effect" ;;
        *) return 99 ;;
    esac
}
xcrun() { print upload >> "$WARP_HOME/events"; return "$UPLOAD_EXIT"; }
release_apple_upload ios "$WARP_HOME/verified.ipa"
'''
            first = self.shell(root,body,UPLOAD_EXIT="37")
            self.assertEqual(first.returncode,37,first.stderr)
            retry = self.shell(root,body,UPLOAD_EXIT="0")
            self.assertEqual(retry.returncode,65,retry.stderr)
            self.assertIn("uncertain",retry.stderr)
            self.assertEqual((root/"events").read_text(),"upload\n")
            (root/"effect").write_text("complete\n")
            observed = self.shell(root,body,UPLOAD_EXIT="0")
            self.assertEqual(observed.returncode,0,observed.stderr)
            self.assertEqual((root/"events").read_text(),"upload\n")

    def test_actual_image_guard_refuses_unobserved_publication_and_ambiguous_absence(self):
        with tempfile.TemporaryDirectory(prefix="release-image-guard-") as directory:
            root = Path(directory)
            receipt_dir = root/".release-checkpoints"/VERSION
            receipt_dir.mkdir(parents=True)
            body = r'''
docker() { print -r -- "$PROBE_BODY"; print -u2 -r -- "$PROBE_ERROR"; return "$PROBE_EXIT"; }
release_image_absent api
'''
            for diagnostic, code, expected in (("", "0", 65), ("credentials: not found", "1", 65), ("manifest unknown: unauthorized", "1", 65), ("connection timed out", "1", 65), ("manifest unknown", "1", 0), ("docker.io/bringyour/main-api:"+VERSION+": not found", "1", 0)):
                result = self.shell(root,body,PROBE_BODY="{}",PROBE_ERROR=diagnostic,PROBE_EXIT=code)
                self.assertEqual(result.returncode,expected,result.stderr)
            body = body.replace("release_image_absent api","release_image_receipt api")
            first = self.shell(root,body,PROBE_BODY='{"digest":"original"}',PROBE_ERROR="",PROBE_EXIT="0")
            self.assertEqual(first.returncode,0,first.stderr)
            mismatch = self.shell(root,body,PROBE_BODY='{"digest":"conflicting"}',PROBE_ERROR="",PROBE_EXIT="0")
            self.assertEqual(mismatch.returncode,65,mismatch.stderr)
            self.assertEqual((receipt_dir/"image-api.json").read_text(),'{"digest":"original"}\n')


if __name__ == "__main__":
    unittest.main()
