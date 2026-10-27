# Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
for new commits. Existing history is not rewritten. A subject has the form
`type(optional-scope): concise imperative summary`. Keep the first line
short, describe the user-visible change rather than the work session, and
put details and references in the body. A scope names the affected area, for
example `api`, `backup`, `chart`, `docs` or `ci`.

| Type | Use for |
|------|---------|
| `feat` | New user-visible behavior or API capability. |
| `fix` | A user-visible bug fix. |
| `docs` | Documentation only. |
| `chore` | Repository upkeep that changes no product behavior. |
| `refactor` | Internal restructuring with unchanged behavior. |
| `perf` | A performance improvement. |
| `test` | Tests only. |
| `ci` | CI workflow changes. |
| `build` | Build and packaging changes. |
| `style` | Formatting without behavior changes. |
| `revert` | Reversion of an earlier commit; name the reverted commit in the body. |

Examples:

```text
feat(backup): add remote restore status
fix(chart): bind the manager service account
docs: explain CRD upgrades
ci: check both Linux architectures
feat(api)!: remove the deprecated storage field

BREAKING CHANGE: migrate existing Forgejo resources to spec.objectStorage.
```

Use `!` immediately before the colon for an incompatible change and explain
the migration in a `BREAKING CHANGE:` footer. A footer without `!` also marks
a breaking change. Keep the footer in the same commit message, separated from
the body by a blank line. Include an issue or PR reference where useful.

For release planning, `feat` normally calls for a SemVer minor version;
`fix` and `perf` normally call for a patch; any breaking change calls for a
major version. `docs`, `chore`, `refactor`, `test`, `ci`, `build`, `style` and
`revert` do not by themselves mandate a version bump. A revert's actual
effect must still be reviewed. Pre-release versions and the first release
are chosen deliberately by the maintainer.

`python3 hack/release-notes.py <planned-tag>` groups new commits into a
changelog draft since the most recent reachable `v*` tag. Use `--base <ref>`
to choose a different starting commit. Nonconforming and older commits go
under Other changes for manual review. Check the draft against actual behavior,
edit it into `CHANGELOG.md`, and verify the SemVer choice. The manual release
workflow publishes the reviewed `CHANGELOG.md` section; it does not read
commit prefixes or GitHub PR labels directly. No commit-message enforcement,
automatic version bump or automatic release trigger is configured.
