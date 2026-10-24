# Forgejo versions

Which Forgejo releases the operator supports, how it reports the version an instance runs, how
to move between majors, and what to edit when Forgejo publishes a release. Everything here is
driven by one table, `forgejo.Versions` in
[`internal/forgejo/versions.go`](../internal/forgejo/versions.go).

## Support policy

The operator supports **the current LTS release and the latest release** of Forgejo:

- The current LTS is the newest LTS major in the table that has been released and has not
  reached its upstream end of life.
- The latest release is the newest non-LTS major in the table that has been released and has
  not reached its end of life.

Both are computed from the table and today's date, so the policy moves by itself: when a newer
release is in the table and its release date arrives, the release it replaces becomes
`Deprecated` until its upstream end of life, and `EndOfLife` after that. Only releases that have
been verified with the operator are in the table (`Releases`); releases known from the upstream
schedule but not verified yet are listed under `Upcoming` and reported as `Future`.

Unsupported versions are **not blocked**: they keep running, they can be upgraded to, and they
are reported through a condition, events, a metric and an alert. The operator refuses only an
upgrade that skips more majors than the table allows (`UpgradePathUnsupported`, see
[Upgrading between majors](#upgrading-between-majors)) and a downgrade (`DowngradeRefused`),
whatever the support of the versions involved.

New instances get the current LTS release: `spec.image` defaults to
`codeberg.org/forgejo/forgejo:15`. The API server stores the default when the object is
created, so changing it in a later operator release never changes the image of an existing
instance.

## Support matrix

As of 2026-10-22 (dates from the upstream
[release schedule](https://forgejo.org/docs/latest/admin/release-schedule/)):

| Major | LTS | Released | Upstream end of life | Operator | Notes |
|-------|-----|----------|----------------------|----------|-------|
| 11 to 14 | | | passed | `EndOfLife` | Upgrade to 15 first, then to 17 if you want it (`UpgradePathUnsupported` otherwise) |
| 10 and older | | | passed | `EndOfLife` | Upgrade to 11 first, then to 15: 15 is upgraded to from 11 or newer (`UpgradePathUnsupported` otherwise) |
| **15** | yes | 2026-04-16 | 2027-07-15 | `Supported`, the default image | Use 15.0.10 or newer: 15.0.8 and 15.0.9 fix critical security issues |
| 16 | no | 2026-07-16 | 2026-10-29 | `Deprecated`, `EndOfLife` from 2026-10-29 | Upgrade to 17 |
| **17** | no | 2026-10-15 | 2027-01-28 | `Supported` | Upstream advises against 17.0.0 (pushes and merges can fail, forgejo#14866): use 17.0.1 or newer once it is out |
| 18 | no | due 2027-01-14 | 2027-04-29 | `Future` (upcoming, its traits are prepared) | Replaces INTERNAL_TOKEN with a UNIX socket |
| 19 | yes | due 2027-04-15 | 2028-07-13 | `Future` (upcoming) | Next LTS |

What changes on which date:

| Date | Without an operator update | Once the new release is verified and in the table |
|------|----------------------------|---------------------------------------------------|
| 2026-10-29 | 16 becomes `EndOfLife` | – |
| 2027-01-14 | 18 is `Future` | Supported: 15 and 18; 17 is `Deprecated` until 2027-01-28 |
| 2027-01-28 | 17 becomes `EndOfLife`; only 15 is supported | – |
| 2027-04-15 | 19 is `Future` | Supported: 19 and 18; 15 is `Deprecated` until 2027-07-15; the default image becomes 19 |

Both image flavors are supported: `codeberg.org/forgejo/forgejo:<tag>` and `<tag>-rootless`.
Upstream publishes the tags `N`, `N.0` and `N.0.P` (there is no `latest`). With a floating tag
(`17`) and the default `imagePullPolicy: IfNotPresent`, a node keeps the patch release it pulled
first; pin `N.0.P` (or use `imagePullPolicy: Always`) to control patch updates. The operator
reads the exact patch release from Forgejo's `/api/v1/version` (`status.version`). It probes
through the Service, so when the nodes of a multi-replica instance hold different patch
releases, `status.version` alternates between them, and with it an advisory in the
`VersionSupported` message; the `VersionDetected` and `VersionAdvisory` events are not repeated
for that (see [Events](#events)). Pinning `N.0.P` or `imagePullPolicy: Always` avoids it. A
floating tag can also pull a patch release that an advisory names (17.0.0): the condition and
the `ForgejoVersionAdvisory` alert report it, and the support matrix above names the patch to
pin.

## Status

### The `VersionSupported` condition

The operator classifies the version the instance **runs**: `status.image`, or the image being
deployed before the first rollout has completed. The major comes from the image tag; the exact
version from `status.version` when the tag floats (`17`) or has no version (a digest).

| Reason | Status | Meaning | What to do |
|--------|--------|---------|------------|
| `Supported` | True | The current LTS or the latest release. The message names the upstream end of life, and an advisory when the running patch release is older than the recommended one. | Nothing, or update the patch release the advisory names |
| `Deprecated` | False | Still maintained upstream, but outside the policy | Upgrade before the end-of-life date in the message |
| `EndOfLife` | False | No more security fixes upstream | Upgrade now |
| `Future` | False | Newer than the operator's table, or not released yet | Update the operator to a release that supports it |
| `Unknown` | Unknown | The image tag carries no version and Forgejo has not reported one yet | Wait for the first health probe, or use a version tag |

Examples:

```console
$ kubectl get fj -o wide
NAME   PHASE     VERSION               READY   REPLICAS   SUPPORT      URL   AGE
a      Running   15.0.10+gitea-1.22.0  True    1          Supported    ...   3d
b      Running   16.0.6+gitea-1.22.0   True    1          Deprecated   ...   9d

$ kubectl get fj b -o jsonpath='{.status.conditions[?(@.type=="VersionSupported")].message}'
Forgejo 16 is outside this operator's support policy (supported: 15 (LTS) and 17) and reaches
its upstream end of life on 2026-10-29. Upgrade to 17 (17.0.1 or newer) before 2026-10-29.
```

### Events

| Event | Type | When |
|-------|------|------|
| `UnsupportedVersion` | Warning | The classification changes to `Deprecated`, `EndOfLife` or `Future`; also when an upgrade starts towards such a version ("Upgrading to ... anyway") |
| `VersionAdvisory` | Warning | A supported major runs a patch release with an advisory ("The running patch release 17.0.0 is older than recommended: use 17.0.1 or newer (...)"). Emitted once per `status.image` and patch release, and repeated at most daily, so that a `status.version` that alternates between nodes does not repeat it |
| `SupportedVersion` | Normal | The instance is supported again after `Deprecated`, `EndOfLife` or `Future` |
| `ConfigIncompatible` | Warning | `spec.config` or `configFrom` has values the target major refuses to start with (see [16 → 17](#16--17)). When the upgrade starts, only `spec.config` is checked. Values from `configFrom` are checked once `app.ini` is rendered for the new image, that is during the rollout; this check repeats at most hourly while the values stay |
| `UpgradePathUnsupported` | Warning | An upgrade skips more majors than the table allows (see [Upgrading between majors](#upgrading-between-majors)) |

Apart from the upgrade start, `UnsupportedVersion`, `VersionAdvisory` and `SupportedVersion`
are emitted when the reason or the message of the condition changes. The message names the major only (the patch release
appears only in an advisory), so it does not change when Forgejo reports its exact version.

### Metrics and alert

| Metric | Labels | Meaning |
|--------|--------|---------|
| `forgejo_operator_version_supported` | `major`, `support` | 1 when `Supported`, else 0; `support` is the reason. One series per instance |
| `forgejo_operator_version_end_of_life_timestamp_seconds` | `major` | Upstream end of life of the running major (when it is in the table) |
| `forgejo_operator_version_advisory` | `major` | 1 when the running patch release is older than the one an advisory of the table recommends (`Advisory` in the condition), else 0; absent while the version is unknown |

The per-instance PrometheusRule has **`ForgejoVersionUnsupported`** (warning, for 1h):
`max by (namespace, name, major, support) (forgejo_operator_version_supported) == 0`, and
**`ForgejoVersionAdvisory`** (warning, for 1h):
`max by (namespace, name, major) (forgejo_operator_version_advisory) == 1`. A supported major
that misses critical security fixes (15.0.7) or runs a patch upstream advises against (17.0.0)
alerts through the latter.

## Upgrading between majors

The version gate of [upgrades.md](upgrades.md) applies, with two rules from the table:

- **Skipping majors** is allowed when the target's `UpgradeFrom` is at most the running
  major. Forgejo's migrations are cumulative and its upgrade guide says to upgrade straight to
  the latest release; for 15 (LTS) → 17 that is verified (15.0.10 → 17.0.0 migrated, then
  served pushes and merges). Today 16, 17 and the upcoming 18 and 19 accept upgrades from 15 or
  newer, and 15 from 11 or newer. An instance on 14 that asks for 17 is refused with
  `UpgradePathUnsupported` (Degraded, one Warning event, the current image keeps running) and
  must go to 15 first; one on 10 or older goes to 11 first, then to 15. Majors older than the
  table are not restricted among themselves (10 → 11 is allowed).
- **Majors newer than the table** (`Future`) are allowed, with an `UnsupportedVersion` warning.
  An upcoming major is held to its row's `UpgradeFrom`; a major newer than every row to the
  newest row's, so that 14 → 20 is refused like 14 → 17. They run with the traits of the
  newest verified release plus the prepared `app.ini` traits of the upcoming rows up to their
  major (see [Per-major differences](#per-major-differences)).

Both rules, and the downgrade check, also apply to an instance whose image carries no version
(a digest or a custom tag): its version is then the one Forgejo reports (`status.version`).

Downgrades stay refused (`DowngradeRefused`): a major never starts on a database that a newer
one migrated. Going back always means restoring the pre-upgrade backup (see
[Rolling back manually](upgrades.md#rolling-back-manually)).

### 16 → 17

1. **Durations are strict on 17** (forgejo#10273). A setting that is set but does not parse as
   a duration stops Forgejo at start, including `forgejo migrate` in the `bootstrap` init
   container, so the upgrade fails and is rolled back. Values such as `-1` or `10` need a unit:
   `-1s`, `10s`. The operator warns about these keys (`ConfigIncompatible`):
   `[server]` `PER_WRITE_TIMEOUT`, `PER_WRITE_PER_KB_TIMEOUT`, `SSH_PER_WRITE_TIMEOUT`,
   `SSH_PER_WRITE_PER_KB_TIMEOUT`, `STARTUP_TIMEOUT`, `GRACEFUL_HAMMER_TIME`,
   `STATIC_CACHE_TIME`, `SUBPROCESS_TERMINATE_GRACE`, `PROXY_PROTOCOL_HEADER_TIMEOUT`,
   `LFS_HTTP_AUTH_EXPIRY`; `[database]` `CONN_MAX_LIFETIME`, `CONN_MAX_IDLETIME`,
   `DB_RETRY_BACKOFF`, `SLOW_QUERY_THRESHOLD`; `[indexer] STARTUP_TIMEOUT`;
   `[mailer] SENDMAIL_TIMEOUT`; `[migrations] AVATAR_FETCH_TIMEOUT`;
   `[moderation] KEEP_RESOLVED_REPORTS_FOR`; `[service] USER_DELETE_WITH_COMMENTS_MAX_TIME`;
   `[actions]` `ZOMBIE_TASK_TIMEOUT`, `ENDLESS_TASK_TIMEOUT`, `ABANDONED_JOB_TIMEOUT`;
   `[authorized_integration]` `REQUEST_TIMEOUT`, `CACHE_TTL`. The operator's own values are
   valid. When the upgrade starts it checks `spec.config` only; values from `configFrom` are
   checked once `app.ini` is rendered for 17, when the rollout has already begun, so check the
   `configFrom` Secrets yourself before changing the image.
2. **Unencrypted mirrors are refused on 17** (forgejo#13490): migrations and mirrors over
   `http://` and `git://` need `[migrations] ALLOW_UNENCRYPTED = true`; in-cluster sources also
   need `ALLOW_LOCALNETWORKS = true`.
3. Prefer 17.0.1 or newer (the 17.0.0 advisory).
4. Change the image; the operator takes the pre-upgrade backup, migrates with one pod, verifies
   and rolls back on failure:

   ```sh
   kubectl patch fj <name> --type merge -p '{"spec":{"image":"codeberg.org/forgejo/forgejo:17"}}'
   ```

5. Expect `status.upgrade.phase: Succeeded`, `status.version` 17.x and `VersionSupported=True`.

### 15 → 17 (skipping 16)

Everything of 16 → 17, plus the 16 changes:

- From 16 on Forgejo keeps its git hooks in one global `core.hooksPath`
  (`/data/gitea/home/hooks`, rewritten at every start) and no longer re-syncs every
  repository's hooks at startup.
- 16 enforces `[migrations] ALLOWED_DOMAINS`, `BLOCKED_DOMAINS` and `ALLOW_LOCALNETWORKS` for
  git and LFS mirroring too, and git no longer follows redirects for mirrors (mirrors of renamed
  remotes fail).
- 16 turns on `transfer.fsckobjects`: pushes and mirrors of repositories with malformed objects
  can be rejected.
- The upstream upgrade guide lists optional cleanups after 16 and 17 (sample hooks,
  `description` files, the `pulls` directories); the operator does not run them.

If the direct jump fails and is rolled back, go through the last 16 patch release
(`codeberg.org/forgejo/forgejo:16.0.6`), then 17, as the upstream guide recommends. 16 is
`EndOfLife` from 2026-10-29: use it only as a stepping stone.

### Recommended manual steps around a major upgrade

The upstream guide also recommends these, which the operator does not do yet:

- Flush the queues right before changing `spec.image` (queued work in LevelDB on the data volume,
  or in Valkey with high availability, is not guaranteed to be readable by the next major):

  ```sh
  kubectl exec deploy/<name> -c forgejo -- /app/gitea/gitea manager flush-queues --config /etc/forgejo/app.ini
  ```

- Check the instance afterwards:

  ```sh
  kubectl exec deploy/<name> -c forgejo -- /app/gitea/gitea doctor check --all --config /etc/forgejo/app.ini --log-file /tmp/doctor.log
  ```

## Per-major differences

Differences between majors are **traits** of a table row (`forgejo.Traits`). Code reads
`rc.Layout.Traits`, never a major number, so adding a row is enough to activate them.

| Trait | Majors | What the operator does |
|-------|--------|------------------------|
| `InternalToken` | 15, 16, 17 | Renders `[security] INTERNAL_TOKEN`. Without it Forgejo 17 and older generate one and try to write it into the read-only `app.ini`, and crash. The token is kept in `<name>-secrets` for every major, so that a rollback finds it |
| `InternalSocket` | 18 (prepared) and newer, images of unknown version | Renders `[server] INTERNAL_LISTENER_PATH = /tmp/forgejo-run/forgejo.sock`: git hooks, the SSH server and `forgejo manager` reach Forgejo 18 over this UNIX socket, and its default (`run/forgejo.sock` under `WORK_PATH`) is on the shared data volume, where the pods of a multi-replica instance would replace each other's socket. `/tmp` is a per-pod emptyDir; Forgejo creates `forgejo-run` itself |
| `StrictDurations` | 17 and newer, images of unknown version | `ConfigIncompatible` warnings (see [16 → 17](#16--17)) |
| `PasswordFromStdin` | 18 (prepared) | The `bootstrap` init container pipes the admin password into `admin user change-password` instead of passing `--password`, so it is not visible in the process list |

The prepared 18 traits were smoke-tested with `codeberg.org/forgejo-experimental/forgejo:18.0-test`
(18.0.0-dev-128) as uid 1000 with a read-only root file system: `migrate`, `admin user create`,
`change-password` from stdin, and an HTTP push whose hooks reached Forgejo over
`/tmp/forgejo-run/forgejo.sock`. The union `app.ini` (token and socket) also ran on 17.0.0
(the socket key is ignored) and on 18 (one error line about the deprecated token).

- **Majors newer than the verified releases** (`Future`: 18 before it is verified, or 21) get
  the newest verified release's traits plus the `app.ini` traits (`InternalToken`,
  `InternalSocket`, `StrictDurations`) of every upcoming row up to their major. `app.ini` then
  carries what both need, so a multi-replica instance on 18 gets its per-pod socket before the
  operator is updated. CLI traits (`PasswordFromStdin`) stay those of the newest verified
  release.
- **Images of unknown version** (digest only, custom tags) get the union of what the verified
  majors, the upcoming majors and the majors older than the table need in `app.ini` (each key
  is ignored, or only logged, by the others) and only CLI features that every verified major
  and the older majors have. For the `ConfigIncompatible` warning about durations, the version
  Forgejo reports (`status.version`) decides once the instance runs the image, so a
  digest-pinned 15 or 16 is not warned about; before that the warning is conditional ("If
  `<image>` is Forgejo 17 or newer, it refuses to start with ..."). An upgrade from such an
  image whose version is not known yet counts as one from a lenient major, so its start warns
  about the values the target refuses.
- **Majors older than the table** run with fixed legacy traits (`INTERNAL_TOKEN`), whatever
  the oldest row is.
- **The last known good `app.ini`** (config rollback, see [autopilot.md](autopilot.md)) records
  the profile it was rendered for (`forgejo.lucas.icu/config-profile`: `internal-token`,
  `internal-socket` or both). It is only mounted for an image with the same profile, so an
  `app.ini` rendered for 18 never reaches a 17 pod. An 18 image that runs as `Future` (both
  keys) changes profile when 18 is verified (socket only): its last known good `app.ini` no
  longer matches, and config rollback reports `ConfigRollbackUnavailable` until a new one is
  recorded.
- Verified to be the same on 15, 16 and 17 (and on the 18 development branch): the image
  layout (`/app/gitea/gitea`, uid 1000 `git`, root and rootless), the CLI the bootstrap uses
  (`migrate`, `admin user list/create/change-password`), `/api/healthz`, `/api/v1/version`,
  `/metrics`, every other `app.ini` key the operator renders, and the minimum PostgreSQL
  version (14).
- For 15–17 the rendered `app.ini` and pod template are byte-identical to operator versions
  before the version table, so updating the operator restarts no instance.
- Not done yet: the one-hour startup probe exists for Forgejo 15, which re-syncs every
  repository's hooks before it listens; 16 and newer would not need it. It is the same for all
  majors so that an operator update does not restart every instance.

## When a new Forgejo version is released

`make forgejo-versions` (`go run ./hack/forgejo-versions`) compares the table with upstream and
needs no credentials: the release schedule JSON, the Codeberg releases API and the container
registry (anonymous token). It prints the matrix and findings, and exits with 1 when the table
is stale (a `STALE` line), and 2 when upstream cannot be read or the arguments are invalid (it
then prints its usage). `WARN` and `INFO` lines need a look but do not fail it. Through `go run`
or make the status is not passed on: `go run` exits with 1 and make with 2 whenever the tool
fails, after printing the tool's `exit status N`. Read that line, or build the tool to get its
own status:

```sh
go build -o bin/forgejo-versions ./hack/forgejo-versions && bin/forgejo-versions; echo "exit=$?"
```

Run it on a schedule.

The procedure below is automated by the Claude Code workflow
[`.claude/workflows/forgejo-new-version.js`](../.claude/workflows/forgejo-new-version.js) (run the
saved workflow `forgejo-new-version` with `{kubeContext}`, the kube context of a local test
cluster, and optionally `{event, major, dockerContext}`, where `dockerContext` is the docker
context of the engine that cluster runs on). It runs the tool, researches the release, applies
the edits below, verifies them on that local cluster with
[`test/e2e/checklists/versions.md`](../test/e2e/checklists/versions.md), reviews the change, and
leaves it uncommitted for you to check and commit.

### At the branch cut (about three weeks before the release)

Finding: `STALE Forgejo N.0 was branched on ... : add it to Upcoming`.

1. Add the row to `Upcoming` in `internal/forgejo/versions.go`: `Major`, `LTS`, `Released`, `EOL`
   from the schedule, `UpgradeFrom` = the current LTS, `Traits` copied from the newest row.
2. Find what changed for the operator (needs judgement):
   - `release-notes-published/N.0.0.md` on the `forgejo` branch, sections *Breaking features*
     and *Breaking bug fixes*; PRs labelled `breaking` in the milestone *Forgejo vN.0.0*.
   - The config cheat sheet, CLI reference and upgrade guide of the docs branch `vN.0` (or
     `next`) against the previous one: every key `defaultConfig`, `haConfig` and the tuning
     render, and every command `bootstrapScript` runs.
   - The Dockerfiles (binary path, uid, volume), `/api/healthz`, `/api/v1/version`, `/metrics`.
   - Security announcements: https://codeberg.org/forgejo/security-announcements/issues
3. Express each difference as a trait: a new `bool` in `forgejo.Traits` with its rendering in
   `internal/core/config.go` or `workload.go`, set in the new row. If it changes `app.ini`, add
   it to `Traits.ConfigProfile` and `CompatTraits`. Unit-test it with
   `forgejo.Versions.WithVerified(N)`.
4. Run the e2e checklist against `codeberg.org/forgejo-experimental/forgejo:N.0-test` (and
   `-rootless`).

### On release day

Finding: `STALE Forgejo N.0 was released on ... but is not a verified release`.

1. Check the release for an upstream advisory (the tool's `WARN ... advise against`, open
   *release blocker* issues). If the first release is advised against, set an `Advisory`
   (`FixedIn: 1`) and verify with the fixed patch once it is out. The tool reads the notes of
   the newest patch of each major sentence by sentence: a sentence that says "advise against"
   and names that patch's own version is a `WARN` with the `FixedIn` to set; any other such
   sentence (for example "we no longer advise against 17.0.0" in the 17.0.1 notes) is an
   `INFO` that quotes it and needs no edit unless reading it says otherwise.
2. Verify on a cluster with the e2e checklist: fresh installs (root and rootless), the upgrade
   from the current LTS and from the previous release, a refused downgrade, backup and restore.
3. Move the row from `Upcoming` to `Releases`.
4. For a new LTS: change `DefaultImage` and the `+kubebuilder:default` marker of `spec.image` in
   `api/v1alpha1/forgejo_types.go`, run `make manifests generate`, and update
   `config/samples/forgejo_v1alpha1_forgejo.yaml` and the examples in the README and docs. The
   tool reports `STALE the default image ... is not the current LTS release` until this is done.
   Existing instances keep their image.
5. Update the support matrix and the calendar above.
6. `go test ./internal/...`: `TestVersionTableIsWellFormed` and `TestDefaultImageIsCurrentLTS`
   guard the edit; update the expectations of `TestSupportPolicyFollowsTheCalendar` and
   `TestClassify` for the new dates.
7. Release the operator.

### Patch releases

The tool lists the newest patch of every major and checks that both image flavors exist. When a
patch fixes a critical security issue, or upstream advises against an earlier patch, set the
row's `Advisory` (`FixedIn`, `Note`). Nothing else changes: floating tags pick up patches.

### End of life

Nothing to edit: the classification follows the date. Keep every row after its end of life.
Majors older than the oldest row are reported as `EndOfLife` and run with fixed legacy traits
(`forgejo.LegacyTraits`: `INTERNAL_TOKEN`, as every Forgejo before 18 needs), not with the
oldest row's, so that an instance left on an old major keeps its `app.ini`;
`TestVersionTableIsWellFormed` checks that the oldest row still has that profile.
