#!/usr/bin/env python3
"""Regression test for release source metadata across different CI checkouts."""
import os
from pathlib import Path
import stat
import subprocess
import tempfile


with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    (root / 'LICENSES' / 'dependencies').mkdir(parents=True)
    license_file = root / 'LICENSES' / 'dependencies' / 'LICENSE'
    license_file.write_text('same license bytes\n')
    executable = root / 'run.sh'
    executable.write_text('#!/bin/sh\nexit 0\n')
    os.chmod(executable, 0o755)
    subprocess.run(['git', 'init', '-q', str(root)], check=True)
    subprocess.run(['git', '-C', str(root), 'add', '.'], check=True)

    os.chmod(license_file, 0o666)
    os.chmod(executable, 0o666)
    os.chmod(root / 'LICENSES' / 'dependencies', 0o777)
    subprocess.run(['python3', str(Path(__file__).with_name('normalize-source-times.py')),
                    str(root), '1700000000'], check=True)

    for path, expected in [(license_file, 0o644), (executable, 0o755),
                           (root / 'LICENSES' / 'dependencies', 0o755)]:
        assert stat.S_IMODE(path.stat().st_mode) == expected, path
        assert int(path.stat().st_mtime) == 1700000000, path
print('Source modes and timestamps normalized from Git metadata')
