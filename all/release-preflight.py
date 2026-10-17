#!/usr/bin/env python3
"""Bound the read-only release gates without changing Warp's guard planner.

Services run two at a time, but every Warp invocation retains its sequential
fresh-connection status samples and clean-pass retries. The native Main LAN,
Grafana and Brevo gate must finish first. There are no local module imports or
state files; all output uses the caller's already-private log descriptors.
"""

import os
import signal
import subprocess
import sys
import time

sys.dont_write_bytecode = True

SERVICES = (
    "config-updater", "grafana", "mmm", "lb", "taskworker", "api", "connect",
    "web", "app", "mcp", "gossip", "alt", "proxy",
)
PARALLEL_SERVICES = 2
PASS_SECONDS = 300
NATIVE_SECONDS = 100
TERM_SECONDS = 3
KILL_SECONDS = 2
POLL_SECONDS = 0.025

SERVICE_COMMAND = '''
source "$BUILD_HOME/all/deploy-rollout.zsh" || exit $?
builder_message() { print -r -- "$1"; }
warp_rollout_deploy "$1" 100 preflight "$2"
'''


class GateFailure(Exception):
    def __init__(self, code, message):
        self.code = code
        super().__init__(message)


def group_alive(pgid):
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        # macOS can briefly report EPERM for a dying/reparented group. It is
        # not evidence of absence: retain ownership and keep bounded polling.
        return True


def signal_group(pgid, signum):
    try:
        os.killpg(pgid, signum)
    except (ProcessLookupError, PermissionError):
        # Permission failures are NOT treated as exit. cleanup still requires
        # group_alive() to prove absence, otherwise it fails closed.
        pass


class Coordinator:
    def __init__(self):
        # This clock starts before the native gate, not when services begin.
        self.deadline = time.monotonic() + PASS_SECONDS
        self.active = {}
        self.interrupted = None

    def interrupt(self, signum, _frame):
        # Do not raise inside Popen: first retain the new process-group handle,
        # then let the monitor enter its single cleanup path.
        if self.interrupted is None:
            self.interrupted = signum

    def check_deadline(self, deadline):
        if self.interrupted is not None:
            raise GateFailure(128 + self.interrupted, "read-only preflight interrupted")
        if time.monotonic() >= min(self.deadline, deadline):
            raise GateFailure(124, "read-only preflight deadline exceeded")

    def start(self, name, command, env=None):
        self.check_deadline(self.deadline)
        process = subprocess.Popen(command, env=env, start_new_session=True,
                                   stdin=subprocess.DEVNULL, umask=0o022)
        self.active[process.pid] = (name, process)
        print(f"read-only preflight worker started: {name} (pgid {process.pid})", flush=True)

    def collect(self):
        # Inspect every active result before allowing any replacement worker.
        for pgid, (name, process) in list(self.active.items()):
            code = process.poll()
            if code is None:
                continue
            if code:
                raise GateFailure(code if code > 0 else 128 - code,
                                  f"read-only preflight failed: {name}")
            if group_alive(pgid):
                raise GateFailure(70, f"read-only preflight worker left descendants: {name}")
            del self.active[pgid]
            print(f"read-only preflight worker passed: {name}", flush=True)

    def wait(self, deadline):
        while self.active:
            self.check_deadline(deadline)
            self.collect()
            if self.active:
                time.sleep(POLL_SECONDS)

    def cleanup(self):
        """Stop only our new sessions, including children that outlive a shell.

        TERM/KILL and direct-child waits finish inside timeout's 10s backstop.
        A vanished shell is not proof that its docker/status descendants exited.
        Never signal this coordinator's or the release parent's process group.
        """
        for pgid in self.active:
            signal_group(pgid, signal.SIGTERM)
        until = time.monotonic() + TERM_SECONDS
        while self.active and time.monotonic() < until:
            for pgid, (_, process) in list(self.active.items()):
                process.poll()  # reap direct children before testing group exit
                if not group_alive(pgid):
                    del self.active[pgid]
            if self.active:
                time.sleep(POLL_SECONDS)
        for pgid in self.active:
            signal_group(pgid, signal.SIGKILL)
        until = time.monotonic() + KILL_SECONDS
        while self.active and time.monotonic() < until:
            for pgid, (_, process) in list(self.active.items()):
                process.poll()
                if not group_alive(pgid):
                    del self.active[pgid]
            if self.active:
                time.sleep(POLL_SECONDS)
        if self.active:
            print("read-only preflight cleanup could not confirm owned process-group exit",
                  file=sys.stderr, flush=True)
            return False
        return True

    def run(self, server_dir, binary, phase):
        native_env = dict(os.environ, BRINGYOURCTL_BINARY=binary)
        native_deadline = min(self.deadline, time.monotonic() + NATIVE_SECONDS)
        self.start("Main LAN/Grafana/Brevo",
                   [os.path.join(server_dir, "run-main-lan.sh"), "release-preflight"],
                   env=native_env)
        self.wait(native_deadline)
        pending = iter(SERVICES)
        exhausted = False
        while self.active or not exhausted:
            self.check_deadline(self.deadline)
            self.collect()
            while len(self.active) < PARALLEL_SERVICES and not exhausted:
                service = next(pending, None)
                if service is None:
                    exhausted = True
                else:
                    self.start(service, ["zsh", "-c", SERVICE_COMMAND,
                                         "release-preflight-service", service, phase])
            if self.active:
                time.sleep(POLL_SECONDS)
        self.check_deadline(self.deadline)
        return 0


def main(argv):
    if len(argv) != 3 or argv[2] not in ("future", "images"):
        print("usage: release-preflight.py SERVER_DIR NATIVE_BINARY future|images", file=sys.stderr)
        return 64
    coordinator = Coordinator()
    handlers = {}
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        handlers[signum] = signal.signal(signum, coordinator.interrupt)
    result = 70
    try:
        result = coordinator.run(*argv)
    except GateFailure as error:
        print(str(error), file=sys.stderr, flush=True)
        result = error.code
    except OSError:
        # No argv/environment/provider error bodies in coordinator diagnostics.
        print("unable to start or inspect read-only preflight worker", file=sys.stderr, flush=True)
    finally:
        if not coordinator.cleanup():
            result = 70
        for signum, handler in handlers.items():
            signal.signal(signum, handler)
    if result == 0 and coordinator.interrupted is not None:
        result = 128 + coordinator.interrupted
    return result


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
