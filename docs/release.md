# Release runbook

Releases are operator versions, independent of Forgejo server versions. The
first release candidate is `v0.1.0-rc.1`; its tag and packages must be created
only after the reviewed version commit passes CI.

1. Confirm licenses, all CI checks, a clean tree and the exact commit. Run
   `python3 hack/release-notes.py <planned-tag>` to draft notes from
   [Conventional Commits](commits.md). Review them into `CHANGELOG.md` and set
   Chart `version` (without `v`), `appVersion` (with `v`) and default image tag.
   Both release workflows read this reviewed changelog section.
2. Create an annotated `vX.Y.Z` tag on `main`. Push it to GitHub and mirror the
   exact tag object to Forgejo. Verify both sites resolve it to the same commit.
   Never force-move a published tag.
3. Manually start `.github/workflows/release.yml` and
   `.forgejo/workflows/release.yml` with that tag. Each site independently builds
   linux/amd64 and linux/arm64 controller images and packages the Chart from
   mirrored source. Each workflow writes only its own OCI registry. Ordinary
   source pushes and pull requests do not publish packages.
   The workflows execute the current reviewed release driver from `main` but
   check out the immutable tag for source, Chart contents, and image context.
   Record the workflow's `main` SHA alongside the tagged source SHA.
4. Make both image and Chart packages anonymously readable. GHCR may create
   packages private initially. With `crane` and Helm available, run
   `TAG=vX.Y.Z bash hack/verify-release.sh`. It compares each platform image
   manifest digest, Chart archive bytes and Chart OCI manifest digest. The outer
   image index digests can differ because independent provenance records are
   attached; record both. Investigate any payload mismatch. Test Helm install
   in an isolated Kind cluster.
5. After both sites pass, create a prerelease on GitHub and Forgejo from the
   reviewed changelog section. Include both registry references, their digests,
   the tagged source commit, and the cross-registry verification result.

On failure, rerun the relevant site's manual workflow with the same tag. The
script checks existing Chart bytes and image revision; do not overwrite a
published version. Source mirroring is separate from package publication.
GitHub uses its repository workflow token for GHCR only. Forgejo's release
workflow expects the existing `REGISTRY_TOKEN` secret with package-write access
on that instance; verify it before the first release. Creating or changing a
persistent credential or runner privilege needs owner review. Neither CI site
needs a credential for the other registry.

Reproducibility controls include pinned Go and base image indexes, a pinned
BuildKit image and Buildx version, commit-time `SOURCE_DATE_EPOCH`, normalized image file times
and `hack/normalize-chart.go`. `hack/normalize-source-times.py` first sets
checkout file modes from the Git index, directory modes to 0755, and all file
and directory times to the commit time. Both differ across CI checkouts by
default. The Chart helper also normalizes the `.tgz` file time
that Helm writes into the OCI manifest. Run independent builds to establish
whether the two CI environments and registries really produce the same payload.
A same-machine comparison is only a local check.
