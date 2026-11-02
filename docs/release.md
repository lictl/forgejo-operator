# Release runbook

Releases are operator versions, independent of Forgejo server versions. The
first release candidate is `v0.1.0-rc.1`; its tag and packages must be created
only after the reviewed version commit passes CI.

1. Confirm licenses, all CI checks, a clean tree and the exact commit. Run
   `python3 hack/release-notes.py <planned-tag>` to draft notes from
   [Conventional Commits](commits.md). Review them into `CHANGELOG.md` and set
   Chart `version` (without `v`), `appVersion` (with `v`) and default image tag.
   Both release workflows read this reviewed changelog section.
2. Create an annotated `vX.Y.Z` tag on the tested `main` commit. Push the same
   tag object to GitHub and Forgejo. A version tag push automatically starts
   `.github/workflows/release.yml` and `.forgejo/workflows/release.yml` on the
   respective sites. Verify both tags resolve to the same commit. Do not move
   published tags. A new manual dispatch from `main` with the existing tag
   remains available to retry a failed job.
3. Each site independently builds
   linux/amd64 and linux/arm64 controller images and packages the Chart from
   mirrored source. Each workflow writes only its own OCI registry. Ordinary
   source pushes and pull requests do not publish packages.
   The workflows execute the release driver and build context from the tag,
   but preserve the CI policy from their own source revision first. Before
   invoking the tagged driver, each workflow runs that preserved
   `hack/check-release-ci.py` gate. A failed gate stops the job;
   it does not wait for other workflows or publish partially verified artifacts.
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

On failure, start a new manual dispatch of the relevant site's workflow from
`main`, passing the same existing tag. Do not rerun a historical workflow that
predates the gate: an existing run retains its old workflow definition. The
script checks existing Chart bytes and image revision; ordinary releases never
overwrite a differing published version. A narrowly guarded exception allows
the first, unused `v0.1.0-rc.1` candidate to replace only the recorded old
image and Chart digests. If a registry rejects that replacement, stop rather
than deleting packages. Source mirroring is separate from package publication.
GitHub uses its repository workflow token for GHCR and read-only GitHub CI
queries (`actions: read`, alongside `contents: read`). Forgejo's release
workflow expects the existing `REGISTRY_TOKEN` secret with package-write access
on that instance; verify it before the first release. Creating or changing a
persistent credential or runner privilege needs owner review. Neither CI site
needs a credential for the other registry.

## Exact-commit CI gate

GitHub is the canonical CI source for both registries. Each workflow first
checks out its own source revision (`github.workflow_sha` on GitHub;
the triggering `github.sha` on Forgejo), copies the gate to the runner's
temporary directory, and then checks out the selected release tag. Manual
dispatches are accepted only from `main`. Thus retrying an older tag that has
no gate, or an obsolete gate, still uses the workflow's trusted policy before
its release driver can run. Policy code is kept outside the tagged build context.
The tagged driver also checks CI when its version includes the gate; workflow
authorization does not depend on that driver check.

The workflows resolve the local tag (including annotated tags) to the checked-out
commit. The gate verifies
that GitHub's tag resolves to that same commit and that it belongs to GitHub's
current `main` history. A stale or divergent mirror cannot authorize publication.
Main is the reviewed release source; this check does not replace PR review or
branch protection.

For that exact commit, the gate selects the newest main-branch run of each
required workflow by run number, including a newer scheduled vulnerability
scan. PR, feature-branch and tag-push results do not substitute for main CI.
Every selected run must be completed successfully, and its latest attempt must
contain these successful jobs:

| Workflow | Required jobs |
|----------|---------------|
| Tests (`test.yml`) | Run on Ubuntu; Prometheus rules |
| Lint (`lint.yml`) | Run on Ubuntu |
| Chart (`chart.yml`) | chart, including the gate's offline regression tests |
| E2E Tests (`test-e2e.yml`) | Kubernetes 1.30; Kubernetes 1.37 |
| Vulnerability check (`govulncheck.yml`) | govulncheck |

Both E2E matrix jobs are required before any release; the previous single E2E
job is insufficient. Keep workflow paths and job names in the gate synchronized
with CI when renaming or extending it. Every additional returned job must also
pass. Missing, skipped, neutral, cancelled, failed or unfinished jobs block the
release even when a workflow's overall conclusion is success. The gate reads
jobs from the latest attempt only: after a partial rerun, use **Re-run all jobs**
if the latest attempt does not contain the complete required job set.

The gate paginates run and job lists and rechecks run identities and attempts
after reading jobs. Truncated, malformed, unavailable or rate-limited API results
fail closed. Retry the release workflow with the same tag after CI completes or
the API recovers. Forgejo reads these public results anonymously; no persistent
GitHub credential is required or borrowed from its registry credential. The gate
is a check immediately before the release driver proceeds, not a lock preventing
someone from starting a later CI rerun. Do not rerun CI while releasing.

For a read-only preflight, invoke the gate from a current `main` checkout with
the full release commit SHA (especially when the selected tag predates the gate):

```bash
python3 hack/check-release-ci.py vX.Y.Z <full-release-commit-sha>
python3 hack/test-check-release-ci.py  # offline mocked API and driver tests
```

The API contracts are GitHub's [workflow runs](https://docs.github.com/en/rest/actions/workflow-runs),
[workflow jobs](https://docs.github.com/en/rest/actions/workflow-jobs) and
[commit comparison](https://docs.github.com/en/rest/commits/commits#compare-two-commits).
Policy checkout uses the documented [GitHub workflow context](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#github-context)
and [Forgejo source context](https://forgejo.org/docs/latest/user/actions/reference/#forgejo).

## Reproducibility

Reproducibility controls include pinned Go and base image indexes, a pinned
BuildKit image and Buildx version, commit-time `SOURCE_DATE_EPOCH`, normalized image file times
and `hack/normalize-chart.go`. `hack/normalize-source-times.py` first sets
checkout file modes from the Git index, directory modes to 0755, and all file
and directory times to the commit time. Both differ across CI checkouts by
default. The Chart helper also normalizes the `.tgz` file time
that Helm writes into the OCI manifest. Run independent builds to establish
whether the two CI environments and registries really produce the same payload.
A same-machine comparison is only a local check.
