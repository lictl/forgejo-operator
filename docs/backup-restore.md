# Backup and restore

The operator backs Forgejo up as two artifacts:

1. A **PostgreSQL logical dump** (`pg_dump --format=custom`) of the Forgejo database.
2. A **gzip-compressed tar archive of the data volume**: repositories, LFS objects,
   attachments, avatars, packages, the JWT signing key and SSH host keys.

Both go to a PVC in the instance's namespace. Backups are described by `ForgejoBackup`
objects and restored with `ForgejoRestore` objects. Scheduled backups and retention are
configured on the `Forgejo` resource.

## Taking a backup

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: ForgejoBackup
metadata:
  name: before-migration
spec:
  forgejoRef:
    name: forgejo
  includeData: true   # default: the instance's spec.backup.includeData (true)
  reason: Manual      # Manual | Scheduled | PreUpgrade (informational; drives retention)
```

```console
$ kubectl get fjb
NAME               FORGEJO   REASON   PHASE       COMPLETED   AGE
before-migration   forgejo   Manual   Completed   12s         40s
```

`status` records `phase` (`Pending` → `Running` → `Completed`/`Failed`), `startTime`,
`completionTime`, `jobName`, `pvcName`, `path`, `forgejoImage` (the image running when the
backup was taken), `databaseSizeBytes`, `dataSizeBytes`, `includesData`, `message` and a
`Ready` condition. Events are recorded on the backup and on the `Forgejo` object.

What happens:

* The backup waits while a `ForgejoRestore` of the same instance is in progress. It fails
  straight away if the instance does not exist, and fails after 2 minutes if the database is
  not ready or the backup PVC cannot be provisioned.
* The operator creates the backup PVC `<forgejo>-backups` on first use, using
  `spec.backup.storage` (`size` 20Gi by default, `storageClassName`), or uses
  `spec.backup.storage.existingClaim`. It is `ReadWriteOnce`. The PVC is owned by the
  `Forgejo` object (and deleted with it) only when `spec.storage.retainPolicy: Delete`.
* A Job `<backup>-job` runs one pod:
  * Init container `dump-database` uses the instance's PostgreSQL image
    (`spec.database.cnpg.imageName`, also for external databases). It connects with
    `PGHOST`/`PGPORT`/`PGDATABASE`/`PGSSLMODE`, and `PGUSER`/`PGPASSWORD` come from the
    credentials Secret through `secretKeyRef`, so passwords never appear in a pod spec. It
    writes `db.dump`.
  * Container `archive` uses the Forgejo image that is currently deployed. It mounts the
    data volume read-only, writes `data.tar.gz` and `manifest.json`, and reports the sizes
    through its termination message, from which the controller fills `status`.
  * Both containers run as uid/gid 1000 with no capabilities, a read-only root filesystem
    and `umask 077`, so backup files are readable only by uid 1000.
  * The data volume is `ReadWriteOnce`. When Forgejo pods are running, the Job gets a
    required pod affinity to them (same node). Otherwise it is pinned to the node affinity
    of the data PV. The instance's tolerations, node selector and image pull secrets apply.
  * The Job has `backoffLimit: 1` and `activeDeadlineSeconds` of 24h. If its pod cannot
    start a container within 10 minutes (unschedulable, image pull errors, rejected by
    admission), the Job is deleted and the backup fails with the reason.

### Files

```
<forgejo>-backups PVC
└── <backup-name>/
    ├── db.dump         # pg_dump custom format (compressed); restore with pg_restore
    ├── data.tar.gz     # tar of the data volume, paths relative to /data (./git, ./gitea, ...)
    └── manifest.json   # backup name, instance, reason, images, pg_dump version,
                        # timestamps, sizes, excluded paths, tar warnings
```

Every file is first written under a temporary name and renamed when complete. A crashed
backup never leaves a half-written `db.dump` or `data.tar.gz`. The data archive is checked
with `tar -tzf` before it is renamed.

The data archive leaves out content Forgejo recreates or rebuilds by itself:
`lost+found`, `gitea/tmp`, `gitea/repo-archive` (archive download cache), `gitea/log`,
`gitea/queues` (pending background tasks) and `gitea/indexers` (bleve search indexes; they
are re-populated from the database on start).

### Consistency

* **Database**: `pg_dump` reads a single MVCC snapshot, so the dump is transactionally
  consistent even while Forgejo is serving.
* **Data volume**: the archive is taken **live**. Files that change or disappear while tar
  reads them (tar exit code 1, "file changed as we read it" / "No such file or directory")
  are tolerated and counted as `warnings` in the manifest and status message. Any other tar
  error, a gzip error or a failing archive check fails the backup.
  Git writes objects immutably and updates refs atomically, so a live copy of a repository
  is usable. A push that lands during the backup may show up in the archive without its
  database rows, or the reverse. Forgejo's `doctor` can reconcile these.
* **Pre-upgrade backups** (`reason: PreUpgrade`) are taken by the upgrade orchestration
  while Forgejo is stopped, so they are fully consistent. For a fully consistent manual
  backup, scale Forgejo to zero (`spec.replicas: 0`) first.
* Forgejo's own `forgejo dump` is not used. Its database export has known long-standing
  bugs, and it produces a single archive that cannot be restored selectively.

### Deleting a backup

`ForgejoBackup` objects carry the finalizer `forgejo.lucas.icu/finalizer`. Deleting
one stops its Job if it is still running, then runs a cleanup Job `<backup>-cleanup` that
removes `<pvc>/<backup-name>/`. The finalizer is released once that is done. Cleanup waits
while an unfinished `ForgejoRestore` is reading the backup. The finalizer is also released
when the PVC no longer exists, when cleanup fails, or after 10 minutes; a Warning event tells
you to remove the directory by hand.

## Scheduled backups and retention

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata:
  name: forgejo
spec:
  backup:
    enabled: true
    schedule: "0 3 * * *"   # standard 5-field cron (or @daily, @every 6h, ...), operator's time zone (UTC in-cluster)
    retention: 7            # completed scheduled backups to keep
    includeData: true
    storage:
      size: 50Gi
```

* At each tick the operator creates `ForgejoBackup` `<name>-scheduled-<yyyymmddhhmmss>`
  with `reason: Scheduled`. Missed ticks, for example while the operator was down, are not
  backfilled; a single backup is taken when the operator next runs. A tick that falls while
  another backup of the instance is `Pending` or `Running` waits for that backup to finish.
* The first tick after enabling is the next schedule time; there is no immediate backup.
* While scheduling is enabled, retention keeps the newest `retention` **completed**
  scheduled backups. It also removes failed scheduled backups that are older than the
  newest completed one. Manual and pre-upgrade backups are never pruned, and neither is a
  backup that an unfinished restore is reading. Pruning deletes the `ForgejoBackup`, and
  the finalizer then removes its files.
* Scheduled backups get an owner reference to the `Forgejo` object only under
  `retainPolicy: Delete`. With `Retain` they outlive the instance.
* `status.backup` reports `lastBackupName`, `lastSuccessfulBackupName`,
  `lastSuccessfulBackupTime`, `lastScheduleTime` and `nextScheduleTime`.
* Condition `BackupHealthy`:
  * `True`/`BackupSucceeded`: the latest scheduled backup succeeded and it is not older
    than two scheduled runs.
  * `False`/`BackupFailed`: the latest scheduled backup failed.
  * `False`/`BackupStale`: two runs have passed since the last success.
  * `False`/`InvalidSchedule`: the cron expression is invalid.
  * `Unknown`: no scheduled backup has finished yet.
  * The condition is removed when `enabled` is false.
* Metrics:
  * `forgejo_operator_backups_total{reason,result}`
  * `forgejo_operator_backup_last_success_timestamp_seconds`
  * `forgejo_operator_backup_duration_seconds`
  * `forgejo_operator_backup_size_bytes{part="database|data"}`

## Restoring

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: ForgejoRestore
metadata:
  name: undo-migration
spec:
  forgejoRef:
    name: forgejo
  backupName: before-migration
  restoreDatabase: true   # default true
  restoreData: true       # default true; ignored when the backup has no data archive
```

| Phase | What happens |
|-------|--------------|
| `Pending` | Validates that the backup exists, is `Completed` and belongs to the same instance. Fails otherwise. Waits for another restore of the instance to finish. |
| `ScalingDown` | The Forgejo pipeline turns on maintenance mode: `status.backup.activeRestore` is set, the Deployment is scaled to 0, the phase becomes `Restoring` and health checks and autopilot actions are suspended. The restore waits until no Forgejo pod exists and no backup of the instance is running. It fails after 15 minutes if Forgejo does not scale down, for example because `spec.paused` is set or the reconcile is blocked earlier. |
| `Restoring` | Job `<restore>-job` runs (see below). If its pod cannot start within 10 minutes, the restore fails before anything is changed. |
| `ScalingUp` | Maintenance ends and Forgejo is scaled back up. The bootstrap init container runs `forgejo migrate` against the restored database. The restore waits up to 15 minutes for the Deployment to be available. |
| `Completed` / `Failed` | Terminal. A failed restore also ends maintenance. |

The restore Job:

1. `verify` (Forgejo image, uid 1000) checks the files before anything is touched.
   `db.dump` must be a pg_dump custom archive and `data.tar.gz` must list cleanly.
2. `restore-database` (PostgreSQL image, uid 1000) replaces the database **in one
   transaction**. The script sends `BEGIN`, then `DROP SCHEMA public CASCADE; CREATE SCHEMA
   public AUTHORIZATION <app user>`, then the output of
   `pg_restore --no-owner --no-acl db.dump` into `psql -v ON_ERROR_STOP=1`. `COMMIT` is sent
   only if pg_restore succeeded, so any failure leaves the database exactly as it was.
   * Recreating the schema removes every object, including tables that a newer Forgejo's
     migrations created after the backup. `pg_restore --clean` would leave those behind,
     which matters for rolling back an upgrade.
   * `--no-owner --no-acl` makes the app user own everything, which is what Forgejo needs.
     The original owner and grants are not replayed.
   * This requires the app user to be able to drop schema `public`, that is, to own the
     database, as with CloudNativePG. An external database user without that right falls
     back to `pg_restore --clean --if-exists` (logged as a warning).
3. `restore-data` (Forgejo image, runs as root with only `CHOWN`, `FOWNER` and
   `DAC_OVERRIDE`) deletes everything on the data volume except `lost+found`, extracts the
   archive, and `chown -R 1000:1000`. Root is needed to replace files of any owner on any
   storage backend. In a namespace that enforces the `restricted` Pod Security Standard,
   data restores are rejected; use `baseline`.

Notes:

* Restore a backup into a Forgejo that runs the same or a newer version. Forgejo
  migrations only go forward, and an older binary refuses a newer database. The operator
  emits a `NewerBackup` warning event when `status.forgejoImage` of the backup is newer
  than the deployed image.
* Excluded content (search indexes, queues, caches) is rebuilt after the restore.
* Do not delete a `ForgejoRestore` while it is `Restoring`. Its Job is garbage-collected
  and the data may be left half-restored.
* Metric: `forgejo_operator_restores_total{result}`.

### Restoring into a new instance (disaster recovery)

A backup belongs to the instance named in its `spec.forgejoRef`. To recover after the
instance was deleted (`retainPolicy: Retain` keeps the backup PVC and the `ForgejoBackup`
objects):

1. Recreate the `Forgejo` with the same name. It reuses `<name>-backups`.
2. Wait until it is `Running`.
3. Create a `ForgejoRestore` for the backup.

If the `ForgejoBackup` object is gone but its files remain, the operator cannot restore
them, because a new `ForgejoBackup` always takes a fresh backup. Restore such files by hand,
as described at the end of the next section.

## Copying backups off-cluster

The backup PVC is `ReadWriteOnce` and usually lives on the same node as the data volume,
so the copy pod should run there. While Forgejo is running, start it next to Forgejo:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: backup-shell
spec:
  securityContext: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  affinity:
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels: {app.kubernetes.io/name: forgejo, app.kubernetes.io/instance: forgejo, app.kubernetes.io/component: server}
  containers:
    - name: shell
      image: codeberg.org/forgejo/forgejo:16
      command: ["sleep", "infinity"]
      volumeMounts: [{name: backups, mountPath: /backups, readOnly: true}]
  volumes:
    - name: backups
      persistentVolumeClaim: {claimName: forgejo-backups, readOnly: true}
```

```console
$ kubectl cp backup-shell:/backups/before-migration ./before-migration
$ kubectl exec backup-shell -- tar -C /backups -cf - before-migration | gzip -1 > before-migration.tar.gz  # or stream it
$ kubectl delete pod backup-shell
```

You can also push from inside the pod with `rclone`, `aws s3 cp` or `restic` from a
different image, mounting the same PVC read-only.

To restore files that have no `ForgejoBackup` object, for example ones brought back from
off-cluster, do by hand what the restore Job does. First scale Forgejo to zero
(`spec.replicas: 0`). Then run the following in pods that mount the data PVC (as root) and
reach the database with the app user's credentials:

```console
$ psql -v ON_ERROR_STOP=1 -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION CURRENT_USER'
$ pg_restore --no-owner --no-acl --single-transaction --exit-on-error -d "$PGDATABASE" db.dump
$ find /data -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
$ tar -xzf data.tar.gz -C /data && chown -R 1000:1000 /data
```

For production disaster recovery, also consider continuous archiving and point-in-time
recovery of PostgreSQL with CloudNativePG's own backup support (object storage). Use
storage snapshots for very large data volumes.
