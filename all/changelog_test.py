"""Deterministic regressions for changelog API consumption; no network access."""
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

spec = importlib.util.spec_from_file_location('changelog', Path(__file__).with_name('changelog.py'))
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class ChangelogTests(unittest.TestCase):
    def setUp(self):
        c.configure_api(0)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name)
        self.run_git('init', '-q')

    def run_git(self, *args):
        # Use the user's global identity, never configure or override it.
        env = dict(os.environ, GIT_AUTHOR_DATE='2026-01-01T00:00:00Z',
                   GIT_COMMITTER_DATE='2026-01-01T00:00:00Z')
        return subprocess.check_output(['git', '-C', str(self.repo), *args], env=env).decode().strip()

    def commit(self, path, message):
        target = self.repo / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(message)
        self.run_git('add', '--', path)
        self.run_git('commit', '-q', '-m', message)
        return self.run_git('rev-parse', 'HEAD')

    def test_diverged_range_and_store_files_need_no_api(self):
        root = self.commit('root.go', 'Initial shared ancestor')
        base = self.commit('stamp.txt', '2026.1.1-1230')
        self.run_git('checkout', '-q', '--detach', root)
        ci = self.commit('.github/workflows/check.yml', 'Add a workflow to verify client builds')
        app = self.commit('app/main.go', 'Improve reconnect behavior on unstable networks\n\nPreserve the session.')
        with patch.object(c.urllib.request, 'urlopen', side_effect=AssertionError('network forbidden')):
            commits = c.compare('urnetwork/android', base, app, None, repo=str(self.repo))
            self.assertEqual([x['sha'] for x in commits], [ci, app])
            self.assertIn('Preserve the session.', commits[-1]['commit']['message'])
            section = dict(name='android', slug='urnetwork/android', repo=str(self.repo),
                           commits=list(reversed(commits)))
            lines, _, held = c.store_bullets([section], ['android'], path_check=True, max_candidates=0)
            self.assertEqual(lines, ['Improve reconnect behavior on unstable networks'])
            self.assertEqual(held['CI, test, docs or build files only'], 1)
            self.assertEqual(c._API_REQUESTS, 0)

    def test_all_outputs_and_missing_component_without_network(self):
        component = self.repo / 'android'
        component.mkdir()
        subprocess.check_call(['git', '-C', str(component), 'init', '-q'])
        build_repo = self.repo
        self.repo = component
        base = self.commit('app/main.go', 'Initial client implementation')
        self.repo = build_repo
        (self.repo / '.gitmodules').write_text('[submodule "android"]\n path = android\n url = https://github.com/urnetwork/android.git\n')
        self.run_git('add', '.gitmodules')
        self.run_git('update-index', '--add', '--cacheinfo', '160000,' + base + ',android')
        self.run_git('commit', '-q', '-m', 'Pin initial release')
        self.run_git('tag', 'v2026.1.1-1230')
        self.repo = component
        self.commit('.github/workflows/check.yml', 'Add a workflow to verify client builds')
        self.commit('app/main.go', 'Improve reconnect behavior on unstable networks')
        self.repo = build_repo
        notes = self.repo / 'notes'
        with patch.object(c.urllib.request, 'urlopen', side_effect=AssertionError('network forbidden')):
            self.assertEqual(c.main(['--repo', str(self.repo), '--from', 'v2026.1.1-1230',
                                     '--notes-dir', str(notes), '--notes-prefix', 'release']), 0)
            self.assertEqual(len(list(notes.iterdir())), 6)
            self.assertIn('Improve reconnect', (notes / 'release_Android.txt').read_text())
            self.assertNotIn('workflow', (notes / 'release_Android.txt').read_text())
            self.assertIn('workflow', (notes / 'release_Full.md').read_text())
            # An unavailable base must be reported, with zero network attempts.
            self.run_git('update-index', '--cacheinfo', '160000,' + '1' * 40 + ',android')
            self.run_git('commit', '-q', '-m', 'Pin unavailable historical object')
            self.assertEqual(c.main(['--repo', str(self.repo), '--from', 'HEAD',
                                     '--notes-dir', str(notes), '--notes-prefix', 'missing']), 0)
            self.assertIn('Not walked', (notes / 'missing_Full.md').read_text())
            self.assertIn('android', (notes / 'missing_Full.md').read_text())
            self.assertNotIn('No component changed', (notes / 'missing_Full.md').read_text())
            component.rename(self.repo / 'unavailable')
            self.assertEqual(c.main(['--repo', str(self.repo), '--from', 'v2026.1.1-1230',
                                     '--notes-dir', str(notes), '--notes-prefix', 'absent']), 0)
            self.assertIn('current checkout unavailable', (notes / 'absent_Full.md').read_text())
            self.assertEqual(c._API_REQUESTS, 0)

    def test_zero_budget_keeps_unchecked_candidate(self):
        section = dict(name='android', slug='urnetwork/android', commits=[
            dict(sha='1' * 40, commit=dict(message='Improve reconnect behavior on unstable networks'))])
        with patch.object(c.urllib.request, 'urlopen', side_effect=AssertionError('network forbidden')):
            lines, _, _ = c.store_bullets([section], ['android'], path_check=True)
            self.assertEqual(len(lines), 1)
            # Failure cache prevents repeating lookups across audiences.
            with patch.object(c, 'commit_files', side_effect=AssertionError('repeat lookup')):
                self.assertEqual(c.store_bullets([section], ['android'], path_check=True)[0], lines)

    def test_budget_counts_retries_across_endpoints(self):
        c.configure_api(2)
        error = urllib.error.HTTPError('https://api.github.com/test', 503, 'unavailable', {}, io.BytesIO(b''))
        with patch.object(c.urllib.request, 'urlopen', side_effect=error) as request, patch.object(c.time, 'sleep'):
            with self.assertRaises(c.ApiError):
                c.api_get('/test', None)
            with self.assertRaises(c.ApiError):
                c.api_get('/other', None)
            self.assertEqual(request.call_count, 2)

    def test_rate_limit_stops_all_further_requests(self):
        for status in (403, 429):
            c.configure_api(25)
            error = urllib.error.HTTPError('https://api.github.com/test', status, 'limited', {}, io.BytesIO(b'rate limit'))
            with patch.object(c.urllib.request, 'urlopen', side_effect=error) as request, patch.object(c.time, 'sleep') as sleep:
                for endpoint in ('/test', '/other'):
                    with self.assertRaises(c.ApiError):
                        c.api_get(endpoint, None)
                self.assertEqual(request.call_count, 1)
                sleep.assert_not_called()

    def test_successful_last_request_stops_before_next_endpoint(self):
        c.configure_api(25)
        response = io.BytesIO(b'{"commits": []}')
        response.headers = {'X-RateLimit-Remaining': '0'}
        with patch.object(c.urllib.request, 'urlopen', return_value=response) as request:
            self.assertEqual(c.api_get('/test', None), {'commits': []})
            with self.assertRaises(c.ApiError):
                c.api_get('/other', None)
            self.assertEqual(request.call_count, 1)

    def test_merge_files_use_first_parent_and_keep_mixed_changes(self):
        root = self.commit('app/main.go', 'Initial client implementation')
        self.assertEqual(c.local_commit_files(str(self.repo), root), ['app/main.go'])
        self.run_git('checkout', '-q', '-b', 'feature')
        self.commit('.github/workflows/check.yml', 'Add a workflow to verify client builds')
        self.commit('app/feature.go', 'Improve reconnect behavior on unstable networks')
        feature = self.run_git('rev-parse', 'HEAD')
        self.run_git('checkout', '-q', '--detach', root)
        self.commit('app/other.go', 'Improve connection recovery after sleep')
        self.run_git('merge', '-q', '--no-ff', '-m', 'Improve client behavior with the new feature', feature)
        head = self.run_git('rev-parse', 'HEAD')
        self.assertEqual(c.local_commit_files(str(self.repo), head),
                         ['.github/workflows/check.yml', 'app/feature.go'])
        commits = c.local_compare(str(self.repo), root, head)
        self.assertEqual(len(commits[-1]['parents']), 2)
        self.assertEqual(c.is_filtered(commits[-1], ['merge']), 'merge')
        section = dict(name='android', slug='urnetwork/android', repo=str(self.repo), commits=[commits[-1]])
        self.assertEqual(len(c.store_bullets([section], ['android'], path_check=True)[0]), 1)

    def test_empty_shallow_and_uninitialized_ranges(self):
        sha = self.commit('app/main.go', 'Initial client implementation')
        self.assertEqual(c.local_compare(str(self.repo), sha, sha), [])
        self.assertIsNone(c.local_compare(str(self.repo), '1' * 40, sha))
        empty = self.repo / 'empty'
        empty.mkdir()
        self.assertIsNone(c.local_compare(str(empty), sha, sha))
        (self.repo / '.git/shallow').write_text(sha + '\n')
        self.assertIsNone(c.local_compare(str(self.repo), sha, sha))
        self.assertIsNone(c.local_commit_files(str(self.repo), sha))


if __name__ == '__main__':
    unittest.main()
