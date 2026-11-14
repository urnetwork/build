#!/usr/bin/env python3
"""Exercise component discovery and exact executable validation without downloads."""
from pathlib import Path
import os
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


class NodeVersionsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='component-node-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.build = self.repo('build')
        self.web = self.repo('build/web')
        self.mmm = self.repo('mmm')
        self.write(self.build, '.gitmodules', '[submodule "web"]\n path = web\n')
        self.write(self.web, 'manager/Makefile', 'NODE_VERSION := v22.14.0\n')
        self.write(self.web, 'web/Makefile', 'NODE_VERSION := v22.23.2\n')
        self.write(self.mmm, 'mmm/Makefile', 'NODE_VERSION := v22.23.2\n')
        self.nvm = self.root / 'nvm'
        self.nvm.mkdir()
        (self.nvm / 'nvm.sh').write_text('''nvm() {
  [ "$1" = exec ] && [ "$3" = node ] && [ "$4" = --version ] || return 71
  printf '%s\\n' "$2" >> "$NODE_TEST_CALLS"
  [ "$2" != "$NODE_TEST_MISSING" ] || return 3
  printf '%s\\n' "${NODE_TEST_WRONG:-$2}"
}
''')

    def repo(self, name):
        path = self.root / name
        path.mkdir(parents=True)
        subprocess.run(['git', 'init', '-q', str(path)], check=True)
        return path

    def write(self, repo, name, text):
        path = repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        subprocess.run(['git', '-C', str(repo), 'add', '--', name], check=True)

    def check(self, **options):
        return subprocess.run([sys.executable, '-B', str(HERE / 'check-node-versions.py'),
                               str(self.build), str(self.mmm)], capture_output=True, text=True,
                              env={**os.environ, 'NVM_DIR': str(self.nvm),
                                   'NODE_TEST_CALLS': str(self.root / 'calls'),
                                   'NODE_TEST_MISSING': '', 'NODE_TEST_WRONG': '', **options})

    def test_exact_pins_from_components_and_standalone_are_executed_once(self):
        result = self.check()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(['v22.14.0', 'v22.23.2'], (self.root / 'calls').read_text().splitlines())
        for recipe in ('manager/Makefile', 'web/Makefile', 'mmm/Makefile'):
            self.assertIn(recipe, result.stdout)

    def test_missing_version_reports_the_owning_recipe_and_exact_install(self):
        result = self.check(NODE_TEST_MISSING='v22.14.0')
        self.assertEqual(1, result.returncode)
        self.assertIn('manager/Makefile', result.stderr)
        self.assertIn('nvm install v22.14.0', result.stderr)

    def test_wrong_executable_version_is_rejected(self):
        result = self.check(NODE_TEST_WRONG='v24.14.1')
        self.assertEqual(1, result.returncode)
        self.assertIn('v22.14.0 is required', result.stderr)

    def test_new_nested_checkout_and_version_files_are_discovered(self):
        nested = self.repo('build/web/new-component')
        self.write(self.web, '.gitmodules', '[submodule "new"]\n path = new-component\n')
        self.write(nested, '.nvmrc', '20.19.0\n')
        self.write(nested, 'tool/.node-version', '20.19.0\n')
        result = self.check(NODE_TEST_MISSING='v20.19.0')
        self.assertEqual(1, result.returncode)
        self.assertIn('.nvmrc', result.stderr)
        self.assertIn('.node-version', result.stderr)

    def test_uninitialized_optional_examples_are_not_selected(self):
        self.write(self.mmm, '.gitmodules', '[submodule "example"]\n path = optional/example\n')
        result = self.check()
        self.assertEqual(0, result.returncode, result.stderr)

    def test_dynamic_pin_fails_closed(self):
        self.write(self.web, 'manager/Makefile', 'NODE_VERSION := $(CURRENT_NODE)\n')
        result = self.check()
        self.assertEqual(1, result.returncode)
        self.assertIn('exact x.y.z pin', result.stderr)


if __name__ == '__main__':
    unittest.main()
