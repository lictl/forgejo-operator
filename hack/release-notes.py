#!/usr/bin/env python3
"""Draft a changelog section from Conventional Commits for maintainer review."""
import argparse
import re
import subprocess


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('tag', help='planned vX.Y.Z tag (also accepts a prerelease)')
parser.add_argument('--base', help='previous release tag or starting commit')
args = parser.parse_args()
if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?', args.tag):
    parser.error('tag must look like vX.Y.Z or vX.Y.Z-rc.1')

base = args.base
if base is None:
    tags = git('tag', '--merged', 'HEAD', '--list', 'v*', '--sort=-version:refname').splitlines()
    base = tags[0] if tags else None
if base:
    base = git('rev-parse', '--verify', '--end-of-options', f'{base}^{{commit}}')
revision = f'{base}..HEAD' if base else 'HEAD'
raw = git('log', '--no-merges', '--format=%s%x1f%b%x1e', revision)

sections = {
    'Breaking changes': [], 'Features': [], 'Fixes': [],
    'Performance': [], 'Maintenance': [], 'Other changes': [],
}
subject_pattern = re.compile(r'^(feat|fix|docs|chore|refactor|perf|test|ci|build|style|revert)(?:\(([^)]+)\))?(!)?: (.+)$')
for record in raw.split('\x1e'):
    if not record.strip():
        continue
    subject, _, body = record.strip().partition('\x1f')
    match = subject_pattern.fullmatch(subject)
    breaking_footer = re.search(r'(?m)^BREAKING(?:-| )CHANGE: (.+)$', body)
    if match:
        kind, scope, bang, summary = match.groups()
        item = f'{summary} (`{scope}`)' if scope else summary
        if bang or breaking_footer:
            if breaking_footer:
                item += f' — {breaking_footer.group(1)}'
            sections['Breaking changes'].append(item)
        elif kind == 'feat':
            sections['Features'].append(item)
        elif kind == 'fix':
            sections['Fixes'].append(item)
        elif kind == 'perf':
            sections['Performance'].append(item)
        else:
            sections['Maintenance'].append(f'{kind}: {item}')
    else:
        sections['Other changes'].append(subject)

print(f'## {args.tag}\n')
for title, items in sections.items():
    if items:
        print(f'### {title}\n')
        for item in items:
            print(f'- {item}')
        print()
