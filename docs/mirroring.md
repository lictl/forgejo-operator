# Source mirror

GitHub `lictl/forgejo-operator` is primary. The public
`git.lucas.icu/lictl/forgejo-operator` repository holds the same history and is
brought up to date by hand (below). Forgejo does not convert an existing
repository into a pull mirror. Its `main` must be fast-forwarded to the release
commit before a release is tagged: the Forgejo release job checks the commit
against its own `main`, and until then it refuses.

After verifying both remotes and authenticating **as lictl on each host**,
compare `main` ancestry. Push only `main` and reviewed release tags to the
existing Forgejo repository; refuse divergence and never use `--force` or
`git push --mirror`. GitHub Actions and Forgejo Actions are not interchangeable;
Forgejo's CI workflow is manual-only. This Git operation does not copy OCI images,
Charts, issues, releases or package visibility. Each site's release workflow
builds and publishes its own OCI artifacts only after checking the exact tag
commit against canonical GitHub main CI; Forgejo's manual diagnostics do not
authorize a release. See the [release gate](release.md#exact-commit-ci-gate).
Automatic source sync requires a narrow
Forgejo repository write credential, stored only after owner approval.
