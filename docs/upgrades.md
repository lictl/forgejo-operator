# Upgrades

Changing `spec.image` on a `Forgejo` starts an orchestrated upgrade. The operator checks
the versions, takes a cold pre-upgrade backup, rolls out the new image as a single pod that
migrates the database, verifies it, and rolls back automatically from the backup if the new
version does not come up. The code lives in `internal/upgrade`; the pipeline step runs after
`maintenance` and before `workload` (see [design.md](design.md)).

```yaml
spec:
  image: codeberg.org/forgejo/forgejo:16
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
| `BackingUp` | Forgejo is stopped. Once no server pod is left, `ForgejoBackup <name>-pre-upgrade-<start>` (`reason: PreUpgrade`) is created and the operator waits for it to complete. If server pods are still there 15 minutes after `startTime` (a pod stuck terminating on an unreachable node or behind a finalizer), the upgrade is aborted (`Failed`, old image back at its usual scale) instead of taking a backup while something may still write. | old image, 0 replicas |
| `RollingOut` | The new image is deployed with the `Recreate` strategy and one replica, in the same reconcile that records `rolloutStartTime`. The `bootstrap` init container runs `forgejo migrate`. | new image, 1 replica |
| `Verifying` | The migration finished and the server container started; the operator waits until the pod is Ready and core reports the image as verified. | new image, 1 replica |
| `Succeeded` | `status.image` is the new image. The usual replica count, strategy and HPA come back. | new image |
| `RollingBack` | `ForgejoRestore <name>-rollback-<start>` restores the database and the data volume from the pre-upgrade backup while Forgejo is stopped, then the old image starts again. | old image, 0 → 1 replica |
| `RolledBack` | The old image is available again on the restored data. | old image |
| `Failed` | See [Failures without a rollback](#failures-without-a-rollback) and [Automatic rollback](#automatic-rollback). | old image; the new image with 1 replica; or 0 replicas after a failed rollback restore |
| `Cancelled` | `spec.image` changed again before the new image was deployed (see below). | old image |

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

## Version gate

The versions are read from the image tags (`16`, `16.0.6`, `v16.0.6`, `16.0-rootless`, ...).

| Change | Result |
|--------|--------|
| Newer version | Upgrade. A jump in the major version emits a `MajorUpgrade` event; Forgejo migrates the database forward across majors. |
| Same version (`16` ⇄ `16.0.6`, `16` → `16-rootless`, another registry) | Plain rollout of the new image without orchestration: nothing to migrate. A floating tag compares equal to every version it covers. |
| Older version | Refused: `Degraded=True` and `Upgrading=False` with reason `DowngradeRefused`, one Warning event, and the current image keeps running. Forgejo cannot migrate a database backwards. |
| Older version with `allowDowngrade: true` | Plain rollout with a `Downgrade` Warning event, applied even while a restore is running. An orchestrated downgrade could never verify: the old version refuses a database migrated by a newer one, and its rollback would restore the newer state. Combine it with a `ForgejoRestore` (see [Rolling back manually](#rolling-back-manually)). |
| Unparseable tag (`latest`, digest only) | Orchestrated like an upgrade, with an `UnverifiedVersion` Warning event. |

An orchestrated upgrade does not start while a `ForgejoRestore` holds the instance in
maintenance mode (`Upgrading=False`, reason `WaitingForRestore`); it starts once the restore
has finished.

## Failure detection

The upgrade fails when the new image's pods show any of:

- the image cannot be pulled (`ErrImagePull`/`ImagePullBackOff`) for more than 2 minutes, or
  the image name is invalid;
- a container (including the `bootstrap` init container, which means a failed migration) in
  `CrashLoopBackOff` after crashing at least twice, or restarted 3 times;
- not available within `spec.upgrade.timeout`, counted from `status.upgrade.rolloutStartTime`.

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
  image: codeberg.org/forgejo/forgejo:16
  upgrade:
    phase: RolledBack
    fromImage: codeberg.org/forgejo/forgejo:16
    toImage: codeberg.org/forgejo/forgejo:99
    backupName: up-pre-upgrade-20261021071501
    restoreName: up-rollback-20261021071501
    startTime: ...
    rolloutStartTime: ...
    completionTime: ...
    failedImage: codeberg.org/forgejo/forgejo:99
    failureReason: 'image codeberg.org/forgejo/forgejo:99 could not be pulled for 2m0s: ...'
    retry: false                  # true while retrying an image left deployed
    rollbackRestoreFailed: false  # true keeps Forgejo stopped, see "Automatic rollback"
    message: ...
```

- Conditions: `Upgrading` (reason = phase while in flight; `UpgradeSucceeded`,
  `UpgradeRolledBack`, `UpgradeFailed`, `UpgradeCancelled`, `DowngradeRefused`, `Rollout`,
  `UpToDate` otherwise) and
  `Degraded` with the reasons `UpgradeRolledBack`, `UpgradeFailed` and `DowngradeRefused`, which
  are cleared once they no longer apply.
- Events: `UpgradeStarted`, `MajorUpgrade`, `UnverifiedVersion`, `Downgrade`,
  `PreUpgradeBackup`, `PreUpgradeBackupCompleted`, `UpgradeRollingOut`, `UpgradeSucceeded`,
  `UpgradeFailed`, `UpgradeRollingBack`, `UpgradeRolledBack`, `UpgradeRollbackFailed`,
  `UpgradeRollbackRestored`, `UpgradeRecovered`, `UpgradeQueued`, `UpgradeCancelled`,
  `UpgradeRetry`, `DowngradeRefused`.
- Metric: `forgejo_operator_upgrades_total{result="succeeded|failed|rolledback"}`; the
  `ForgejoUpgradeFailed` alert fires on the latter two. Cancelled upgrades are not counted.

Pre-upgrade backups are never pruned by the backup retention; delete them when you no longer
need them.
