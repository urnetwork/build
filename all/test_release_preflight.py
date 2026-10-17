#!/usr/bin/env python3
"""Execute the canonical preflight shell/helper with fake provider boundaries."""

import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
SERVICES = ("config-updater", "grafana", "mmm", "lb", "taskworker", "api", "connect",
            "web", "app", "mcp", "gossip", "alt", "proxy")

FAKE_PROVIDER = r'''
import fcntl, json, os, pathlib, signal, subprocess, sys, time
root = pathlib.Path(os.environ["PREFLIGHT_FIXTURE"])
args = sys.argv[1:]
name = "native" if args == ["release-preflight"] else args[2]
mode = os.environ.get("PREFLIGHT_FIXTURE_MODE", "success")
def event(kind):
    current_umask = os.umask(0o022)
    os.umask(current_umask)
    value = {"kind":kind,"name":name,"args":args,"pid":os.getpid(),
             "pgid":os.getpgrp(),"at":time.monotonic(),"umask":current_umask}
    with (root/"events.jsonl").open("a") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        stream.write(json.dumps(value)+"\n")
        stream.flush()
event("start")
if name == "native":
    if mode == "native-failure":
        sys.exit(42)
    time.sleep(float(os.environ.get("PREFLIGHT_FIXTURE_NATIVE_DELAY", ".05")))
    event("end")
    sys.exit(0)
if mode in ("failure", "timeout", "signal", "orphan"):
    if name == "config-updater" and mode == "failure":
        until = time.monotonic()+5
        while not (root/"grafana-grandchild").exists() and time.monotonic()<until:
            time.sleep(.01)
        sys.exit(29)
    # Simulate a shell -> warpctl -> docker -> helper tree. The last two
    # deliberately ignore TERM; cancelling only direct children must fail.
    child = r"""
import os,pathlib,signal,subprocess,sys,time
root=pathlib.Path(sys.argv[1]); name=sys.argv[2]
signal.signal(signal.SIGTERM, signal.SIG_IGN)
grandchild=subprocess.Popen([sys.executable,"-c",
    "import os,pathlib,signal,sys,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(60)",
    str(root/(name+"-grandchild"))])
(root/(name+"-child")).write_text(str(os.getpid()))
grandchild.wait()
"""
    subprocess.Popen([sys.executable,"-c",child,str(root),name],close_fds=True)
    if mode == "orphan" and name == "config-updater":
        until=time.monotonic()+5
        while not (root/(name+"-grandchild")).exists() and time.monotonic()<until:
            time.sleep(.01)
        event("end")
        sys.exit(0)
    time.sleep(60)
else:
    time.sleep(.12)
    event("end")
'''


class CanonicalPreflight(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release-preflight-test-")
        self.root = Path(self.temporary.name)
        self.build = self.root / "build"
        self.all = self.build / "all"
        self.all.mkdir(parents=True)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.server = self.root / "server"
        self.server.mkdir()
        for name in ("release-preflight.zsh", "release-preflight.py", "deploy-rollout.zsh"):
            shutil.copyfile(HERE / name, self.all / name)
        self.write(self.bin / "warpctl", f"#!{sys.executable}\n" + FAKE_PROVIDER, 0o755)
        self.native = self.bin / "bringyourctl"
        self.write(self.native, f"#!{sys.executable}\n" + FAKE_PROVIDER, 0o755)
        self.write(self.server / "run-main-lan.sh",
                   '#!/bin/sh\nexec "$BRINGYOURCTL_BINARY" "$@"\n', 0o755)
        timeout = shutil.which("timeout")
        self.assertIsNotNone(timeout, "the canonical host requires GNU timeout")
        # Only substitute wall time at the external command boundary; execute
        # unchanged production sources and GNU timeout's real group semantics.
        self.write(self.bin / "timeout", f"#!{sys.executable}\n" + '''
import json,os,pathlib,sys
root=pathlib.Path(os.environ["PREFLIGHT_FIXTURE"])
args=sys.argv[1:]
with (root/"timeouts.jsonl").open("a") as stream: stream.write(json.dumps(args)+"\\n")
if args[:2] == ["--kill-after=10","300"] and os.environ.get("PREFLIGHT_FIXTURE_SECONDS"):
    args[1]=os.environ["PREFLIGHT_FIXTURE_SECONDS"]
os.execv(''' + repr(timeout) + ''',['timeout',*args])
''', 0o755)
        self.write(self.bin / "python3", f"#!{sys.executable}\n" + '''
import os,pathlib,sys
pathlib.Path(os.environ["PREFLIGHT_FIXTURE"],"coordinator.pid").write_text(str(os.getpid()))
# Exercise the actual helper without -B too: it must not import local modules
# or create an untracked __pycache__ after a release was sealed.
args=[arg for arg in sys.argv[1:] if arg != "-B"]
os.execv(sys.executable,[sys.executable,*args])
''', 0o755)
        self.env = dict(os.environ, BUILD_HOME=str(self.build), WARP_HOME=str(self.root),
                        BUILD_ENV="main", WARP_VERSION="9999.0.0+0",
                        EXTERNAL_WARP_VERSION="9999.0.0-0", PREFLIGHT_FIXTURE=str(self.root),
                        PATH=str(self.bin) + os.pathsep + os.environ["PATH"])
        self.env.pop("WARP_SKIP_DEPLOY", None)
        self.env.pop("PYTHONDONTWRITEBYTECODE", None)
        self.children = []
        self.unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"],
                                          start_new_session=True)
        self.log = self.root / "private.log"
        self.write(self.log, "", 0o600)

    def tearDown(self):
        for process in self.children:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
        # Cleanup failed test fixtures as well; never act on a nonfixture group.
        for event in self.events():
            if event["kind"] == "start":
                try:
                    os.killpg(event["pgid"], signal.SIGKILL)
                except ProcessLookupError:
                    pass
        self.unrelated.kill()
        self.unrelated.wait()
        self.temporary.cleanup()

    @staticmethod
    def write(path, content, mode=0o644):
        path.write_text(content)
        path.chmod(mode)

    def events(self):
        path = self.root / "events.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def launch(self, mode="success", phase="future", seconds=None, native_delay=None):
        environment = dict(self.env, PREFLIGHT_FIXTURE_MODE=mode)
        if seconds is not None:
            environment["PREFLIGHT_FIXTURE_SECONDS"] = str(seconds)
        if native_delay is not None:
            environment["PREFLIGHT_FIXTURE_NATIVE_DELAY"] = str(native_delay)
        command = '''
source "$BUILD_HOME/all/release-preflight.zsh"
builder_message() { print -r -- "$1"; }
release_preflight "$1" "$2" "$3"
code=$?
print -r -- "release-parent-survived:$code"
exit "$code"
'''
        with self.log.open("a") as output:
            process = subprocess.Popen(["zsh", "-c", command, "preflight-fixture",
                                        str(self.server), str(self.native), phase],
                                       env=environment, stdout=output, stderr=subprocess.STDOUT,
                                       start_new_session=True, umask=0o077)
        self.children.append(process)
        return process

    def wait_for(self, predicate, seconds=6):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(.02)
        self.fail("fixture readiness timed out\n" + self.log.read_text())

    def assert_clean(self, result):
        self.assertIn(f"release-parent-survived:{result}", self.log.read_text())
        self.assertIsNone(self.unrelated.poll(), "preflight killed unrelated work")
        self.assertEqual(stat.S_IMODE(self.log.stat().st_mode), 0o600)
        self.assertFalse(list(self.build.rglob("__pycache__")))
        for event in self.events():
            self.assertEqual(event["umask"], 0o022)
            if event["kind"] == "start":
                with self.assertRaises(ProcessLookupError, msg=f"owned group survived: {event}"):
                    os.killpg(event["pgid"], 0)
        for path in self.root.glob("*-child"):
            with self.assertRaises(ProcessLookupError):
                os.kill(int(path.read_text()), 0)
        for path in self.root.glob("*-grandchild"):
            with self.assertRaises(ProcessLookupError):
                os.kill(int(path.read_text()), 0)

    def test_all_services_exact_guards_two_workers_after_native(self):
        for phase in ("future", "images"):
            with self.subTest(phase=phase):
                (self.root / "events.jsonl").unlink(missing_ok=True)
                process = self.launch(phase=phase)
                self.assertEqual(process.wait(timeout=12), 0, self.log.read_text())
                events = self.events()
                self.assertEqual([(event["kind"], event["name"]) for event in events[:2]],
                                 [("start", "native"), ("end", "native")])
                started = [event for event in events if event["kind"] == "start" and event["name"] != "native"]
                self.assertCountEqual([event["name"] for event in started], SERVICES)
                self.assertEqual(len(started), len(SERVICES))
                active = peak = 0
                for event in events[2:]:
                    active += 1 if event["kind"] == "start" else -1
                    peak = max(peak, active)
                    self.assertLessEqual(active, 2)
                self.assertEqual((active, peak), (0, 2))
                for event in started:
                    name = event["name"]
                    expected = ["deploy", "main", name, "9999.0.0+0", "--percent=100", "--timeout", "0"]
                    if name == "mmm":
                        expected += ["--only-older", "--initialize-unset"]
                    elif name in ("grafana", "taskworker", "api", "connect", "web", "app", "mcp"):
                        expected += ["--only-older"]
                    expected += ["--preflight"]
                    if phase == "future":
                        expected += ["--allow-missing-image"]
                    self.assertEqual(event["args"], expected)
                self.assert_clean(0)

    def test_native_failure_starts_no_services(self):
        process = self.launch("native-failure")
        self.assertEqual(process.wait(timeout=12), 42, self.log.read_text())
        self.assertEqual([event["name"] for event in self.events()], ["native"])
        self.assert_clean(42)

    def test_service_failure_kills_term_ignoring_descendants(self):
        process = self.launch("failure")
        self.assertEqual(process.wait(timeout=12), 29, self.log.read_text())
        self.assertEqual({event["name"] for event in self.events()}, {"native", "config-updater", "grafana"})
        self.assertTrue((self.root / "grafana-grandchild").exists())
        self.assert_clean(29)

    def test_outer_deadline_includes_native_and_reaps_all_workers(self):
        start = time.monotonic()
        process = self.launch("timeout", seconds=2, native_delay=1)
        self.assertEqual(process.wait(timeout=12), 124, self.log.read_text())
        elapsed = time.monotonic() - start
        self.assertGreaterEqual(elapsed, 4.5)  # two-second pass + TERM grace
        self.assertLess(elapsed, 7)
        self.assertTrue((self.root / "config-updater-grandchild").exists())
        self.assertTrue((self.root / "grafana-grandchild").exists())
        args = json.loads((self.root / "timeouts.jsonl").read_text().splitlines()[0])
        self.assertEqual(args[:2], ["--kill-after=10", "300"])
        self.assert_clean(124)

    def test_external_signal_reaps_workers_without_signalling_parent(self):
        process = self.launch("signal")
        self.wait_for(lambda: (self.root / "grafana-grandchild").exists())
        os.kill(int((self.root / "coordinator.pid").read_text()), signal.SIGTERM)
        self.assertEqual(process.wait(timeout=12), 143, self.log.read_text())
        self.assert_clean(143)

    def test_success_with_orphaned_descendants_is_not_a_pass(self):
        process = self.launch("orphan")
        self.assertEqual(process.wait(timeout=12), 70, self.log.read_text())
        self.assertIn("worker left descendants", self.log.read_text())
        self.assert_clean(70)


class CoordinatorDeadlines(unittest.TestCase):
    def load_module(self):
        spec = importlib.util.spec_from_file_location("preflight_fixture_module", HERE / "release-preflight.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_native_budget_is_100_inside_unchanged_300_total(self):
        module = self.load_module()
        self.assertEqual((module.PASS_SECONDS, module.NATIVE_SECONDS, module.PARALLEL_SERVICES), (300, 100, 2))
        with mock.patch.object(module.time, "monotonic", return_value=10):
            coordinator = module.Coordinator()
        self.assertEqual(coordinator.deadline, 310)
        with mock.patch.object(module.time, "monotonic", return_value=111):
            with self.assertRaises(module.GateFailure) as caught:
                coordinator.check_deadline(110)
            self.assertEqual(caught.exception.code, 124)
        with mock.patch.object(module.time, "monotonic", return_value=311):
            with self.assertRaises(module.GateFailure):
                coordinator.check_deadline(1000)

    def test_signal_during_last_collection_is_not_success(self):
        module = self.load_module()
        coordinator = module.Coordinator()
        def start(*_args, **_kwargs):
            coordinator.active[1] = ("fixture", mock.Mock())
        def collect():
            if coordinator.active:
                coordinator.active.clear()
                coordinator.interrupt(signal.SIGTERM, None)
        with mock.patch.object(module, "SERVICES", ("web",)), \
             mock.patch.object(coordinator, "start", side_effect=start), \
             mock.patch.object(coordinator, "wait", side_effect=lambda _deadline: coordinator.active.clear()), \
             mock.patch.object(coordinator, "collect", side_effect=collect):
            with self.assertRaises(module.GateFailure) as caught:
                coordinator.run("fixture", "fixture", "future")
            self.assertEqual(caught.exception.code, 143)

    def test_signal_during_success_cleanup_is_not_success(self):
        module = self.load_module()
        coordinator = module.Coordinator()
        def cleanup():
            coordinator.interrupt(signal.SIGHUP, None)
            return True
        with mock.patch.object(module, "Coordinator", return_value=coordinator), \
             mock.patch.object(coordinator, "run", return_value=0), \
             mock.patch.object(coordinator, "cleanup", side_effect=cleanup):
            self.assertEqual(module.main(["fixture", "fixture", "future"]), 129)

    def test_permission_denied_is_never_proof_of_group_exit(self):
        module = self.load_module()
        with mock.patch.object(module.os, "killpg", side_effect=PermissionError):
            self.assertTrue(module.group_alive(123))
            coordinator = module.Coordinator()
            coordinator.active[123] = ("fixture", mock.Mock())
            with mock.patch.object(module, "TERM_SECONDS", 0), \
                 mock.patch.object(module, "KILL_SECONDS", 0), \
                 mock.patch.object(module.sys, "stderr"):
                self.assertFalse(coordinator.cleanup())
                self.assertIn(123, coordinator.active)


if __name__ == "__main__":
    unittest.main()
