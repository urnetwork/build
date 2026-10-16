"""Real Git provenance and expected-transition fixtures, with no network."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location("source_state",HERE/"release-state.py")
state=importlib.util.module_from_spec(spec)
spec.loader.exec_module(state)
VERSION="2026.10.15-123456789"


class RealSourceProvenance(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory(prefix="release-source-test-")
        self.addCleanup(temporary.cleanup)
        self.root=Path(temporary.name).resolve()
        self.repos={name:self.root/name for name in ("config","build","build/android","mmm")}
        for name,repo in self.repos.items():
            repo.mkdir(parents=True,exist_ok=True)
            self.git(repo,"init","-q")
            self.git(repo,"config","user.name","Release Fixture")
            self.git(repo,"config","user.email","fixture@example.invalid")
            self.write(repo/"source.txt","sealed input\n")
            self.write(repo/".gitignore","sdk/\nandroid/\nbuild/\n")
            self.commit(repo)
        for name in ("app/settings.gradle","app/app/build.gradle"):
            self.write(self.repos["build/android"]/name,"keep this line\n dependency /* build: google */\n")
        self.commit(self.repos["build/android"])
        self.pin_android()
        for name in ("sdk/build/go.sum","sdk/cgo/go.sum","sdk/js/go.sum"):
            self.write(self.repos["build"]/name,"sealed graph\n")

    def write(self,path,text):
        path.parent.mkdir(parents=True,exist_ok=True)
        path.write_text(text)

    def git(self,repo,*args):
        return subprocess.check_output(["git","-C",str(repo),*args],stderr=subprocess.PIPE).decode().strip()

    def commit(self,repo):
        self.git(repo,"add",".")
        self.git(repo,"commit","-qm","fixture source transition")

    def pin_android(self):
        head=self.git(self.repos["build/android"],"rev-parse","HEAD")
        self.git(self.repos["build"],"update-index","--add","--cacheinfo","160000,"+head+",android")
        self.git(self.repos["build"],"commit","-qm","fixture exact Android pin")

    def snapshot(self):
        with mock.patch.object(state,"repositories",return_value=self.repos):
            return state.source_snapshot(self.root)

    def canonical_symlink(self, name):
        temporary = tempfile.TemporaryDirectory(prefix="release-external-" + name + "-")
        self.addCleanup(temporary.cleanup)
        target = Path(temporary.name).resolve() / name
        canonical = self.root / name
        if canonical.exists():
            canonical.rename(target)
        else:
            target.mkdir()
            self.git(target, "init", "-q")
            self.git(target, "config", "user.name", "Release Fixture")
            self.git(target, "config", "user.email", "fixture@example.invalid")
        self.write(target / "private-fixture.txt", "private-fixture-input\n")
        (target / "private-fixture.txt").chmod(0o600)
        self.commit(target)
        canonical.symlink_to(target, target_is_directory=True)
        self.repos[name] = canonical
        return canonical, target

    def validate_sources(self, sealed):
        with mock.patch.object(state, "repositories", return_value=self.repos):
            return state.validate_inputs(self.root, sealed)

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_canonical_config_and_vault_symlinks_accept_equivalent_selections(self):
        for name in ("config", "vault"):
            with self.subTest(root=name):
                canonical, target = self.canonical_symlink(name)
                self.assertFalse(target.is_relative_to(self.root))
                self.write(self.root / "warp.json", "{}")
                original = state.deployment_snapshot(self.root)
                snapshot = self.snapshot()
                self.assertEqual(snapshot[name]["head"], self.git(target, "rev-parse", "HEAD"))
                self.assertNotIn("private-fixture-input", json.dumps(snapshot))
                for selected in (canonical, target):
                    with self.subTest(selected=selected):
                        self.write(self.root / "warp.json", json.dumps({name + "Home": str(selected)}))
                        self.assertEqual(state.deployment_snapshot(self.root), original)
                        self.write(self.root / "warp.json", "{}")
                        with mock.patch.dict(os.environ, {"WARP_" + name.upper() + "_HOME": str(selected)}):
                            self.assertEqual(state.deployment_snapshot(self.root), original)

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_canonical_symlink_retarget_rejects_resume_even_with_identical_git_contents(self):
        for name in ("config", "vault"):
            with self.subTest(root=name):
                canonical, target = self.canonical_symlink(name)
                self.write(self.root / "warp.json", json.dumps({name + "Home": str(canonical)}))
                original = self.snapshot()
                sealed = {"sources": original, "environment": state.environment_snapshot()}
                self.assertEqual(self.validate_sources(sealed), original)
                other = target.with_name(name + "-other")
                shutil.copytree(target, other)
                canonical.unlink()
                canonical.symlink_to(other, target_is_directory=True)
                changed = self.snapshot()
                self.assertEqual(changed[name], original[name], "the copied Git input is identical")
                self.assertNotEqual(changed["deployment-inputs"], original["deployment-inputs"])
                with self.assertRaisesRegex(state.Refusal, "changed: deployment-inputs"):
                    self.validate_sources(sealed)
                self.assertEqual(sealed["sources"], original, "validation must not adopt a new root")
                canonical.unlink()
                canonical.symlink_to(target, target_is_directory=True)
                self.write(self.root / "warp.json", "{}")

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_canonical_vault_symlink_tracks_private_content_without_storing_it(self):
        _, target = self.canonical_symlink("vault")
        original = self.snapshot()
        sealed = {"sources": original, "environment": state.environment_snapshot()}
        for mutation in ("tracked", "staged", "untracked"):
            with self.subTest(mutation=mutation):
                file = target / ("new-private-fixture.txt" if mutation == "untracked" else "private-fixture.txt")
                self.write(file, "changed-private-fixture-input\n")
                file.chmod(0o600)
                if mutation == "staged":
                    self.git(target, "add", file.name)
                changed = self.snapshot()
                self.assertEqual(changed["deployment-inputs"], original["deployment-inputs"])
                self.assertNotEqual(changed["vault"], original["vault"])
                self.assertNotIn("changed-private-fixture-input", json.dumps(changed))
                with self.assertRaisesRegex(state.Refusal, "changed: vault"):
                    self.validate_sources(sealed)
                if mutation == "untracked":
                    file.unlink()
                else:
                    self.write(file, "private-fixture-input\n")
                    if mutation == "staged":
                        self.git(target, "add", file.name)
                self.assertEqual(self.validate_sources(sealed), original)

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_alternate_config_and_vault_targets_remain_rejected(self):
        for name in ("config", "vault"):
            with self.subTest(root=name):
                canonical, target = self.canonical_symlink(name)
                other = target.with_name(name + "-alternate")
                shutil.copytree(target, other)
                variable = "WARP_" + name.upper() + "_HOME"
                self.write(self.root / "warp.json", "{}")
                with mock.patch.dict(os.environ, {variable: str(other)}):
                    with self.assertRaisesRegex(state.Refusal, "alternate " + name + " root"):
                        state.deployment_snapshot(self.root)
                self.write(self.root / "warp.json", json.dumps({name + "Home": str(other)}))
                # A canonical environment selection must not hide a different
                # JSON selection, since Warp JSON takes precedence.
                with mock.patch.dict(os.environ, {variable: str(canonical)}):
                    with self.assertRaisesRegex(state.Refusal, "alternate " + name + " root"):
                        state.deployment_snapshot(self.root)
                self.write(self.root / "warp.json", "{}")

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_external_gowork_symlink_is_still_rejected(self):
        temporary = tempfile.TemporaryDirectory(prefix="release-external-gowork-")
        self.addCleanup(temporary.cleanup)
        target = Path(temporary.name).resolve() / "go.work"
        target.write_text("go 1.27.2\n")
        work = self.root / "go.work"
        work.symlink_to(target)
        values = {key: "" for key in state.GO_ENV_KEYS}
        values.update(GOENV="off", GOWORK=str(work))
        with mock.patch.object(state, "run", return_value=json.dumps(values).encode()):
            with self.assertRaisesRegex(state.Refusal, "external GOWORK"):
                state.go_environment_snapshot(self.root)

    def test_tracked_staged_untracked_standalone_and_ignored_module_graph_changes_detected(self):
        original=self.snapshot()
        for path in (self.root/"mmm/source.txt",self.root/"config/source.txt",self.root/"build/sdk/cgo/go.sum"):
            before=path.read_text()
            self.write(path,"changed product input\n")
            self.assertNotEqual(self.snapshot(),original,path)
            self.write(path,before)
            self.assertEqual(self.snapshot(),original)
        self.write(self.root/"mmm/new-source.js","new input\n")
        self.assertNotEqual(self.snapshot(),original)
        self.git(self.root/"mmm","add","new-source.js")
        self.assertNotEqual(self.snapshot(),original)
        self.write(self.root/"mmm/build/generated.js","ignored generated scratch\n")
        before=self.snapshot()
        self.write(self.root/"mmm/build/generated.js","different generated scratch\n")
        self.assertEqual(self.snapshot(),before)

    def test_deployment_settings_are_pinned_without_refreshable_auth_bytes(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            settings = {"dockerNamespace":"bringyour", "dockerHubUsername":"fixture-login", "dockerHubToken":"never-store-fixture-token"}
            self.write(self.root/"warp.json",json.dumps(settings))
            self.write(self.root/"version.json",json.dumps({"stagedVersion":"2026.10.15","stagedVersionCode":123456789}))
            original = self.snapshot()
            self.assertNotIn("fixture-token",str(original))
            self.write(self.root/"warp.json",json.dumps(dict(settings,dockerHubToken="rotated-auth")))
            self.assertEqual(self.snapshot(),original)
            self.write(self.root/"warp.json",json.dumps(dict(settings,dockerNamespace="different")))
            self.assertNotEqual(self.snapshot(),original)
            self.write(self.root/"warp.json",json.dumps(settings))
            self.write(self.root/"version.json",json.dumps({"stagedVersion":"2026.10.15","stagedVersionCode":123456790}))
            self.assertNotEqual(self.snapshot(),original)
            os.environ["WARP_CONFIG_HOME"] = str(self.root/"untracked-config")
            with self.assertRaisesRegex(state.Refusal,"alternate config root"):
                self.snapshot()

    def test_exact_fdroid_transform_and_original_restore_are_allowed(self):
        before=self.snapshot()
        original_android=before["build/android"]["head"]
        for path in ("app/settings.gradle","app/app/build.gradle"):
            self.write(self.root/"build/android"/path,"keep this line\n/*ungoogled*/\n")
        self.commit(self.root/"build/android")
        self.pin_android()
        prepared=self.snapshot()
        state.allowed_transition(self.root,before,prepared,"fdroid-prepare",VERSION,before)
        self.git(self.root/"build/android","checkout","--detach",original_android)
        self.pin_android()
        restored=self.snapshot()
        state.allowed_transition(self.root,prepared,restored,"fdroid-restore",VERSION,before)
        self.assertEqual(restored["build/android"]["head"],original_android)

    def test_fdroid_whitelisted_filename_does_not_bless_unrelated_content(self):
        before=self.snapshot()
        for path in ("app/settings.gradle","app/app/build.gradle"):
            self.write(self.root/"build/android"/path,"silently changed product behavior\n/*ungoogled*/\n")
        self.commit(self.root/"build/android")
        self.pin_android()
        with self.assertRaisesRegex(state.Refusal,"exact reviewed"):
            state.allowed_transition(self.root,before,self.snapshot(),"fdroid-prepare",VERSION,before)

    def test_ip_transition_rejects_unrelated_source_dirty_index_and_extra_ancestry(self):
        before=self.snapshot()
        for path in ("mmdb","arindb"):
            self.write(self.root/"config/all"/path/VERSION.replace("-","+",1)/"database.mmdb","verified generated data\n")
        self.commit(self.root/"config")
        valid=self.snapshot()
        state.allowed_transition(self.root,before,valid,"ip-databases",VERSION,before)
        self.write(self.root/"mmm/source.txt","unrelated dirty product edit\n")
        with self.assertRaisesRegex(state.Refusal,"unexpected source mutation"):
            state.allowed_transition(self.root,before,self.snapshot(),"ip-databases",VERSION,before)
        self.write(self.root/"mmm/source.txt","sealed input\n")
        self.write(self.root/"config/source.txt","unrelated committed change\n")
        self.commit(self.root/"config")
        with self.assertRaisesRegex(state.Refusal,"unexpected commit/rebase"):
            state.allowed_transition(self.root,before,self.snapshot(),"ip-databases",VERSION,before)
        dirty=self.snapshot()
        self.write(self.root/"config/source.txt","unrelated staged change\n")
        self.git(self.root/"config","add","source.txt")
        staged=self.snapshot()
        self.commit(self.root/"config")
        with self.assertRaisesRegex(state.Refusal,"started with tracked edits"):
            state.allowed_transition(self.root,staged,self.snapshot(),"ip-databases",VERSION,dirty)

    def test_exact_remote_ref_is_revalidated_and_main_may_advance(self):
        repo=self.root/"config"
        remote=self.root/"remote.git"
        subprocess.run(["git","init","--bare","-q",str(remote)],check=True)
        self.git(repo,"remote","add","origin",str(remote))
        head=self.git(repo,"rev-parse","HEAD")
        tag="refs/tags/v"+VERSION
        self.git(repo,"tag","-a","v"+VERSION,"-m","immutable")
        self.git(repo,"push","origin",tag)
        expected={"config":{"remote":state.digest(state.git(repo,"remote","get-url","origin")),"refs":{tag:head}}}
        state.verify_published_refs(self.root,expected)
        self.write(repo/"source.txt","later main change\n")
        self.commit(repo)
        self.git(repo,"push","origin","HEAD:refs/heads/main")
        state.verify_published_refs(self.root,expected)
        self.git(remote,"update-ref",tag,self.git(repo,"rev-parse","HEAD"))
        with self.assertRaisesRegex(state.Refusal,"tag changed"):
            state.verify_published_refs(self.root,expected)


if __name__=="__main__": unittest.main()
