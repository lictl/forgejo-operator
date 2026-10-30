#!/usr/bin/env python3
"""Snapshot the notices for modules linked into the manager binary."""
import json
import os
from pathlib import Path
import shutil
import subprocess

repo = Path(__file__).resolve().parents[1]
# Match the project toolchain, including its standard-library license.
go_env = {**os.environ, "GOTOOLCHAIN": "go1.26.10"}
output = repo / 'LICENSES' / 'dependencies'
decoder = json.JSONDecoder()
modules = {}
for arch in ('amd64', 'arm64'):
    platform_env = {**go_env, 'GOOS': 'linux', 'GOARCH': arch, 'CGO_ENABLED': '0'}
    raw = subprocess.check_output(['go', 'list', '-deps', '-json', './cmd'], cwd=repo, text=True, env=platform_env)
    i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace():
            i += 1
        if i == len(raw):
            break
        package, i = decoder.raw_decode(raw, i)
        module = package.get('Module')
        if module and not module.get('Main'):
            modules[module['Path']] = module

if output.exists():
    shutil.rmtree(output)
output.mkdir(parents=True)
rows = []
for name, module in sorted(modules.items()):
    location = Path(module['Dir'])
    notices = sorted(p for p in location.iterdir() if p.is_file() and p.name.upper().startswith(('LICENSE', 'COPYING', 'NOTICE')))
    if not notices:
        raise SystemExit(f'Missing license notice for {name}')
    destination = output / name
    destination.mkdir(parents=True)
    for notice in notices:
        shutil.copyfile(notice, destination / notice.name)
    links = ', '.join(f'[{p.name}](dependencies/{name}/{p.name})' for p in notices)
    rows.append(f'- `{name}` `{module.get("Version", "unknown")}`: {links}')

goroot = Path(subprocess.check_output(['go', 'env', 'GOROOT'], text=True, env=go_env).strip())
shutil.copyfile(goroot / 'LICENSE', repo / 'LICENSES' / 'Go-stdlib-LICENSE.txt')
(repo / 'LICENSES' / 'README.md').write_text('# Go runtime dependency licenses\n\nThese notices correspond to the modules linked into `./cmd` by `go list -deps`.\nRegenerate with `python3 hack/update-third-party-licenses.py` after dependency changes.\n\n- [Go standard library](Go-stdlib-LICENSE.txt)\n' + '\n'.join(rows) + '\n')
print(f'Copied notices for {len(modules)} Go modules')
