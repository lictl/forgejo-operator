#!/usr/bin/env python3
"""Set checkout mtimes and modes to the tagged Git source metadata."""
import os
from pathlib import Path
import subprocess
import sys

root = Path(sys.argv[1]).resolve()
epoch = int(sys.argv[2])
index = subprocess.check_output(['git', '-C', str(root), 'ls-files', '--stage', '-z'])
file_modes = {}
for entry in index.split(b'\0'):
    if entry:
        metadata, _, name = entry.partition(b'\t')
        mode = metadata.split(b' ', 1)[0]
        file_modes[os.fsdecode(name)] = int(mode, 8) & 0o777
directories = []
for directory, children, files in os.walk(root):
    children[:] = [name for name in children if name != '.git']
    directories.append(Path(directory))
    for name in files:
        path = Path(directory) / name
        if not path.is_symlink():
            relative = path.relative_to(root).as_posix()
            os.chmod(path, file_modes.get(relative, 0o644))
            os.utime(path, (epoch, epoch))
for directory in reversed(directories):
    os.chmod(directory, 0o755)
    os.utime(directory, (epoch, epoch))
