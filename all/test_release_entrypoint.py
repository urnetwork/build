"""Execute the unchanged canonical wrapper AND run.sh in a disposable workspace.

Real Git repositories, source fingerprints, flock, atomic manifests, artifact
retention and stage orchestration run here. Only host/provider/build commands
are fakes. The fixture's source preparation is already sealed, like a release
that reached its first artifact stage before interruption. No live credentials,
tool setup, Git remotes, Apple APIs, Docker daemon or other network are used.
"""
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import importlib.util

HERE = Path(__file__).resolve().parent
XOPS = HERE.parents[1] / "xops"
VERSION = "2026.10.15-123456789"


FAKE_TOOL = r'''#!PYTHON
import base64, hashlib, json, os, pathlib, plistlib, subprocess, sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ["RECOVERY_FIXTURE"])
def event(text):
    with (root/"events").open("a") as f: f.write(text+"\n")
def put(path, data=b"verified synthetic artifact"):
    path=pathlib.Path(path); path.parent.mkdir(parents=True,exist_ok=True); path.write_bytes(data)
if name=="python3":
    if args and pathlib.Path(args[0]).name=="release-publication.py":
        event("publication:"+args[1])
        if args[1]=="release": print(json.dumps({"id":42})); sys.exit(0)
        if args[1]=="asset":
            # First stop after the SDK stages, then allow the real CLI stages.
            sys.exit(0 if (root/"allow-cli-publication").exists() else 89)
        raise SystemExit("unexpected publication operation")
    os.execv("PYTHON",["PYTHON",*args])
elif name=="git":
    if any(a in ("checkout","reset","pull","stash","fetch","push","submodule") for a in args):
        event("FORBIDDEN-GIT:"+" ".join(args)); sys.exit(91)
    os.execv("REALGIT",["REALGIT",*args])
elif name=="go":
    if args==["version"]: print("go version go1.27.3 darwin/arm64")
    # Probe genuine effective Go settings, including the three private-module
    # prefixes. Constant fake values previously hid a real resume-order bug.
    elif args[:2]==["env","-json"]: os.execv("REALGO",["REALGO",*args])
    elif len(args)>=2 and args[-2]=="credential": print("no")
    elif args[-2:]==["release","mobile"]:
        event("sdk-mobile")
        if not (root/"allow-mobile").exists(): sys.exit(37)
    elif args[:1]==["build"] and "-o" in args:
        if pathlib.Path.cwd().parent==root/"build/sn/v2026/cli":
            event("cli-build:"+pathlib.Path.cwd().name+":"+os.environ["GOOS"]+"/"+os.environ["GOARCH"])
        else: event("preflight-build")
        put(args[args.index("-o")+1],b"#!/bin/sh\nexit 0\n")
    else: event("FORBIDDEN-GO:"+" ".join(args)); sys.exit(92)
elif name=="make":
    event("make:"+str(pathlib.Path.cwd().relative_to(root)))
    if pathlib.Path.cwd()!=root/"build/sdk/build": sys.exit(93)
    for relative in ("sdk/build/android/URnetworkSdk.aar","sdk/build/android/URnetworkSdk-sources.jar","sdk/build/apple/URnetworkSdk.xcframework.zip","sdk/js/build/URnetworkSdkJs.zip","sdk/js/wasm/main.wasm","sdk/cgo/build/linux/arm64/liburnetwork.so","sdk/cgo/build/URnetworkSdkLinux.zip"):
        put(root/"build"/relative)
elif name=="java": print('openjdk version "21.0.9"',file=sys.stderr)
elif name=="node": print("v24.14.1")
elif name=="npm":
    if args==["--version"]: print("11.11.0")
    else: event("FORBIDDEN-NPM:"+" ".join(args)); sys.exit(94)
elif name=="xcodebuild":
    if args!=["-version"]: event("FORBIDDEN-XCODEBUILD"); sys.exit(95)
    print("Xcode synthetic 27.0")
elif name=="docker":
    if args==["context","inspect","desktop-linux","--format","{{.Endpoints.docker.Host}}"]: print("unix:///fixture/docker.sock"); sys.exit(0)
    if args==["--context","desktop-linux","ps","--quiet","--no-trunc"]: sys.exit(0)
    if args not in (["--version"],["info"]): event("FORBIDDEN-DOCKER:"+" ".join(args)); sys.exit(96)
    print("Docker synthetic")
elif name=="zig": print("synthetic zig 1")
elif name=="warpctl":
    if args==["ls","docker-namespace"]: print("bringyour")
    elif args and args[0]=="deploy" and "--preflight" in args: event("preflight:"+args[1])
    else: event("FORBIDDEN-WARP:"+" ".join(args)); sys.exit(97)
elif name=="df": print("Filesystem 1024-blocks Used Available Capacity Mounted on\nfixture 1000000000 0 1000000000 0% /fixture")
elif name=="lsof": print("p"+str(os.getpid())+"\nn/synthetic-unrelated-workspace")
elif name=="ps": pass # Hermetic owned-process inventory, never the host's VMs.
elif name=="security":
    if args[:1]==["find-identity"]:
        print('1) '+hashlib.sha1(b"fixture-certificate").hexdigest().upper()+' "Developer ID Application: Fixture (F7RQ5ZZ798)"')
    elif args[:1]==["cms"]: sys.stdout.buffer.write(pathlib.Path(args[-1]).read_bytes())
    elif args[:1]==["list-keychains"]: print('"fixture-keychain"')
elif name=="plutil":
    values=plistlib.loads(pathlib.Path(args[-1]).read_bytes()); key=args[1]
    if key.startswith("DeveloperCertificates."): print(base64.b64encode(values["DeveloperCertificates"][0]).decode())
    elif key=="DeveloperCertificates": print(len(values[key]))
    else: print(values[key])
elif name=="jq":
    if args[:2]==["-r","--arg"]: print("false"); sys.exit(0)
    value=json.load(sys.stdin)
    if args==["-r",".id"]: print(value["id"])
    else: print(json.dumps(value))
elif name=="curl": event("message-boundary") # Never contact even a synthetic URL.
elif name=="codesign": event("signing-smoke")
else:
    event("FORBIDDEN-TOOL:"+name+":"+" ".join(args)); sys.exit(98)
'''


class CanonicalEntrypoint(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="canonical-release-test-")
        cls.root = Path(cls.temp.name).resolve()
        cls.build = cls.root / "build"
        cls.bin = cls.root / "fake-tools"
        cls.bin.mkdir()
        cls.home = cls.root / "synthetic-home"
        cls.home.mkdir()
        cls.real_git = shutil.which("git")
        cls.real_go = shutil.which("go")
        if cls.real_go is None:
            raise AssertionError("canonical recovery fixture requires Go for read-only go env probes")
        cls.base_env = {"PATH": str(cls.bin)+":"+os.environ["PATH"], "HOME": str(cls.home),
                        "RECOVERY_FIXTURE": str(cls.root), "NVM_DIR": str(cls.root/"nvm"),
                        "ANDROID_HOME": str(cls.root/"android-sdk"), "LC_ALL": "C", "WARP_SKIP_DEPLOY": "1",
                        "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
                        "GOCACHE": str(cls.root/"go-cache"), "GOMODCACHE": str(cls.root/"go-modcache"),
                        "GOPRIVATE": "private.example", "GONOPROXY": "proxy.example", "GONOSUMDB": "sum.example"}
        cls.env = dict(cls.base_env, WARP_HOME=str(cls.root), BRINGYOUR_HOME=str(cls.build),
                       BUILD_HOME=str(cls.build), BUILD_TEST="", BUILD_RESET="",
                       BUILD_URIO_CHANGELOG="0", CONNECT_IP_UPDATE="1", BUILD_APPLE_IDENTITY="1",
                       BUILD_OUT=str(cls.home/"Google Drive/My Drive/URnetwork/Build"), NPM_TOKEN="synthetic-only",
                       SDK_PACKAGE_CHANNEL="nightly", FDROID_VERSION_CODE_OFFSETS="0 2 3", STAGE_SECONDS="60",
                       ANDROID_NDK_HOME=str(cls.root/"android-sdk/ndk/29.0.14206865"))
        cls.write(cls.home/".zprofile", 'export PATH="'+str(cls.bin)+':$PATH"\n')
        for name in (".keychain-pw", ".identity.p12", ".p12-pw"):
            cls.write(cls.home/name, "synthetic-only")
        cls.write(cls.root/"nvm/nvm.sh", "# synthetic nvm\n")
        cls.write(cls.root/"android-sdk/ndk/29.0.14206865/source.properties", "Pkg.Revision=29.0.14206865\n")
        cls.write(cls.home/"Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw", "synthetic sparse disk marker")
        cls.write(cls.root/"events", "")
        (cls.root/"build-logs").mkdir()
        for name in (".apple-api-key", ".apple-api-issuer", ".slack-webhook", ".github-api-key", ".virustotal-api-key", ".npm-token"):
            cls.write(cls.root/name, "synthetic-only")
            (cls.root/name).chmod(0o600)
        for index, name in enumerate(("URnetwork Download", "URnetwork Extension Download", "URnetwork Split Tunnel Download")):
            cls.write(cls.home/f".provisionprofiles/{index}.provisionprofile", plistlib.dumps({"Name":name,"UUID":str(index),"ExpirationDate":"2999-01-01T00:00:00Z","DeveloperCertificates":[b"fixture-certificate"]}))
        # Every required command exists, but unexpected execution fails closed.
        tools = ("python3 git go make java node npm xcodebuild docker zig warpctl df lsof ps security plutil jq curl codesign ffprobe geoipupdate gsed hdiutil nc npx pandoc pip productbuild productsign python qemu-img qemu-system-aarch64 rsync scp sdkmanager ssh ssh-keygen timeout xcrun").split()
        for name in tools:
            if name == "timeout":
                # The genuine bounded subprocess wrapper is safe with fake children.
                continue
            cls.write(cls.bin/name, FAKE_TOOL.replace("PYTHON",sys.executable).replace("REALGIT",cls.real_git).replace("REALGO",cls.real_go), mode=0o755)
        # Git is the busiest read boundary; use a tiny shell shim so real source
        # fingerprints do not pay a Python startup for every read-only query.
        cls.write(cls.bin/"git", '#!/bin/sh\nfor arg in "$@"; do\ncase "$arg" in checkout|reset|pull|stash|fetch|push|submodule) printf "FORBIDDEN-GIT:%s\\n" "$*" >> "$RECOVERY_FIXTURE/events"; exit 91;; esac\ndone\nexec "'+cls.real_git+'" "$@"\n',mode=0o755)
        # Canonical scripts are copied unchanged, not extracted or modified.
        cls.build.mkdir()
        # Host VM images and private SSH keys are generated inputs, not harness
        # source. Copying them makes this synthetic test consume tens of GiB.
        shutil.copytree(HERE, cls.build/"all", ignore=shutil.ignore_patterns("__pycache__", "output", ".ssh"))
        shutil.copytree(XOPS/"builder", cls.root/"xops/builder", ignore=shutil.ignore_patterns("__pycache__"))
        # A real fresh run seals AFTER canonical private-module setup. Execute
        # that exact function here; the unchanged resume entrypoint must derive
        # identical settings BEFORE its first validation, with no adoption.
        cls.env = cls.private_go_environment(cls.env)
        modules=("glog","connect","userwireguard","goidenticons","proxy","sdk","sn","server","android","apple","windows","linux","web","warp","localizations","extension")
        cls.write(cls.build/".gitmodules", "".join(f'[submodule "{name}"]\n\tpath = {name}\n' for name in modules))
        standalone=("config","vault","release","localizations","gvisor","sdk","mmm","goidenticons","connect","glog","userwireguard","examples","xops")
        for name in (*standalone, *("build/"+n for n in modules)):
            repo=cls.root/name
            repo.mkdir(parents=True,exist_ok=True)
            cls.write(repo/"tracked-input",name)
            if name=="build/sn":
                # SN's release fork must keep its sources visible. Its scoped
                # CLI output rules are independently tested in SN itself;
                # never hide this whole v2026 tree with the fixture default.
                cls.write(repo/".gitignore", "/build/\n")
                for pkg in ("miner", "validator", "snclaim"):
                    cls.write(repo/f"v2026/cli/{pkg}/.gitignore", "/build/\n")
                    cls.write(repo/f"v2026/cli/{pkg}/main.go", "package main\n")
            else:
                cls.write(repo/".gitignore", "build/\ncgo/\njs/\npackaging/\nwarpctl/build/\nv2026/\n")
            cls.commit_repo(repo)
        cls.write(cls.build/".gitignore", "".join(name+"/\n" for name in modules)+"changelogs/\n")
        cls.commit_repo(cls.build)
        for repo in [cls.build,*(cls.build/name for name in modules)]:
            subprocess.run([cls.real_git,"-C",str(repo),"remote","add","origin",str(repo)],check=True,capture_output=True)
            for offset in (0,2,3) if repo==cls.build else (0,):
                version=VERSION.rsplit("-",1)[0]+"-"+str(int(VERSION.rsplit("-",1)[1])+offset)
                subprocess.run([cls.real_git,"-C",str(repo),"tag","v"+version],check=True,capture_output=True)
        for name in ("sdk/build/go.sum","sdk/cgo/go.sum","sdk/js/go.sum"):
            cls.write(cls.build/name,"pinned generated module graph\n")
        cls.write(cls.build/"sdk/js/build/URnetworkSdkJs.zip","prepared js")
        cls.write(cls.build/"sdk/js/wasm/main.wasm","prepared wasm")
        cls.write(cls.build/"sdk/js/release/plan.json","prepared npm release")
        cls.write(cls.build/"sdk/packaging/release/plan.json",json.dumps({"version":VERSION,"registries":{name:False for name in ("npm","pypi","nuget","rubygems","crates","maven","swift","cocoapods","conan","vcpkg")}}))
        cls.write(cls.build/"warp/warpctl/build/darwin/arm64/warpctl", FAKE_TOOL.replace("PYTHON",sys.executable).replace("REALGIT",cls.real_git).replace("REALGO",cls.real_go),mode=0o755)
        cls.write(cls.build/"server/v2026/bringyourctl/run-main-lan.sh", '#!/bin/sh\nprintf "preflight:main-lan\\n" >> "$RECOVERY_FIXTURE/events"\nexit 0\n',mode=0o755)
        directory=cls.root/".release-checkpoints"/VERSION
        directory.mkdir(parents=True,mode=0o700)
        directory.parent.chmod(0o700)
        spec=importlib.util.spec_from_file_location("entrypoint_state", cls.build/"all/release-state.py")
        state=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(state)
        cls.state=state
        for name,paths in {"web":[cls.build/"web/web/build/preview.ur.xyz"],"mmm":[cls.root/"mmm/mmm/build/preview.ur.io",cls.root/"mmm/mmm/build/lag.dev"]}.items():
            for path in paths: cls.write(path/"index.html","passed candidate output")
            cls.write(directory/(name+"-candidate.json"),json.dumps({"gate":name+" make check (WEB_BUILD_CHECKS=1)","outputs":state.outputs(paths),"sources":{"synthetic":"passed candidate input"}}))
        prepared=[directory/"web-candidate.json",directory/"mmm-candidate.json",cls.build/"sdk/js/build",cls.build/"sdk/js/wasm",cls.build/"sdk/js/release",cls.build/"sdk/packaging/release/plan.json"]
        command=[sys.executable,str(cls.build/"all/release-state.py"),"locked",str(cls.root),"--",sys.executable,str(cls.build/"all/release-state.py"),"seal",str(cls.root),VERSION,*map(str,prepared)]
        result=subprocess.run(command,env=cls.env,capture_output=True,text=True,timeout=60)
        if result.returncode:
            raise AssertionError("fixture seal failed: "+result.stdout+result.stderr)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    @classmethod
    def write(cls,path,value,mode=None):
        path.parent.mkdir(parents=True,exist_ok=True)
        path.write_bytes(value if isinstance(value,bytes) else value.encode())
        if mode is not None: path.chmod(mode)

    @classmethod
    def commit_repo(cls,repo):
        for args in (("init","-q"),("config","user.name","Release Fixture"),("config","user.email","fixture@example.invalid"),("add","."),("commit","-qm","sealed fixture input")):
            subprocess.run([cls.real_git,"-C",str(repo),*args],check=True,capture_output=True)

    @classmethod
    def private_go_environment(cls,environment):
        functions=re.findall(r"(?ms)^configure_private_go_modules \(\) \{\n.*?^\}\n", (cls.build/"all/run.sh").read_text())
        if len(functions)!=1: raise AssertionError("expected one canonical private Go setup function")
        result=subprocess.run(["/bin/zsh","-eu","-c",'eval "$1"; configure_private_go_modules; exec "$2" -B -c "import json, os; print(json.dumps(dict(os.environ)))"',"fixture-private-go",functions[0],sys.executable],env=environment,capture_output=True,text=True,timeout=10)
        if result.returncode: raise AssertionError("fixture private Go configuration failed: "+result.stderr)
        configured=json.loads(result.stdout)
        return dict(environment,**{key:value for key,value in configured.items() if key in ("GOPRIVATE","GONOPROXY","GONOSUMDB") or key.startswith("GIT_CONFIG_")})

    def invoke(self,*args,environment=None):
        return subprocess.run(["/bin/zsh",str(self.root/"xops/builder/build.sh"),*args],cwd=self.root,env=self.base_env if environment is None else environment,capture_output=True,text=True,timeout=60,umask=0o077)

    def test_canonical_resume_retries_failed_stage_without_preparing_or_rebuilding_sdk(self):
        first=self.invoke("--resume",VERSION)
        self.assertEqual(first.returncode,37,first.stdout+first.stderr)
        self.assertIn("--resume "+VERSION,first.stdout+first.stderr)
        first_events=(self.root/"events").read_text().splitlines()
        self.assertEqual(first_events.count("make:build/sdk/build"),1)
        self.assertEqual(first_events.count("sdk-mobile"),1)
        self.assertNotIn("preflight:main-lan",first_events)  # fixture explicitly skips deployment
        self.assertFalse([e for e in first_events if e.startswith("FORBIDDEN")],first_events)
        self.write(self.root/"allow-mobile","dependency restored")
        # Callers may already have applied canonical process-local setup. It
        # must be idempotent, preserving exactly the sealed effective Go input.
        configured=self.private_go_environment(self.base_env)
        self.assertEqual(self.private_go_environment(configured),configured)
        second=self.invoke("--resume",VERSION,environment=configured)
        self.assertEqual(second.returncode,89,second.stdout+second.stderr)
        self.assertIn("reuse sdk: verified",second.stdout+second.stderr)
        events=(self.root/"events").read_text().splitlines()
        self.assertEqual(events.count("make:build/sdk/build"),1,events)
        self.assertEqual(events.count("sdk-mobile"),2,events)
        self.assertEqual(events.count("preflight:main-lan"),0,events)
        self.assertFalse([e for e in events if e.startswith("FORBIDDEN")],events)
        manifests=list((self.root/".release-checkpoints").glob("*/manifest.json"))
        self.assertEqual([p.parent.name for p in manifests],[VERSION])
        value=json.loads(manifests[0].read_text())
        self.assertEqual(value["stages"]["sdk"]["state"],"complete")
        self.assertEqual(value["stages"]["sdk-mobile"]["state"],"complete")
        sealed_bytes=manifests[0].read_bytes()
        drift=self.invoke("--resume",VERSION,environment=dict(self.base_env,GOFLAGS="-tags=changed_input"))
        self.assertEqual(drift.returncode,65,drift.stdout+drift.stderr)
        self.assertIn("pinned toolchain changed",drift.stdout+drift.stderr)
        self.assertEqual(manifests[0].read_bytes(),sealed_bytes)
        self.assertEqual((self.root/"events").read_text().splitlines().count("make:build/sdk/build"),1)
        self.write(self.root/"allow-cli-publication", "synthetic publication boundary ready")
        third=self.invoke("--resume",VERSION)
        # All three canonical SN CLI stages finish; stop at the next make.
        self.assertEqual(third.returncode,93,third.stdout+third.stderr)
        value=json.loads(manifests[0].read_text())
        events=(self.root/"events").read_text().splitlines()
        cli_events=[event for event in events if event.startswith("cli-build:")]
        for pkg,count in (("miner",12),("validator",4),("snclaim",4)):
            self.assertEqual(sum(event.startswith("cli-build:"+pkg+":") for event in cli_events),count)
            self.assertEqual(value["stages"]["cli-"+pkg]["state"],"complete")
        self.assertIn("make:build/server/v2026/bringyourctl",events)
        self.assertFalse([event for event in events if event.startswith("FORBIDDEN")],events)

        # Ignoring only generated output must not hide adjacent, tracked or
        # staged source edits. Exercise the real sealed source validator.
        sn=self.build/"sn"
        with mock.patch.dict(os.environ,self.env,clear=True):
            for pkg,kind in (("miner","untracked"),("validator","tracked"),("snclaim","staged")):
                file=sn/f"v2026/cli/{pkg}/"/("new.go" if kind=="untracked" else "main.go")
                self.write(file,"package main // changed product source\n")
                if kind=="staged":
                    subprocess.run([self.real_git,"-C",str(sn),"add",str(file)],check=True,capture_output=True)
                with self.assertRaisesRegex(self.state.Refusal,"changed: build/sn"):
                    self.state.validate_inputs(self.root,value)
                if kind=="untracked": file.unlink()
                else: self.write(file,"package main\n")
                if kind=="staged":
                    subprocess.run([self.real_git,"-C",str(sn),"add",str(file)],check=True,capture_output=True)
            self.state.validate_inputs(self.root,value)

        # Retained CLI artifacts restore after cleanup and never recompile.
        shutil.rmtree(sn/"v2026/cli/miner/build")
        fourth=self.invoke("--resume",VERSION)
        self.assertEqual(fourth.returncode,93,fourth.stdout+fourth.stderr)
        for pkg in ("miner","validator","snclaim"):
            self.assertIn("reuse cli-"+pkg+": verified",fourth.stdout+fourth.stderr)
        self.assertTrue((sn/"v2026/cli/miner/build/provider.tar.gz").is_file())
        events=(self.root/"events").read_text().splitlines()
        self.assertEqual([event for event in events if event.startswith("cli-build:")],cli_events)
        self.assertEqual(events.count("make:build/sdk/build"),1)
        self.assertEqual(events.count("sdk-mobile"),2)

        # A corrupt retained CLI output cannot silently become a rebuild.
        retained=Path(value["stages"]["cli-miner"]["outputs"][0]["path"])
        self.write(retained/"linux/arm64/provider","corrupt retained binary")
        corrupt=self.invoke("--resume",VERSION)
        self.assertEqual(corrupt.returncode,65,corrupt.stdout+corrupt.stderr)
        self.assertEqual([event for event in (self.root/"events").read_text().splitlines() if event.startswith("cli-build:")],cli_events)
        self.assertFalse(list((self.build/"changelogs").glob("*")))
        for logfile in (self.root/"build-logs").glob("*.log"):
            self.assertEqual(logfile.stat().st_mode & 0o777,0o600)
        self.assertEqual((self.build/"sdk/build/android/URnetworkSdk.aar").stat().st_mode & 0o777,0o644)
        # Normal no-argument entry is still the full workflow: its first pull is
        # reached, intercepted and refused by the fake Git boundary.
        normal=self.invoke()
        self.assertEqual(normal.returncode,91,normal.stdout+normal.stderr)
        self.assertIn("FORBIDDEN-GIT:stash",(self.root/"events").read_text())


if __name__=="__main__":
    unittest.main()
