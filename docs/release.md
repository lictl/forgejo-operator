# Release runbook

Releases are operator versions, independent of the Forgejo server version table.
No initial version has been selected or published. A suggested first candidate is
`v0.1.0-rc.1`; choosing and creating it is a separate maintainer action.

1. Confirm MIT and retained third-party notices, all CI checks, a clean tree,
   and the exact commit to release. Run `python3 hack/release-notes.py <planned-tag>`
   to draft notes from [Conventional Commits](commits.md); review the actual
   changes and edit the draft into `CHANGELOG.md`. The release workflow reads
   that changelog section, not commit prefixes or PR labels. Update Chart `version`
   (without `v`) and `appVersion` (with `v`), plus default image tag. Merge
   these changes to `main`.
2. Create an annotated `vX.Y.Z` tag at that commit and push it to GitHub.
   Start `.github/workflows/release.yml` manually with that tag. It rejects
   a missing tag, a different Chart version, and a commit outside `main`.
3. The workflow builds an amd64/arm64 controller once in GHCR, copies its OCI
   index to Forgejo, packages the Chart once and pushes it to both registries.
   It then creates release notes from the changelog. Do not treat a partially
   successful run as a completed release.
4. Inspect both image indexes for `linux/amd64` and `linux/arm64`, digest
   equality, Chart metadata and anonymous pull. Set both GHCR packages
   public if first publication left them private. Test Helm install in an
   isolated Kind cluster. Check the GitHub Release and mirrored source tag.

If a registry operation fails, rerun with the same tag. The workflow checks
existing tags/packages and refuses mismatched content; do not force a tag or
replace a published version. Git source mirroring and OCI mirroring are
separate operations. A release requires the Forgejo registry token in the
GitHub repository secret `FORGEJO_REGISTRY_TOKEN`; a distinct
`FORGEJO_MIRROR_TOKEN` would be needed for automatic source mirroring. Neither
credential is in the repository. Their creation and permissions require owner
review. Keep local credentials out of CI.
