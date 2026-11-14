#!/usr/bin/env python3
"""Check exact Node pins in tracked component recipes without installing tools."""
from pathlib import Path
import os
import re
import subprocess
import sys


def git(repo, *args):
    return subprocess.check_output(['git', '-C', str(repo), *args])


def repositories(roots):
    seen = set()
    pending = [Path(root).resolve() for root in roots]
    while pending:
        repo = pending.pop()
        if repo in seen:
            continue
        seen.add(repo)
        yield repo
        modules = repo / '.gitmodules'
        if modules.exists():
            result = subprocess.run(['git', 'config', '--file', str(modules),
                                     '--get-regexp', r'\.path$'], capture_output=True, text=True)
            if result.returncode not in (0, 1):
                raise RuntimeError(f'cannot read submodule paths in {modules}')
            # Standalone MMM also declares optional example submodules that
            # releases do not initialize or build. Inspect selected checkouts.
            for line in result.stdout.splitlines():
                child = repo / line.split(None, 1)[1]
                if (child / '.git').exists():
                    pending.append(child)


def node_pins(roots):
    pins = {}
    for repo in repositories(roots):
        files = git(repo, 'ls-files', '-z', '--', ':(glob)**/Makefile',
                    ':(glob)**/.nvmrc', ':(glob)**/.node-version').decode().split('\0')
        for name in filter(None, files):
            path = repo / name
            text = path.read_text()
            if path.name == 'Makefile':
                values = re.findall(r'^\s*NODE_VERSION\s*(?::=|[?+]?=)\s*([^#\n]*)', text, re.M)
            else:
                values = [text.split('#', 1)[0].strip()]
            for value in values:
                version = value.strip()
                if not re.fullmatch(r'v?\d+\.\d+\.\d+', version):
                    raise RuntimeError(f'{path}: Node version must be an exact x.y.z pin, got {version!r}')
                version = 'v' + version.removeprefix('v')
                pins.setdefault(version, []).append(str(path))
    return pins


def main():
    if len(sys.argv) < 2:
        raise RuntimeError('usage: check-node-versions.py <build-repo> [standalone-repo ...]')
    pins = node_pins(sys.argv[1:])
    missing = []
    for version, sources in sorted(pins.items()):
        result = subprocess.run([
            'bash', '-c', '. "$NVM_DIR/nvm.sh" --no-use && nvm exec "$1" node --version',
            'check-component-node', version,
        ], capture_output=True, text=True, timeout=30)
        lines = result.stdout.strip().splitlines()
        if result.returncode or not lines or lines[-1] != version:
            missing.append(version)
            print(f'Node {version} is required by ' + ', '.join(sources), file=sys.stderr)
            print(f'Install it before release preparation: nvm install {version}', file=sys.stderr)
        else:
            print(f'component Node {version}: ready (' + ', '.join(sources) + ')')
    return 1 if missing else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        print('component Node preflight failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
