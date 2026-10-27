# Source mirror

GitHub `lictl/forgejo-operator` is primary. The existing public
`git.lucas.icu/lictl/forgejo-operator` repository already contains the same
43-commit history at the pre-publication audit HEAD. Forgejo does not convert
an existing repository into a pull mirror.

After verifying both remotes and authenticating **as lictl on each host**,
compare `main` ancestry. Push only `main` and reviewed release tags to the
existing Forgejo repository; refuse divergence and never use `--force` or
`git push --mirror`. GitHub Actions and Forgejo Actions are not interchangeable;
Forgejo's workflow is manual-only. This Git operation does not copy OCI images,
Charts, issues, releases or package visibility. The release workflow publishes
the OCI artifacts separately. Automatic source sync requires a narrow
Forgejo repository write credential, stored only after owner approval.
