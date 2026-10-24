# Upgrades

Changing `spec.image` on a `Forgejo` starts an orchestrated upgrade. The operator checks
the versions, takes a cold pre-upgrade backup, rolls out the new image as a single pod that
migrates the database, verifies it, and rolls back automatically from the backup if the new
version does not come up. The code lives in `internal/upgrade`; the pipeline step runs after
`maintenance` and before `workload` (see [design.md](design.md)). Which Forgejo versions are
supported, and the notes for each major upgrade, are in [versions.md](versions.md).

Changing `spec.database.cnpg.imageName` to a newer PostgreSQL major upgrades the CNPG cluster
the same way: see [PostgreSQL major upgrades](#postgresql-major-upgrades). The two never run at
the same time.

```yaml
spec:
  # A floating tag can pull a patch release that an advisory names (17.0.0): pin the minimum
  # patch that docs/versions.md's support matrix names (N.0.P) once it is published.
  image: codeberg.org/forgejo/forgejo:17
  upgrade:
    backupBeforeUpgrade: true   # default
    autoRollback: true          # default
    timeout: 15m                # default; counted from the moment the new image is deployed
    allowDowngrade: false       # default
```

## Flow

`status.image` is the image that is verified running: core sets it only after the Deployment
has fully rolled out with it and every replica is available. An upgrade is a change of
`spec.image` away from `status.image`. A fresh install (empty `status.image`) is not an
upgrade; the image is simply deployed.

```
           version gate
spec.image ───────────► Pending ─► BackingUp ─► RollingOut ─► Verifying ─► Succeeded
                │          │           │              │            │
                │          └─────┬─────┘              └─────┬──────┘
                │                ▼                    failure detected
                │   Cancelled (spec.image changed)          │
                │   Failed (backup failed or pods           │
                │           did not stop, BackingUp only)   │
                ▼                                ┌──────────┴───────────┐
         DowngradeRefused                 autoRollback &&         otherwise
                                          backup taken                 │
                                                 ▼                     ▼
                                           RollingBack ─► RolledBack  Failed
                                                 │                  (new image
                                                 ▼                   kept)
                                              Failed
                                         (restore failed)
```

| Phase | What happens | Deployed image / replicas |
|-------|--------------|---------------------------|
| `Pending` | `fromImage`, `toImage`, `startTime` and the backup name are recorded. | old image, unchanged (a [retry](#retrying) keeps the new image) |
| `BackingUp` | Forgejo is stopped. Once no server pod is left, `ForgejoBackup <name>-pre-upgrade-<start>` (`reason: PreUpgrade`) is created and the operator waits for it to complete (on the backup PVC; with `spec.backup.remote` its upload runs afterwards and does not hold up the upgrade). If server pods are still there 15 minutes after `startTime` (a pod stuck terminating on an unreachable node or behind a finalizer), the upgrade is aborted (`Failed`, old image back at its usual scale) instead of taking a backup while something may still write. | old image, 0 replicas |
| `RollingOut` | The new image is deployed with the `Recreate` strategy and one replica, in the same reconcile that records `rolloutStartTime`. The `bootstrap` init container runs `forgejo migrate`. | new image, 1 replica |
| `Verifying` | The migration finished and the server container started; the operator waits until the pod is Ready, core reports the image as verified (`status.image`), and the Deployment has rolled it out (its current generation observed, every replica updated and available). | new image, 1 replica |
| `Succeeded` | `status.image` is the new image and the Deployment runs it with every replica available. The usual replica count, strategy and HPA come back. | new image |
| `RollingBack` | `ForgejoRestore <name>-rollback-<start>` restores the database and the data volume from the pre-upgrade backup while Forgejo is stopped, then the old image starts again. | old image, 0 → 1 replica |
| `RolledBack` | The old image is available again on the restored data. | old image |
| `Failed` | See [Failures without a rollback](#failures-without-a-rollback) and [Automatic rollback](#automatic-rollback). | old image; the new image with 1 replica; or 0 replicas after a failed rollback restore |
| `Cancelled` | `spec.image` changed again before the new image was deployed (see below). | old image |

With `spec.highAvailability.enabled`, the cron runner (StatefulSet `<name>-cron`) stops with the
server pods in `BackingUp` (its pod counts among the pods that must be gone before the backup),
stays at zero replicas through every phase in flight and while a failed image is held, and
starts again on the new image once the upgrade has `Succeeded`, so `forgejo migrate` runs in
exactly one pod. When an upgrade deploys the new image without a backup, the workload step first
scales the cron runner to zero and waits for its pod to stop (`WaitingForCronRunner`; a pod stuck
on an unreachable node keeps it waiting until the pod is force-deleted, `CronRunnerStuck`). See
[high-availability.md](high-availability.md#the-cron-runner).

The whole state machine is persisted in `status.upgrade`, so an operator restart resumes it
where it was. Every step is idempotent: the backup and restore have names derived from
`status.upgrade.startTime` and are only created after the phase that needs them has been
saved.

While an upgrade is in flight:

- The `Upgrading` condition is `True` with the phase as its reason, and `status.phase` is
  `Upgrading`. `Progressing` is `True` as well.
- Further changes of `spec.image` are not acted on once the new image may have been deployed
  (from `RollingOut` on, and always for upgrades without a pre-upgrade backup). An
  `UpgradeQueued` event records the new value, which is handled as a new upgrade once the
  current one has finished.
- Before that, in `Pending` or `BackingUp`, changing `spec.image` cancels the upgrade: the
  database has not been touched yet. A pre-upgrade backup that is already running finishes
  first, with Forgejo stopped, and is kept. The phase becomes `Cancelled` (`Upgrading=False`,
  reason `UpgradeCancelled`, an `UpgradeCancelled` event; not counted in
  `forgejo_operator_upgrades_total`), the old image comes back, and the new `spec.image` is
  handled from there: nothing to do when it is the running image again, otherwise a new
  upgrade. Reverting a mistaken major upgrade during the backup therefore needs no restore.
- Self-healing, config rollback, vertical scaling and the HPA leave the instance alone.
- A `ForgejoRestore` pauses the upgrade, see [Restores during an upgrade](#restores-during-an-upgrade).

## Version gate

The versions are read from the image tags (`16`, `16.0.6`, `v16.0.6`, `16.0-rootless`, ...).
When the running image carries no version (a digest, or a custom tag such as a mirror's
`prod`), its version is the one Forgejo reports (`status.version`). That version can lag
behind an image change until the next health check, so it is only used to refuse: a change it
alone shows to be the same version, or an allowed downgrade, is orchestrated like an
unparseable tag.

| Change | Result |
|--------|--------|
| Newer version | Upgrade. A jump in the major version emits a `MajorUpgrade` event; Forgejo migrates the database forward across majors, also when majors are skipped (15 → 17). A target outside the support policy (`Deprecated`, `EndOfLife`, or `Future`: newer than the operator's version table) is upgraded to anyway, with an `UnsupportedVersion` Warning event. |
| Newer version that skips more majors than the version table allows (e.g. 14 → 17: 17 is upgraded to from 15 or newer) | Refused like a downgrade: `Degraded=True` and `Upgrading=False` with reason `UpgradePathUnsupported`, one Warning event naming the major to upgrade to first, and the current image keeps running. See [versions.md](versions.md#upgrading-between-majors). |
| Same version (`16` ⇄ `16.0.6`, `16` → `16-rootless`, another registry) | Plain rollout of the new image without orchestration: nothing to migrate. A floating tag compares equal to every version it covers. |
| Older version | Refused: `Degraded=True` and `Upgrading=False` with reason `DowngradeRefused`, one Warning event, and the current image keeps running. Forgejo cannot migrate a database backwards. |
| Older version with `allowDowngrade: true` | Plain rollout with a `Downgrade` Warning event, applied even while a restore is running. An orchestrated downgrade could never verify: the old version refuses a database migrated by a newer one, and its rollback would restore the newer state. Combine it with a `ForgejoRestore` (see [Rolling back manually](#rolling-back-manually)). |
| Unparseable tag (`latest`, digest only), and no reported version to compare with | Orchestrated like an upgrade, with an `UnverifiedVersion` Warning event. |

An orchestrated upgrade does not start while a `ForgejoRestore` holds the instance in
maintenance mode (`Upgrading=False`, reason `WaitingForRestore`); it starts once the restore
has finished. Nor does it start while the Secret of SSH host keys is missing or unusable
(`Upgrading=False`, reason `WaitingForSSHHostKeys`): the new image's pods could not start (see
[ssh.md](ssh.md#when-something-is-wrong)), or while the endpoint of `spec.objectStorage` answers
with a failure (`Upgrading=False`, reason `WaitingForObjectStorage`; an endpoint only the operator
cannot reach holds nothing, see [object-storage.md](object-storage.md#status)).

## Restores during an upgrade

When maintenance mode starts while an upgrade is in flight (a `ForgejoRestore` scales the
instance to zero, a failed restore that changed the data holds it there, or a restore Job is
still running), the upgrade is **paused** in its phase until maintenance has ended:

- No phase advances: `Pending` does not stop Forgejo or deploy the new image, `BackingUp` does
  not create the pre-upgrade backup nor start the rollout once the backup has completed, and
  `RollingBack` does not create its restore. No failure is detected on the stopped pods.
- No timeout runs. `rolloutStartTime` is not set while paused, and when it is already set
  (`RollingOut`, `Verifying`, `RollingBack`) it is kept at the current time, so
  `spec.upgrade.timeout` counts again from the end of maintenance. The 15-minute stop
  timeout of `BackingUp` keeps counting from `startTime`: maintenance stops the pods as well.
- `Verifying` goes back to `RollingOut`: the pod is stopped, and after maintenance the new
  image starts again and migrates the database, which the restore may have replaced.
- `status.upgrade.message` says what the upgrade waits for, `Upgrading` stays `True` with the
  phase as its reason, and `UpgradePaused` / `UpgradeResumed` events mark the window.
- A cancellation still applies (`spec.image` changed in `Pending` or `BackingUp`): it deploys
  nothing.

The rollback's own `ForgejoRestore` (`status.upgrade.restoreName`) is what `RollingBack` waits
for, so the maintenance mode it causes does not pause the upgrade, and its failure ends the
rollback as described under [Automatic rollback](#automatic-rollback).

Once maintenance has ended the upgrade continues on whatever the restore left behind. The
restore waits in `ScalingUp` for Forgejo to come back, which happens when the upgrade has
deployed its image: in `BackingUp` the pre-upgrade backup is then taken of the restored data,
and in `RollingOut` the new image migrates the restored database.

## Failure detection

The upgrade fails when the new image's pods show any of:

- the image cannot be pulled (`ErrImagePull`/`ImagePullBackOff`) for more than 2 minutes, or
  the image name is invalid;
- a container (including the `bootstrap` init container, which means a failed migration) in
  `CrashLoopBackOff` after crashing at least twice, or restarted 3 times;
- not available within `spec.upgrade.timeout`, counted from `status.upgrade.rolloutStartTime`
  (or from the end of a maintenance window that [paused](#restores-during-an-upgrade) the upgrade).

The reason is stored in `status.upgrade.failureReason` and in an `UpgradeFailed` event.

## Automatic rollback

With `autoRollback: true` (the default) and a completed pre-upgrade backup, a failure moves
the upgrade to `RollingBack`:

1. Forgejo is scaled to zero right away, so the failing version cannot migrate any further.
2. `ForgejoRestore <name>-rollback-<start>` restores the pre-upgrade backup. The restore
   replaces the whole database schema, so tables a newer version created are removed too
   (see [backup-restore.md](backup-restore.md)). Its Jobs run with the old image.
3. The old image starts again on the restored data. Once it is available the phase is
   `RolledBack`: `failedImage` is the new image, `forgejo_operator_upgrades_total{result="rolledback"}`
   is incremented, an `UpgradeRolledBack` Auto Pilot action is recorded, and `Degraded=True`
   with reason `UpgradeRolledBack` until `spec.image` changes.

If the restore fails, or the old image is not available within the timeout after the restore,
the phase is `Failed` with `Degraded=True` (`UpgradeFailed`) and an `UpgradeRollbackFailed`
event. This needs a human: see [Rolling back manually](#rolling-back-manually).

- When the **restore failed** (`status.upgrade.rollbackRestoreFailed: true`), the database may
  still be in the new version's state (possibly half-migrated) and the data volume may be
  partly extracted. Forgejo **stays scaled to zero**, so that neither version serves users on
  it, until a `ForgejoRestore` created after the failure has restored a backup (it reaches
  `ScalingUp`; an `UpgradeRollbackRestored` event is emitted and the old image starts on the
  restored data), or until `spec.image` is changed. Setting `spec.image` back to `status.image`
  starts the old image without a restore; do that only when the new version is known not to
  have migrated anything (for example when its image could not be pulled).
- When the restore completed but the old image was only slow to become available, the data is
  consistent and the old image keeps running at its usual scale.

Rolling back always restores the backup, even when the new image never started (an image that
cannot be pulled). That costs a restore, but it is the only way to be sure the database matches
the old version.

## Failures without a rollback

| Situation | Outcome |
|-----------|---------|
| The pre-upgrade backup fails, or Forgejo's pods do not stop within 15 minutes for it | `Failed` before anything changed: the old image starts again, `failedImage` is set, `Degraded` (`UpgradeFailed`). |
| The new version fails and `backupBeforeUpgrade: false` or `autoRollback: false` | `Failed`, **the new image stays deployed with a single replica**, `failedImage` is set, `Degraded` (`UpgradeFailed`). |

The new image stays deployed in the second case because its migrations may already have
changed the database, and the old version refuses to start on a database migrated by a newer
one. Reverting the image without restoring the database is unsafe, so the operator leaves the
decision to you. Its migration may still be running (it outlived the timeout) or retrying
after a crash, and `forgejo migrate` takes no lock, so until the new image becomes available
the workload stays as during the upgrade: one replica, the `Recreate` strategy, no HPA, and no
restarts by self-healing, config rollback or vertical scaling. If the new version becomes
available later (a slow migration, a configuration fix), the upgrade turns into `Succeeded`
by itself (`UpgradeRecovered` event) and the usual replica count, strategy and HPA come back.

## Retrying

A failed image (`status.upgrade.failedImage`) is not retried automatically. Either:

- set `spec.image` to another image (for example a fixed patch release), or
- retry the same image:

  ```sh
  kubectl annotate forgejo <name> forgejo.lucas.icu/retry-upgrade=true
  ```

  The operator clears `failedImage`, starts a new upgrade (with a new pre-upgrade backup), and
  removes the annotation on the next reconcile. An annotation that has nothing to retry is
  removed as well, so it never triggers a retry later.

  When the failed image was left deployed (a failure without rollback), the retry rolls it
  out again **without** a backup (`status.upgrade.retry: true`): the database may already be
  migrated, so a backup taken now could not bring the old version back. `spec.upgrade.timeout`
  counts again from the retry. The pods of the failed attempt (created before `startTime`)
  are handled by their state: a pod that is failing (crash loop, image pull error, exited
  container) is deleted so that a fresh pod starts with a clean restart count, while a pod
  whose migration is still running, or whose server is running, is kept and is not judged by
  its earlier crashes; interrupting `forgejo migrate` could leave the schema half-migrated. If
  such a pod fails again it is deleted then. A new failure ends in `Failed` again.

## Rolling back manually

To go back to an older version after a `Succeeded` upgrade (or after a `Failed` one that kept
the new image), restore the database the old version wrote and deploy the old image:

1. Find the pre-upgrade backup: `status.upgrade.backupName`, or
   `kubectl get fjb -l forgejo.lucas.icu/backup-reason=PreUpgrade`.
2. Set the old image and allow the downgrade. It is rolled out as is; the old version refuses
   to start on the newer database (its `forgejo migrate` exits without changing anything), so
   the pod crash-loops harmlessly until step 3 is done:

   ```yaml
   spec:
     image: codeberg.org/forgejo/forgejo:15
     upgrade:
       allowDowngrade: true
   ```

3. Restore the backup. Forgejo is scaled to zero, the database and data volume are replaced,
   and the old image starts on them:

   ```yaml
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: ForgejoRestore
   metadata:
     name: manual-rollback
   spec:
     forgejoRef: {name: <name>}
     backupName: <pre-upgrade backup>
   ```

4. Remove `allowDowngrade` again.

Do step 2 before the restore finishes: if the newer image is still deployed when the restore
scales Forgejo back up, it migrates the restored database forward again.

When `failedImage` is held after a failure without rollback, setting `spec.image` back to
`status.image` is not a downgrade: the old image is deployed right away. That is enough when the
new version never migrated the database (it could not be pulled, or crashed before
`forgejo migrate` finished); otherwise restore the pre-upgrade backup as in step 3.

## Status and observability

```yaml
status:
  image: codeberg.org/forgejo/forgejo:17
  upgrade:
    phase: RolledBack
    fromImage: codeberg.org/forgejo/forgejo:17
    toImage: codeberg.org/forgejo/forgejo:99
    backupName: up-pre-upgrade-20261021071501
    restoreName: up-rollback-20261021071501
    startTime: ...
    rolloutStartTime: ...         # spec.upgrade.timeout counts from here
    completionTime: ...
    failedImage: codeberg.org/forgejo/forgejo:99
    failureReason: 'image codeberg.org/forgejo/forgejo:99 could not be pulled for 2m0s: ...'
    retry: false                  # true while retrying an image left deployed
    rollbackRestoreFailed: false  # true keeps Forgejo stopped, see "Automatic rollback"
    message: ...
```

- Conditions: `Upgrading` (reason = phase while in flight; `UpgradeSucceeded`,
  `UpgradeRolledBack`, `UpgradeFailed`, `UpgradeCancelled`, `DowngradeRefused`,
  `UpgradePathUnsupported`, `Rollout`, `UpToDate` otherwise) and
  `Degraded` with the reasons `UpgradeRolledBack`, `UpgradeFailed`, `DowngradeRefused` and
  `UpgradePathUnsupported`, which are cleared once they no longer apply. `VersionSupported`
  reports whether the running version is supported ([versions.md](versions.md#status)).
- Events: `UpgradeStarted`, `MajorUpgrade`, `UnverifiedVersion`, `Downgrade`,
  `PreUpgradeBackup`, `PreUpgradeBackupCompleted`, `UpgradeRollingOut`, `UpgradeSucceeded`,
  `UpgradeFailed`, `UpgradeRollingBack`, `UpgradeRolledBack`, `UpgradeRollbackFailed`,
  `UpgradeRollbackRestored`, `UpgradeRecovered`, `UpgradeQueued`, `UpgradeCancelled`,
  `UpgradePaused`, `UpgradeResumed`, `UpgradeRetry`, `DowngradeRefused`,
  `UpgradePathUnsupported`, `UnsupportedVersion`, `ConfigIncompatible`.
- Metric: `forgejo_operator_upgrades_total{result="succeeded|failed|rolledback"}`; the
  `ForgejoUpgradeFailed` alert fires on the latter two. Cancelled upgrades are not counted.

Pre-upgrade backups are never pruned by the backup retention; delete them when you no longer
need them.

## PostgreSQL major upgrades

With `spec.database.type: CNPG` (the default), changing `spec.database.cnpg.imageName` to an
image of a newer PostgreSQL major upgrades the cluster in place with CloudNativePG's offline
`pg_upgrade`, orchestrated like a Forgejo upgrade: Forgejo is stopped, a cold backup of the
database is taken with the old PostgreSQL image, CloudNativePG upgrades, and Forgejo is started
and verified on the new major. A failure is rolled back. The code is
`internal/upgrade/database*.go`, step `database-upgrade`, which runs before the `database` step
and decides which image that step writes to Cluster `<name>-db`.

```yaml
spec:
  database:
    cnpg:
      imageName: ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie   # the default
  upgrade:
    autoRollback: true   # default; also rolls back a PostgreSQL upgrade from its backup
    timeout: 15m         # default; Forgejo on the new major must come up within it (a revert: at least 15m)
```

External databases (`spec.database.type: External`) are never upgraded by the operator: their
`spec.database.cnpg.imageName` only selects the `pg_dump` and `pg_restore` of backup and restore
Jobs, so give it the server's major.

### Images

New instances get `ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie`: the current
CloudNativePG major (PostgreSQL 18, maintained upstream until 2030), the `minimal` flavour (the
database uses the C collation and no extension) on Debian trixie (stable). Like the Forgejo
image it floats: the `MM-TYPE-OS` and `MM.mm-TYPE-OS` tags are rebuilt every week. Pin
`18.6-minimal-trixie` (a minor release), an immutable `MM.mm-TS-TYPE-OS` tag or a digest
(`...:18-minimal-trixie@sha256:...`) to control the updates. The API server refuses an image
whose tag does not start with the major version (CloudNativePG reads the major from the tag and
refuses `latest` and a digest alone).

The operator verifies PostgreSQL **17 and 18** with Forgejo 15 and 17: fresh installs, the
upgrade 17 → 18, Forgejo upgrades and backups and restores on both. Majors 14 to 16 run with an
`UnsupportedVersion` warning. Older ones (Forgejo needs 14 or newer) and newer ones (CloudNativePG
1.30 supports up to 18; PostgreSQL 19 is at a release candidate) are refused.

The deprecated tags `MM` and `MM.mm` (`ghcr.io/cloudnative-pg/postgresql:17`, the default of
earlier operator builds) are bullseye `system` images that are no longer built. CloudNativePG
upgrades a major only between images of the same Debian release, so such a cluster first moves
to trixie within its major (`17-minimal-trixie`, a change that CloudNativePG rolls out and that
the C collation makes safe), then upgrades.

### Version gate

The major of the data comes from the Cluster's status (`status.pgDataImageInfo`), and is
reported in `status.database.image`, `majorVersion` and `systemID`; the requested major and the
Debian release from the tag of `spec.database.cnpg.imageName`.

| Change | Result |
|--------|--------|
| Same major (another minor release, flavour, registry or Debian release) | Written to the Cluster as it is: CloudNativePG replaces the instances one by one (its rolling update), Forgejo keeps running |
| Newer major, same Debian release | Orchestrated upgrade (below) |
| Newer major, another Debian release (`17-minimal-bookworm` → `18-minimal-trixie`) | Refused: `Degraded=True` and `DatabaseUpgrading=False` with reason `PostgresUpgradePathUnsupported`, one Warning event; the current image keeps running. `pg_upgrade` runs the old binaries with the new image's libraries |
| Older major | Refused: reason `PostgresDowngradeRefused`. No major moves back in place; see [Rolling back a PostgreSQL upgrade](#rolling-back-a-postgresql-upgrade) |
| A major below 14 or above 18 | Refused: reason `PostgresVersionUnsupported`. A new instance's Cluster is not created (`DatabaseReady=False`) |
| Majors 14 to 16 | Allowed, with an `UnsupportedVersion` Warning event |
| A tag the operator cannot read (an unknown Debian release, e.g. a custom `18-custom`) | The release check is skipped with an `UnverifiedVersion` Warning event |

The `database` step also never writes a major older than the data's: CloudNativePG's webhook
would refuse it on every pass.

An upgrade does not start (`DatabaseUpgrading=False`, the current image keeps running) while a
`ForgejoRestore` of the instance has not finished, also one that still waits in `Pending` and one
in `ScalingUp` whose first pod migrates the restored database, while a failed restore holds the
instance in maintenance, while `spec.restoreFrom` is being restored, and after a rollback whose
restore failed until a restore has lifted that hold (`WaitingForRestore`, see
[Failures](#failures-of-a-postgresql-upgrade)); while a Forgejo upgrade is in flight or holds the
workload (`WaitingForForgejoUpgrade`), before Forgejo has rolled out for the first time
(`WaitingForFirstRollout`), while the CNPG cluster is not healthy (`WaitingForDatabase`), and
while the SSH host keys or the bucket of `spec.objectStorage` are unusable
(`WaitingForSSHHostKeys`, `WaitingForObjectStorage`): Forgejo has to start on the new major to
verify it.

### Flow

```
                 version gate
spec.database ─────────────► Pending ─► BackingUp ─► Upgrading ─────────────────► Verifying ─► Succeeded
.cnpg.imageName                 │           │            │ Job failed or                │ Forgejo fails
                                ▼           ▼            ▼ image not pulled             ▼ on the new major
                           Cancelled     Failed      Reverting ──────────────────► RollingBack ─► RolledBack
                                       (nothing          │ old data back  (not back,    │ backup unusable: Failed (Cluster kept)
                                        changed)         ▼ (same system  pg_upgrade     │ restore failed: Failed (Forgejo stays stopped)
                                                    RolledBack     ID)   had started)
                                                    (not back, pg_upgrade never started: Failed, Cluster kept)
```

| Phase | What happens | Forgejo | Cluster image |
|-------|--------------|---------|---------------|
| `Pending` | `fromImage`, `toImage`, the majors, the system ID and the backup name are recorded | unchanged | old |
| `BackingUp` | Forgejo and the cron runner are stopped (the HPA is removed meanwhile). Once no Forgejo pod, runner registration pod and backup or restore pod of the instance is left, `ForgejoBackup <name>-pre-pg<major>-<start>` (`reason: PreUpgrade`, **database only**) dumps the database with the running, old PostgreSQL image. Its pods must stop within 15 minutes of `startTime`, else the upgrade is aborted | 0 | old |
| `Upgrading` | The new image is written to the Cluster in the pass that records `rolloutStartTime`. CloudNativePG stops PostgreSQL and runs `pg_upgrade --link` in Job `<primary>-major-upgrade` | 0 | new |
| `Verifying` | The data is on the new major (`status.pgDataImageInfo`). Forgejo starts with one replica once the database serves again; the upgrade succeeds when the Deployment is available on a new system ID. Only pods started for the verification are judged: a pod created before the new image was written, or one that has finished (evicted, stopped by a node shutdown), ran on the old major | 1 | new |
| `Succeeded` | The usual replicas, cron runner and HPA come back. Replicas of an instances: 2 cluster are cloned again by CloudNativePG meanwhile (`DatabaseReady` reports it) | usual | new |
| `Reverting` | `pg_upgrade` failed or could not start: the old image is written back, CloudNativePG deletes the upgrade Job and starts the old data again. `pgUpgradeStarted` records whether the container that runs `pg_upgrade` had started | 0 | old |
| `RollingBack` | The old data does not come back after `pg_upgrade` had started, or Forgejo fails on the new major: the Cluster is deleted (only while the pre-upgrade backup can restore it) and created again on the old image, and the backup is restored into it | 0 | old (recreated) |
| `RolledBack` | The old major runs: `failedImage` is the new image, `Degraded=True/PostgresUpgradeRolledBack` until `spec.database.cnpg.imageName` changes | usual | old |
| `Failed` | See [Failures](#failures-of-a-postgresql-upgrade) | see there | old, or new without `autoRollback` or when the backup could not restore the old one |
| `Cancelled` | `spec.database.cnpg.imageName` changed before the new image was written | usual | old |

The state is persisted in `status.database.upgrade`, so an operator restart resumes it; the
backup and the restore have names derived from `startTime` and are created only once the phase
that needs them is saved. While it is in flight, `DatabaseUpgrading=True` (reason = phase),
`status.phase` is `Upgrading` and `Progressing=True`. On a local cluster a 17 → 18 upgrade of a
small database took about 75 s, the `pg_upgrade` Job 40 s of it; with two instances another 20 s
for cloning the replica.

**One upgrade at a time.** While a PostgreSQL upgrade is in flight:

- a change of `spec.image` (also the retry annotation) waits: `Upgrading=False/WaitingForDatabaseUpgrade`,
  and the Forgejo upgrade starts once the database upgrade has finished;
- a `ForgejoRestore` waits in `Pending` and a `ForgejoBackup` in `Pending` ("Waiting for the
  PostgreSQL major upgrade ..."), except the upgrade's own backup and restore; scheduled backups
  are deferred. The upgrade's own backup does not wait for such a restore (it could not start
  before the upgrade has finished), only for one that has already started;
- `ForgejoRunner` registration Jobs wait (they write to the database through `forgejo-cli`);
  runner pods and Valkey keep running;
- autopilot does not restart, resize or roll back Forgejo, and storage autopilot does not grow
  the CNPG volumes (`StorageHealthy=False/ExpansionDeferred` while one is above the threshold);
- a change of `spec.database.cnpg.imageName` cancels the upgrade in `Pending` and `BackingUp`
  (a running pre-upgrade backup finishes first; one still `Pending` is not waited for and runs
  later like a manual backup), and is queued from `Upgrading` on (`DatabaseUpgradeQueued`
  event): `pg_upgrade` is never interrupted.

### Failures of a PostgreSQL upgrade

| Situation | Outcome |
|-----------|---------|
| The pre-upgrade backup fails, Forgejo's pods do not stop within 15 minutes, or the Cluster does not show the new image 2 minutes after it was written (e.g. refused by CloudNativePG) | `Failed` before anything changed: the old major keeps running, Forgejo starts again |
| CloudNativePG's upgrade Job fails (`BackoffLimitExceeded`), or its pod cannot pull an image for 2 minutes | `Reverting`: the old image is written back. `RolledBack` once the primary serves the old major with the **same system ID** (replicas that are still starting are CloudNativePG's to bring back): nothing was restored, nothing lost |
| The Job runs longer than `spec.upgrade.timeout` | Only reported (`DatabaseUpgradeSlow` event, message): aborting `pg_upgrade --link` can leave the old data unusable |
| The old data does not come back within `spec.upgrade.timeout`, and at least 15 minutes, after `pg_upgrade` had started (a `pg_upgrade --link` that failed after it started linking leaves the old data directory unusable) | `RollingBack` |
| The old data does not come back within that time although `pg_upgrade` never started (`pgUpgradeStarted: false`: an image that cannot be pulled) | `Failed`, nothing deleted: the old data is untouched, so the Cluster keeps it and is not rolled back from the backup; Forgejo starts once the cluster serves again |
| Forgejo crash-loops on the new major, or is not available within `spec.upgrade.timeout` of `Verifying` | `RollingBack`: the old data directory is gone after a successful `pg_upgrade` |
| `RollingBack`: Forgejo is stopped, Cluster `<name>-db` deleted; once its pods, volumes, Secrets and Services are gone, it is created again on the old image (with at least the old volume size, a new system ID and a new `<name>-db-app` password), and once it has served for 15 s (`DatabaseReady`), `ForgejoRestore <name>-pg-rollback-<start>` restores the pre-upgrade backup (database only: Forgejo was stopped from the backup on, so the data volume matches) | `RolledBack` when the restore reaches `ScalingUp` |
| `RollingBack`, but the pre-upgrade backup, read again before the Cluster is deleted, cannot restore it: it is missing or being deleted, not `Completed`, records no database dump or one of another major than the old one, or its volume is missing or being deleted | `Failed` without deleting anything (`DatabaseUpgradeRollbackFailed` event), as with `autoRollback: false`: the Cluster keeps its data; restore a backup by hand |
| The rollback's restore fails | `Failed` with `rollbackRestoreFailed: true`: the recreated database may be empty, so **Forgejo stays stopped** until a `ForgejoRestore` created afterwards has restored a backup; `DatabaseUpgradeRollbackRestored` event. Meanwhile no upgrade starts: another image of the new major and the retry annotation wait (`DatabaseUpgrading=False/WaitingForRestore`, the annotation stays) and start once that restore has finished; a change of `spec.database.cnpg.imageName` does not lift the hold either |
| A failure with `spec.upgrade.autoRollback: false` | `Failed`: the cluster keeps the new major (or the revert that did not come up); restore the backup by hand |

A rollback from the backup loses nothing users wrote: Forgejo is stopped from the backup on, and
an upgrade succeeds as soon as Forgejo is available on the new major, so a rollback only follows
a Forgejo that never served on it.

**Retrying.** A failed image (`status.database.upgrade.failedImage`) is not tried again
automatically. Set another image of the new major, or retry the same one (with a new backup):

```sh
kubectl annotate fj <name> forgejo.lucas.icu/retry-database-upgrade=true
```

The annotation is removed once read, as for Forgejo upgrades. After a rollback whose restore
failed (`rollbackRestoreFailed: true`) restore a backup first: until then neither starts an
upgrade, since it would back up and upgrade a database that may be empty. A Job failure that leaves
`*.failed_*` directories next to the old data makes every later `pg_upgrade` of that cluster fail
too; CloudNativePG's documentation describes how to clean them up.

### Rolling back a PostgreSQL upgrade

There is no way back in place after `Succeeded`: `pg_upgrade` deleted the old data directory and
CloudNativePG refuses an older major (`PostgresDowngradeRefused`). To return to the old major,
recreate the cluster from a backup taken with it, such as the pre-upgrade backup
(`status.database.upgrade.backupName`; its `status.postgresImage` is the old image). Everything
written after that backup is lost; a backup taken on the new major cannot be restored into the old
one (`NewerDump`).

1. Stop Forgejo: `spec.replicas: 0` (and disable horizontal autoscaling).
2. Set `spec.database.cnpg.imageName` to the old image (`Degraded=True/PostgresDowngradeRefused`
   until the next step).
3. Delete the Cluster and wait until its volumes are gone:
   `kubectl delete cluster.postgresql.cnpg.io <name>-db` and
   `kubectl get pvc -l cnpg.io/cluster=<name>-db` until it lists nothing. The operator creates the
   Cluster again, empty, on the old image.
4. Restore the backup into it once `DatabaseReady=True`:

   ```yaml
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: ForgejoRestore
   metadata:
     name: pg-rollback
   spec:
     forgejoRef: {name: <name>}
     backupName: <pre-upgrade backup>
     restoreData: false
   ```

5. Set `spec.replicas` back.

### After the upgrade

- PostgreSQL 18's `pg_upgrade` keeps most planner statistics; the operator runs no `ANALYZE`. To
  fill in what is missing (or, on an older target major, all of them), run on the primary:

  ```sh
  P=$(kubectl get cluster.postgresql.cnpg.io <name>-db -o jsonpath='{.status.currentPrimary}')
  kubectl exec $P -c postgres -- vacuumdb --all --analyze-in-stages --missing-stats-only   # PostgreSQL 18
  ```
- The operator does not configure CloudNativePG's own backups (WAL archiving, the barman-cloud
  plugin). If you added them, change their `serverName` with the upgrade: a new major starts a new
  system ID and timeline, and its archive must not mix with the old one.

### Status, events and metrics

```yaml
status:
  database:
    image: ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie   # the data's image (pg_dump/pg_restore of Jobs)
    majorVersion: 18
    systemID: "7700285913296703512"
    upgrade:
      phase: Succeeded
      fromImage: ghcr.io/cloudnative-pg/postgresql:17-minimal-trixie
      toImage: ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie
      fromMajor: 17
      toMajor: 18
      fromSystemID: "7700285381722689561"
      backupName: fj-pre-pg18-20261024172151
      restoreName: ""                   # fj-pg-rollback-<start> after a rollback from the backup
      startTime: ...
      rolloutStartTime: ...             # the new image was written
      pgUpgradeStarted: true            # the container that runs pg_upgrade started
      phaseStartTime: ...               # Verifying and Reverting count spec.upgrade.timeout from here
      completionTime: ...
      failedImage: ""
      failureReason: ""
      storageSize: ""                   # the deleted Cluster's volume size (rollback)
      rollbackRestoreFailed: false
      message: Upgraded PostgreSQL from 17 (...) to 18 (...) in 1m11s; system ID ...
```

- `kubectl get fj -o wide` shows the major in the `POSTGRES` column.
- Conditions: `DatabaseUpgrading` (reason = phase while in flight; otherwise `UpToDate`,
  `Rollout`, `PostgresUpgradeSucceeded`, `PostgresUpgradeRolledBack`, `PostgresUpgradeFailed`,
  `PostgresUpgradeCancelled`, `PostgresDowngradeRefused`, `PostgresUpgradePathUnsupported`,
  `PostgresVersionUnsupported` or one of the `WaitingFor...` reasons above), and `Degraded` with
  the reasons `PostgresUpgradeRolledBack`, `PostgresUpgradeFailed`, `PostgresDowngradeRefused`,
  `PostgresUpgradePathUnsupported` and `PostgresVersionUnsupported`.
- Events (action `DatabaseUpgrade`): `DatabaseUpgradeStarted`, `DatabaseUpgradeBackup`,
  `DatabaseUpgradeBackupCompleted`, `DatabaseUpgradeApplying`, `DatabaseUpgradeVerifying`,
  `DatabaseUpgradeSucceeded`, `DatabaseUpgradeFailed`, `DatabaseUpgradeSlow`,
  `DatabaseUpgradeRollingBack`, `DatabaseUpgradeRolledBack`, `DatabaseUpgradeRollbackFailed`,
  `DatabaseUpgradeRollbackRestored`, `DatabaseUpgradeCancelled`, `DatabaseUpgradeQueued`,
  `DatabaseUpgradePaused`, `DatabaseUpgradeResumed`, `DatabaseUpgradeRetry`,
  `PostgresDowngradeRefused`, `PostgresUpgradePathUnsupported`, `PostgresVersionUnsupported`,
  `UnsupportedVersion`, `UnverifiedVersion`.
- Metric: `forgejo_operator_database_upgrades_total{result="succeeded|failed|rolledback"}`; the
  `ForgejoDatabaseUpgradeFailed` alert ([runbook](runbooks/ForgejoDatabaseUpgradeFailed.md))
  fires on the latter two. Cancelled upgrades are not counted.

Pre-upgrade backups are never pruned by the backup retention; delete them when you no longer
need them.
