"""Cross-repository release regression, with no provider/network writes.

Run explicitly with a real, already-built Warpctl and the MMM source checkout:
  python3 -B all/mmm_release_inputs_test.py --warpctl /path/to/warpctl --mmm /path/to/mmm

The canonical release command, Warpctl, all three Makefiles and network-stats
generator are real. Package/compiler/browser/image boundaries are fixtures;
the feed is a deterministic in-process fetch, never the production service.
"""

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


RUN = Path(__file__).with_name("run.sh")

BOUNDARY = r'''
import json, os, pathlib, shutil, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["EVENTS"], "a") as stream:
    stream.write(json.dumps({"name": name, "args": args,
        "preserve": os.environ.get("UR_PRESERVE_NETWORK_STATS"),
        "gowork": os.environ.get("GOWORK")}) + "\n")
if name + ":" + " ".join(args) == os.environ.get("FAIL_COMMAND"):
    sys.exit(37)
if name == "make":
    if args == ["warp_build_image"]:
        pathlib.Path(os.environ["PUBLISHED"]).write_text("fixture image only\n")
        sys.exit(0)
    os.execv(os.environ["REAL_MAKE"], [os.environ["REAL_MAKE"], *args])
if name == "node" and args == ["../react/scripts/generate-network-stats.mjs"]:
    os.execv(os.environ["REAL_NODE"], [os.environ["REAL_NODE"], "--import",
        os.environ["FETCH_PRELOAD"], *args])
if name == "node" and args[:1] == ["scripts/stage-build.mjs"]:
    shutil.copytree(args[1], args[2])
if name == "npx" and args == ["astro", "build"]:
    pathlib.Path("dist").mkdir(exist_ok=True)
    snapshot = pathlib.Path("../react/src/data/networkStats.js")
    pathlib.Path("dist/index.html").write_bytes(snapshot.read_bytes() if snapshot.exists() else b"lag.dev\n")
if name == "go" and args[:1] == ["build"]:
    target = pathlib.Path(args[args.index("-o") + 1])
    target.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(os.environ["REAL_WARPCTL"], target / "mmm")
'''


class MmmReleaseInputs(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="mmm-release-inputs-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.service = self.root / "mmm/mmm"
        self.build = self.root / "build"
        self.build.mkdir()
        for relative in ("mmm/Makefile", "mmm/go.mod", "ur.io/astro/Makefile",
                         "lag.dev/astro/Makefile", "ur.io/react/scripts/generate-network-stats.mjs",
                         "ur.io/react/src/data/networkStatsFeed.js"):
            target = self.root / "mmm" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(MMM / relative, target)
        (self.root / "mmm/lag.dev/react").mkdir(parents=True)
        (self.root / "mmm/bringyour.com/res/emails").mkdir(parents=True)
        (self.root / "mmm/ur.io/react/package.json").write_text('{"type":"module"}\n')
        self.snapshot = self.root / "mmm/ur.io/react/src/data/networkStats.js"
        self.snapshot.write_text("// previous candidate snapshot\n")
        self.preload = self.root / "fetch.mjs"
        self.preload.write_text('''import { appendFileSync } from "node:fs";
globalThis.fetch = async () => {
  appendFileSync(process.env.FETCHES, "fetch\\n");
  return { ok: true, json: async () => ({ online_providers: 12, countries: 3, total_networks: 45 }) };
};
''')
        fake = self.root / "bin"
        fake.mkdir()
        for name in ("make", "node", "npm", "npx", "go", "pip", "govulncheck", "docker"):
            command = fake / name
            command.write_text(f"#!{sys.executable}\n" + BOUNDARY)
            command.chmod(0o755)
        (fake / "warpctl").symlink_to(WARPCTL)
        nvm = self.root / "nvm"
        nvm.mkdir()
        (nvm / "nvm.sh").write_text("nvm() { return 0; }\n")
        (self.root / "warp.json").write_text(json.dumps({"dockerNamespace": "fixture"}))
        (self.root / "version.json").write_text(json.dumps({
            "stagedVersion": "2026.10.17", "stagedVersionCode": 1230}))
        self.events = self.root / "events.jsonl"
        self.fetches = self.root / "fetches"
        self.published = self.root / "published"
        # Deliberately do not inherit host Warp/auth/build settings.
        self.env = {"PATH": str(fake) + os.pathsep + os.defpath,
                    "REAL_MAKE": shutil.which("make"), "REAL_NODE": shutil.which("node"),
                    "REAL_WARPCTL": str(WARPCTL), "FETCH_PRELOAD": str(self.preload),
                    "FETCHES": str(self.fetches), "EVENTS": str(self.events),
                    "PUBLISHED": str(self.published), "NVM_DIR": str(nvm),
                    "WARP_HOME": str(self.root), "BUILD_HOME": str(self.build),
                    "BUILD_ENV": "main", "mmm_go_work_dir": str(self.root / "workspace"),
                    "PUBLIC_WALLETCONNECT_PROJECT_ID": "synthetic-public-project",
                    "PUBLIC_STRIPE_PUBLISHABLE_KEY": "", "MAKEFLAGS": ""}
        commands = [line for line in RUN.read_text().splitlines()
                    if line.startswith('(cd $BUILD_HOME && GOWORK=') and '"$WARP_HOME/mmm/mmm/Makefile")' in line]
        self.assertEqual(len(commands), 1, "must exercise exactly the canonical final MMM command")
        self.command = commands[0]

    def run_shell(self, source, **overrides):
        return subprocess.run(["zsh", "-f", "-c", source], env={**self.env, **overrides},
                              text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                              timeout=40)

    def read_events(self):
        return [json.loads(line) for line in self.events.read_text().splitlines()]

    def test_candidate_refresh_then_canonical_warp_build_preserves_snapshot(self):
        candidate = '(cd "$WARP_HOME/mmm/mmm" && URNETWORK_ROOT="$BUILD_HOME" make check)'
        self.assertIn(candidate, RUN.read_text(), "candidate generation must remain enabled")
        result = self.run_shell(candidate)
        self.assertEqual(result.returncode, 0, result.stdout)
        frozen = self.snapshot.read_bytes()
        self.assertIn(b'"providers": 12', frozen)
        self.assertEqual(self.fetches.read_text(), "fetch\n")
        self.assertFalse(self.published.exists())
        # This changes the feed result so the before-fix control cannot pass
        # accidentally just because both generators saw the same live values.
        self.preload.write_text(self.preload.read_text().replace("providers: 12", "providers: 987"))
        result = self.run_shell(self.command + '\nresult=$?\n'
            '[[ "$UR_PRESERVE_NETWORK_STATS" == caller-value ]] || exit 53\nexit $result',
            UR_PRESERVE_NETWORK_STATS="caller-value")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.snapshot.read_bytes(), frozen, "post-seal Warp/Make changed the snapshot")
        self.assertEqual(self.fetches.read_text(), "fetch\n", "final build fetched the live feed")
        self.assertEqual((self.service / "build/preview.ur.io/index.html").read_bytes(), frozen)
        self.assertTrue((self.service / "build/lag.dev/index.html").is_file())
        self.assertTrue(self.published.is_file())
        events = self.read_events()
        final_make = next(event for event in events if event["name"] == "make" and event["args"] == ["all"])
        self.assertEqual(final_make["preserve"], "1", "Warpctl discarded the canonical environment")
        generator = [event for event in events if event["name"] == "node" and
                     event["args"] == ["../react/scripts/generate-network-stats.mjs"]]
        self.assertEqual([event["preserve"] for event in generator], [None, "1"])
        self.assertEqual(generator[-1]["gowork"], "off", "site helper lost its module isolation")
        scans = [event for event in events if event["name"] == "govulncheck"]
        self.assertEqual(len(scans), 2, "both Linux binaries must retain the vulnerability gate")

    def test_real_generator_failure_prevents_image_publication(self):
        parser = self.root / "mmm/ur.io/react/src/data/networkStatsFeed.js"
        parser.write_text("throw new Error('synthetic generator dependency failure');\n")
        result = self.run_shell(self.command)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertFalse(self.published.exists())
        self.assertFalse(any(event["name"] == "govulncheck" for event in self.read_events()))

    def test_vulnerability_failure_prevents_image_publication(self):
        first_binary = str(self.service / "build/linux/amd64/mmm")
        result = self.run_shell(self.command, FAIL_COMMAND="govulncheck:-mode=binary " + first_binary)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertFalse(self.published.exists())
        self.assertTrue(any(event["name"] == "govulncheck" for event in self.read_events()))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--warpctl", type=Path, required=True)
    parser.add_argument("--mmm", type=Path, required=True)
    args, remaining = parser.parse_known_args()
    WARPCTL, MMM = args.warpctl.resolve(), args.mmm.resolve()
    if not WARPCTL.is_file() or not (MMM / "mmm/Makefile").is_file():
        parser.error("real Warpctl binary and MMM source checkout are required; no skipped coverage")
    unittest.main(argv=[sys.argv[0], *remaining])
