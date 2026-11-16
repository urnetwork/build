"""Real Git and pinned generator merges; no Brevo credentials or network."""
import json
from pathlib import Path
import shutil
import subprocess
import unittest

from test_release_sources import RealSourceProvenance, state, HERE, VERSION


class BrevoTransitions(RealSourceProvenance):
    def setUp(self):
        super().setUp()
        self.actual_mmm = HERE.parents[1] / 'mmm'
        self.write(self.root / 'mmm/.gitignore', 'node_modules\nstate/\ndist/\n')
        target = self.root / 'mmm/onboarding/lib/config.mjs'
        target.parent.mkdir(parents=True)
        shutil.copy2(self.actual_mmm / 'onboarding/lib/config.mjs', target)
        (self.root / 'mmm/onboarding/node_modules').symlink_to(
            self.actual_mmm / 'onboarding/node_modules', target_is_directory=True)
        for program in ('onboarding', 'reengagement'):
            self.write(self.root / f'mmm/{program}/build.mjs',
                       'import {fileURLToPath} from "node:url";\n'
                       'console.log(fileURLToPath(new URL("./state/brevo-templates.json",import.meta.url)));\n')
            self.write(self.root / f'config/main/{program}.yml', self.config(program, 1))
            self.write(self.root / f'mmm/{program}/state/brevo-templates.json',
                       json.dumps({'welcome': {'default': {'en': 42}}}))
        self.commit(self.root / 'mmm')
        self.commit(self.root / 'config')

    def config(self, program, identifier):
        return (f'{program}:\n  enabled: false\n  brevo:\n    templates:\n'
                f'      welcome:\n        default:\n          en: {identifier}\n')

    def check(self, before, program):
        state.allowed_transition(self.root, before, self.snapshot(), program, VERSION, before)

    def test_independent_program_commits_and_no_change_rechecks(self):
        for program in ('onboarding', 'reengagement'):
            before = self.snapshot()
            self.write(self.root / f'config/main/{program}.yml', self.config(program, 42))
            self.commit(self.root / 'config')
            self.check(before, program)
            self.check(self.snapshot(), program)

    def test_missing_state_refuses_even_if_legacy_dist_receipt_exists(self):
        before = self.snapshot()
        (self.root / 'mmm/onboarding/state/brevo-templates.json').unlink()
        self.write(self.root / 'mmm/onboarding/dist/brevo-templates.json',
                   json.dumps({'welcome': {'default': {'en': 1}}}))
        with self.assertRaisesRegex(state.Refusal, 'state is missing'):
            self.check(before, 'onboarding')

    def test_receipt_shape_rejects_non_ids_and_wrong_nesting(self):
        path = self.root / 'mmm/onboarding/state/brevo-templates.json'
        for receipt in ({}, [], {'step': 1}, {'step': {'default': {'en': True}}},
                        {'step': {'default': {'en': 0}}}, {'step': {'default': {'en': '42'}}}):
            with self.subTest(receipt=receipt):
                path.write_text(json.dumps(receipt))
                with self.assertRaisesRegex(state.Refusal, 'invalid shape'):
                    state.brevo_templates(self.root, 'onboarding')

    def test_unrelated_config_change_and_wrong_merge_are_rejected(self):
        before = self.snapshot()
        self.write(self.root / 'config/main/onboarding.yml', self.config('onboarding', 43))
        self.commit(self.root / 'config')
        with self.assertRaisesRegex(state.Refusal, 'exact pinned template-ID merge'):
            self.check(before, 'onboarding')
        before = self.snapshot()
        self.write(self.root / 'config/main/reengagement.yml', self.config('reengagement', 42))
        self.commit(self.root / 'config')
        with self.assertRaisesRegex(state.Refusal, 'unrelated path'):
            self.check(before, 'onboarding')

    def test_generator_contract_requires_no_upload_receipt(self):
        for program in ('onboarding', 'reengagement'):
            path = self.root / f'mmm/{program}/state/brevo-templates.json'
            path.unlink()
            self.assertEqual(path, state.brevo_state_path(self.root, program))

    def test_actual_generators_report_state_paths_without_network_or_migration(self):
        # Execute the real pinned CLI command with network replaced by a trap.
        guard = self.root / 'no-network.mjs'
        guard.write_text('globalThis.fetch=()=>{throw new Error("unexpected network access")};\n')
        for program in ('onboarding', 'reengagement'):
            actual = self.actual_mmm / program
            result = subprocess.run(['node', '--import', str(guard), str(actual / 'build.mjs'), 'state-path'],
                                    capture_output=True, text=True, check=True)
            self.assertEqual(str(actual / 'state/brevo-templates.json'), result.stdout.strip())

    def test_real_legacy_only_generators_do_not_migrate_receipts_during_contract_check(self):
        copied = self.root / 'legacy-only/mmm'
        for program in ('onboarding', 'reengagement'):
            shutil.copytree(self.actual_mmm / program, copied / program,
                            ignore=shutil.ignore_patterns('node_modules', 'state', 'dist'))
            legacy = copied / program / 'dist/brevo-templates.json'
            self.write(legacy, '{"legacy":{"default":{"en":7}}}')
        (copied / 'onboarding/node_modules').symlink_to(
            self.actual_mmm / 'onboarding/node_modules', target_is_directory=True)
        guard = self.root / 'legacy-no-network.mjs'
        guard.write_text('globalThis.fetch=()=>{throw new Error("unexpected network access")};\n')
        for program in ('onboarding', 'reengagement'):
            directory = copied / program
            result = subprocess.run(['node', '--import', str(guard), str(directory / 'build.mjs'), 'state-path'],
                                    capture_output=True, text=True, check=True)
            expected = directory / 'state/brevo-templates.json'
            self.assertEqual(str(expected), result.stdout.strip())
            self.assertFalse(expected.exists())
            self.assertEqual('{"legacy":{"default":{"en":7}}}',
                             (directory / 'dist/brevo-templates.json').read_text())


def load_tests(loader, tests, pattern):
    return unittest.TestSuite(BrevoTransitions(name) for name in sorted(BrevoTransitions.__dict__)
                              if name.startswith('test_'))


if __name__ == '__main__':
    unittest.main()
