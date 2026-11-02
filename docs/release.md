# Release runbook

Releases are operator versions, independent of Forgejo server versions. A
release's tag and packages are created only after the reviewed version commit
has passed CI on `main`.

1. Prepare the release in one pull request. Run
   `python3 hack/release-notes.py <planned-tag>` to draft notes from
   [Conventional Commits](commits.md) and write the reviewed section into
   `CHANGELOG.md` under the heading `## vX.Y.Z` (exactly that: the release
   scripts look the section up by it, and it becomes the text of the
   prereleases), including what an upgrade needs. Set Chart `version`
   (without `v`) and `appVersion` (with `v`). The Chart's default controller
   image tag is its `appVersion`, so `values.yaml` sets no tag; Chart CI and
   `hack/release.sh` check that the rendered image is the release's. Search the
   documents for text that names the previous release. Merge the pull request
   with a merge or squash commit.
2. Create an annotated `vX.Y.Z` tag on the commit that merge created on `main`.
   Only a commit that `main` itself was pushed at has the CI runs the gate
   requires (a commit of the pull request branch, however green, has none):
   wait until `gh run list --commit <sha> --branch main` shows every workflow
   of the table below completed successfully, the E2E jobs take the longest.
   Fast-forward `main` of the Forgejo repository to that commit first
   ([mirroring](mirroring.md)); the Forgejo job checks the commit against its
   own `main`. Push the tag to GitHub first and then the same tag object to
   Forgejo: the Forgejo job reads GitHub's copy of the tag. A version tag push
   automatically starts `.github/workflows/release.yml` and
   `.forgejo/workflows/release.yml` on the respective sites. Verify both tags
   resolve to the same commit. Do not move published tags. A new manual
   dispatch from `main` with the existing tag remains available to retry a
   failed job.
3. Each site independently builds
   linux/amd64 and linux/arm64 controller images and packages the Chart from
   mirrored source. Each workflow writes only its own OCI registry. Ordinary
   source pushes and pull requests do not publish packages.
   The workflows execute the release driver and build context from the tag,
   but run the CI gate of their own source revision first: on a manual dispatch
   that is `main`, on a tag push it is the tagged commit itself (so a release
   tag must contain the gate, which every commit since it was introduced
   does). Before invoking the tagged driver, each workflow runs that
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
script checks existing Chart bytes and image revision; releases never overwrite a
differing published version, and `v0.1.0-rc.1` cannot be published again: it
predates the gate. Source mirroring is separate from package publication.
GitHub uses its repository workflow token for GHCR and read-only GitHub CI
queries (`actions: read`, alongside `contents: read`). Forgejo's release
workflow expects the existing `REGISTRY_TOKEN` secret with package-write access
on that instance; verify it before a release. Creating or changing a
persistent credential or runner privilege needs owner review. Neither CI site
needs a credential for the other registry.

Forgejo reads GitHub's results anonymously, which GitHub limits to 60 requests per
hour and IP address. One run of the gate takes about 20 requests and a Forgejo job
runs it twice (workflow and driver), so after a failed Forgejo run wait up to an hour
before the next dispatch (the gate reports the HTTP 403 as an API failure).

## Exact-commit CI gate

GitHub is the canonical CI source for both registries. Each workflow first
checks out its own source revision (`github.workflow_sha` on GitHub;
the triggering `github.sha` on Forgejo), copies the gate to the runner's
temporary directory, and then checks out the selected release tag. Manual
dispatches are accepted only from `main`. Thus retrying an older tag that has
no gate, or an obsolete gate, from such a dispatch still uses the workflow's trusted
policy before its release driver can run. A tag push starts the workflow file of the tagged
commit, so there the policy is that commit's own copy: the tag decides, not `main`.
Policy code is kept outside the tagged build context.
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

For a read-only check once the tag exists on GitHub (the gate reads GitHub's copy of it),
invoke the gate from a current `main` checkout with the full release commit SHA
(especially when the selected tag predates the gate):

```bash
python3 hack/check-release-ci.py vX.Y.Z <full-release-commit-sha>
python3 hack/test-check-release-ci.py  # offline mocked API and driver tests
```

To try the Forgejo workflow before a release without publishing anything, dispatch it from
`main` with an existing tag that predates the gate (`v0.1.0-rc.1`): it must stop at the gate
with "Missing canonical main CI". That exercises the runner, the policy checkout and the
anonymous GitHub reads, not the registry credential. Do it only after `main` of the Forgejo
repository has been fast-forwarded to a commit that contains the gated workflow: the
workflow of an older `main` has no gate and would run the old release driver.

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
