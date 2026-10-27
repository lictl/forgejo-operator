#!/usr/bin/env python3
"""Set checkout file and directory mtimes to the release commit time."""
import os
from pathlib import Path
import sys

root = Path(sys.argv[1]).resolve()
epoch = int(sys.argv[2])
directories = []
for directory, children, files in os.walk(root):
    children[:] = [name for name in children if name != '.git']
    directories.append(Path(directory))
    for name in files:
        path = Path(directory) / name
        if not path.is_symlink():
            os.utime(path, (epoch, epoch))
for directory in reversed(directories):
    os.utime(directory, (epoch, epoch))
