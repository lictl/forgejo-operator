# Backup and restore

The operator backs Forgejo up as two artifacts:

1. A **PostgreSQL logical dump** (`pg_dump --format=custom`) of the Forgejo database.
2. A **gzip-compressed tar archive of the data volume**: repositories, LFS objects,
   attachments, avatars, packages and the JWT signing key. With `spec.objectStorage`, the files
   kept in the bucket are not on the volume, so not in the archive: the bucket's own versioning
   or copies protect them (see [object-storage.md](object-storage.md#backups)).

It also keeps a copy of the SSH host keys from their Secret (see [ssh.md](ssh.md)).

Both go to a PVC in the instance's namespace. Backups are described by `ForgejoBackup`
objects and restored with `ForgejoRestore` objects. Scheduled backups and retention are
configured on the `Forgejo` resource.

With `spec.backup.remote`, every backup is then also uploaded, encrypted, to an S3-compatible
bucket outside the cluster, together with the instance's identity (its Secrets): see
[Remote backups](#remote-backups). A remote backup can be restored into an instance by its
location ([Restoring a remote backup](#restoring-a-remote-backup)), and a lost instance can be
recreated from one, in a new namespace or cluster, with `spec.restoreFrom`
([Disaster recovery](#disaster-recovery)).

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
backup was taken), `postgresImage` (the PostgreSQL image whose `pg_dump` wrote the dump),
`databaseSizeBytes`, `dataSizeBytes`, `includesData`, `message`, a
`Ready` condition and, with `spec.backup.remote`, `remote` (see
[Remote backups](#remote-backups)). Events are recorded on the backup and on the `Forgejo` object. Status is
the record of a finished backup: the Job is deleted once its outcome is written there (see
[Jobs are deleted when they finish](#jobs-are-deleted-when-they-finish)).

What happens:

* The backup waits while a `ForgejoRestore` of the same instance is in progress, while the
  instance is being created from a remote backup (`spec.restoreFrom`), and while a PostgreSQL
  major upgrade is in flight (except that upgrade's own backup, which waits only for restores
  that have started: one that has not waits for the upgrade, see
  [upgrades.md](upgrades.md#postgresql-major-upgrades)). It fails
  straight away if the instance does not exist, and fails after 2 minutes if the database is
  not ready or the backup PVC cannot be provisioned.
* The operator creates the backup PVC `<forgejo>-backups` on first use, using
  `spec.backup.storage` (`size` 20Gi by default, `storageClassName`), or uses
  `spec.backup.storage.existingClaim`. It is `ReadWriteOnce`. The PVC is owned by the
  `Forgejo` object (and deleted with it) only when `spec.storage.retainPolicy: Delete`.
  Changing `retainPolicy` later updates the ownership of the existing PVC and of the
  existing `ForgejoBackup`s on the next reconcile (see
  [Scheduled backups and retention](#scheduled-backups-and-retention)).
* Raising `spec.backup.storage.size` grows the existing operator-managed PVC on the next
  reconcile, without waiting for a backup, when its StorageClass has
  `allowVolumeExpansion: true`. Otherwise a `StorageResizeUnsupported` Warning event is
  recorded on the `Forgejo`, once per hour for the same claim and size. The claim is never
  shrunk; a lower size, or one below what was
  already grown by hand, is ignored, so setting the size back to the claim's size stops
  the warning. An `existingClaim` is never resized. Plan for about
  `(retention + 1) ×` the size of one backup, plus manual and pre-upgrade backups, which
  retention does not prune.
* A Job `<backup>-backup` runs one pod:
  * Init container `dump-database` uses the PostgreSQL image of the running CNPG cluster's data
    (`status.database.image`: during a PostgreSQL major upgrade the old one, so that the dump can
    be restored into the old major), and `spec.database.cnpg.imageName` for external databases
    (give it the server's major) and before the cluster reports one. It connects with
    `PGHOST`/`PGPORT`/`PGDATABASE`/`PGSSLMODE`, and `PGUSER`/`PGPASSWORD` come from the
    credentials Secret through `secretKeyRef`, so passwords never appear in a pod spec. It
    writes `db.dump`. With an external database in `sslMode: verify-ca` or `verify-full`,
    the server certificate is verified against the image's CA bundle
    (`/etc/ssl/certs/ca-certificates.crt` in the CloudNativePG images), the same system
    roots Forgejo itself uses; without a bundle, `verify-full` falls back to libpq's
    `sslrootcert=system` (PostgreSQL 16 or newer) and `verify-ca` fails with a clear message.
    A server certificate signed by a private CA cannot be verified yet (there is no CA
    setting); use `sslMode: require` for such a server.
  * Container `archive` uses the Forgejo image that is currently deployed. It mounts the
    data volume read-only, writes `data.tar.gz` and `manifest.json`, and reports the sizes
    through its termination message, from which the controller fills `status`. It also mounts
    the Secret of SSH host keys read-only (optional: it may not exist yet) and copies the keys
    into `ssh-host-keys/`, also for database-only backups.
  * Both containers run as uid/gid 1000 with no capabilities, a read-only root filesystem
    and `umask 077`, so backup files are readable only by uid 1000.
  * Placement. A `ReadWriteOnce` data volume is attached to the node of the running Forgejo
    pods, so a Job that mounts it gets a required pod affinity to them (same node). Without
    running pods, or for a `ReadWriteMany`/`ReadOnlyMany` data volume, which any node can
    attach, only the data PV's node affinity applies. The backup PVC is a constraint too:
    while it is still unbound, every Job that uses it (database-only backups and cleanup
    Jobs included) is placed next to the data volume, so that a node-local or zonal backup
    volume is provisioned where the data is. Once it is bound, the scheduler follows its
    PV's node affinity; a Job that also mounts the data volume gets both constraints. If no
    node satisfies both (the backup volume lives on another node or zone than the data), the
    backup or restore fails right away with reason `BackupVolumeConflict` instead of a Job
    that never starts. Recreate the backup PVC, or use a StorageClass that both nodes can
    reach. The instance's tolerations, node selector and image pull secrets apply.
  * The Job has `backoffLimit: 1` and `activeDeadlineSeconds` of 24h. If its pod cannot
    start a container within 10 minutes (unschedulable, image pull errors, rejected by
    admission), the Job is deleted and the backup fails with the reason.

### Files

```
<forgejo>-backups PVC
└── <backup-name>/
    ├── db.dump         # pg_dump custom format (compressed); restore with pg_restore
    ├── data.tar.gz     # tar of the data volume, paths relative to /data (./git, ./gitea, ...)
    ├── ssh-host-keys/  # ssh_host_{ed25519,ecdsa,rsa}_key: the SSH host keys from their Secret
    └── manifest.json   # backup name, instance, reason, images, pg_dump version,
                        # timestamps, sizes, excluded paths, tar warnings, host key files,
                        # the bucket and subsystems of spec.objectStorage (or null)
```

Every file is first written under a temporary name (`*.partial`) and renamed when complete.
A crashed backup never leaves a half-written `db.dump` or `data.tar.gz`. The data archive is
checked with `tar -tzf` before it is renamed. A failing step deletes its partial files on exit.
When a backup fails, the operator also runs the cleanup Job `<backup>-cleanup` to remove its
whole directory, after its Job has stopped. This covers a container killed by OOM, for
example. A failed backup cannot be restored, and its leftovers would otherwise keep a full
volume full. The `ForgejoBackup` object stays, so you can still see its status. The outcome
goes into the condition `FilesRemoved`, and then the cleanup Job is deleted:

| `FilesRemoved` | Meaning |
|----------------|---------|
| `True`/`Removed` | The directory is gone. |
| `True`/`VolumeDeleted` | The backup PVC was deleted (or is being deleted) while the cleanup ran. |
| `False`/`CleanupFailed` | The cleanup Job failed; the message has its output. |
| `False`/`CleanupStalled` | Its pod could not start a container within 10 minutes. |

When the condition is `False`, deleting the `ForgejoBackup` tries the cleanup again.

The data archive leaves out content Forgejo recreates or rebuilds by itself:
`lost+found`, `gitea/tmp`, `gitea/repo-archive` (archive download cache), `gitea/log`,
`gitea/queues` (pending background tasks) and `gitea/indexers` (bleve search indexes; they
are re-populated from the database on start).

### Consistency

* **Database**: `pg_dump` reads a single MVCC snapshot, so the dump is transactionally
  consistent even while Forgejo is serving.
* **Data volume**: the archive is taken **live**, while Forgejo keeps serving.
  * Repositories (`git/repositories/<owner>/<name>.git`) are archived in a git-safe order.
    Each repository's refs come first (loose refs, then `packed-refs`), then its other
    files, then its loose objects, and its packs last. Git writes objects before it moves a
    ref and never changes an object afterwards, so every ref in the archive points to
    objects that tar reads later. tar reads each directory when it gets to it, so a push
    that lands in the meantime is either included with its objects or not at all. Push
    quarantine directories (`objects/tmp_objdir-*`, `objects/incoming-*`) and packs that
    are still being written are left out.
  * A concurrent repack (`git gc`) can delete a pack between the moment tar lists it and
    the moment it reads it. A ref captured earlier could then point to objects that are
    missing from the archive. If a pack or `packed-refs` of a repository that still exists
    vanishes, the run fails, and the Job's retry (`backoffLimit: 1`) takes a fresh
    archive. If the retry hits the same race, the backup is `Failed`, not silently
    incomplete.
  * Other files that change or disappear while tar reads them (tar exit code 1, "No such
    file or directory") are tolerated and counted as `warnings` in the manifest and status
    message. Any other tar error, a failing listing of the volume, a gzip error or a failing
    archive check fails the backup.
  * The database dump is taken before the archive. A push that lands during the backup may
    show up in the archive without its database rows; Forgejo's `doctor` can reconcile
    these.
* **Pre-upgrade backups** (`reason: PreUpgrade`) are taken by the upgrade orchestration
  while Forgejo is stopped, so they are fully consistent. When a backup must be fully
  consistent, stop Forgejo first. Use `spec.replicas: 0`, but note that this does not stop
  an instance whose HPA owns the replica count (`spec.autoscaling.horizontal.enabled` with
  `spec.highAvailability.enabled`): disable horizontal autoscaling as well (the cron runner
  `<name>-cron` stops with the server pods). Alternatively,
  take CSI `VolumeSnapshot`s of the data volume, which are crash-consistent.
* Forgejo's own `forgejo dump` is not used. Its database export has known long-standing
  bugs, and it produces a single archive that cannot be restored selectively.

### Deleting a backup

`ForgejoBackup` objects carry the finalizer `forgejo.lucas.icu/finalizer`. Deleting
one stops its Job (and its upload Job) if it is still running, then runs a cleanup Job
`<backup>-cleanup` that removes `<pvc>/<backup-name>/`. Deleting a `ForgejoBackup` never deletes
its remote copy: only remote retention does, for scheduled backups (see
[Remote retention](#remote-retention)). If the files of a failed backup were already removed
(`FilesRemoved=True`), deletion finishes right away. The finalizer is released once that is
done, and the cleanup Job goes with the object. Cleanup waits
while an unfinished `ForgejoRestore` is reading the backup. The finalizer is also released
when the PVC no longer exists, when cleanup fails, or after 10 minutes; a Warning event tells
you to remove the directory by hand. When the namespace is being deleted, no cleanup Job can
be created there, and the backup PVC is deleted with the namespace anyway, so the finalizer
is released right away and the namespace deletion is not held up (the files remain only if
the PersistentVolume's reclaim policy is `Retain`).

A backup that is deleted only because the `Forgejo` that owned it is gone keeps its files
when the backup PVC outlives that instance (it is not controlled by it). This covers
deleting the instance right after switching `retainPolicy` to `Retain`, before the operator
removed the backup's owner reference. A `BackupFilesKept` event names the PVC and the
directory. The operator reads the owner from the API server, not from its cache, for this
decision. A backup deleted explicitly, while its instance exists or without an owner
reference, is always cleaned up. So is one whose instance had `retainPolicy: Delete`, since
its backup PVC is deleted too. An `existingClaim` is never owned by the instance, so its
files stay in this case.

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
  scheduled backups. Of the failed scheduled backups, it keeps only the newest, so that
  `BackupHealthy` can report it, and only while no completed backup is newer. It removes
  the rest whether or not a backup has succeeded since. Completed backups within
  `retention` are never removed to make room. Manual and pre-upgrade backups are never
  pruned, and neither is a backup that an unfinished restore is reading. With
  `spec.backup.remote`, a completed backup beyond `retention` whose upload is still `Pending` or
  `Uploading` is kept until the upload has finished (25 hours after the backup at most, longer
  than an upload Job may run), then pruned on the next pass: deleting it would stop the upload.
  Pruning deletes the `ForgejoBackup`, and
  the finalizer then removes its files. Independently of retention, the files of every
  failed backup are removed right after it fails (see [Files](#files)); its object stays.
* A tick that falls while the instance is being upgraded (or while a failed upgrade still
  holds its new image's migration, or while it is created from `spec.restoreFrom`) is deferred
  until that has settled, so a dump is
  never taken mid-migration and labelled with the old image. A deferred run does not count
  as missed for `BackupHealthy`.
* Under `retainPolicy: Delete`, scheduled and pre-upgrade backups get an owner reference to
  the `Forgejo` object and are deleted with it. Manual backups never get one. With `Retain`,
  no backup carries a reference to the instance, and they outlive it. Changing
  `retainPolicy` updates the owner references of the existing `ForgejoBackup`s and of the
  backup PVC on the next reconcile. This does not depend on the database or the workload
  being ready.
* `status.backup` reports `lastBackupName`, `lastSuccessfulBackupName`,
  `lastSuccessfulBackupTime`, `lastScheduleTime` and `nextScheduleTime`.
* Condition `BackupHealthy`:
  * `True`/`BackupSucceeded`: the latest scheduled backup succeeded and it is not older
    than two scheduled runs. With `spec.backup.remote`, the message adds "uploaded to
    s3://..." or, while its upload runs, "uploading to s3://...".
  * `False`/`BackupFailed`: the latest scheduled backup failed.
  * `False`/`UploadFailed`: the latest scheduled backup succeeded on the backup PVC, but its
    upload to `spec.backup.remote` failed; the message has the reason.
  * `False`/`BackupStale`: two runs have passed since the last success.
  * `False`/`InvalidSchedule`: the cron expression is invalid.
  * `Unknown`: no scheduled backup has finished yet.
  * The condition is removed when `enabled` is false.
* Metrics:
  * `forgejo_operator_backups_total{reason,result}`
  * `forgejo_operator_backup_last_success_timestamp_seconds`: completion time of the newest
    successful backup of any reason, from `status.backup.lastSuccessfulBackupTime`. It is
    exported on every reconcile, so it is back right after an operator restart, and it never
    moves back; it disappears when the instance has no successful backup left.
  * `forgejo_operator_backup_duration_seconds`
  * `forgejo_operator_backup_size_bytes{part="database|data"}`
  * `forgejo_operator_backup_uploads_total{reason,result}` and
    `forgejo_operator_backup_last_upload_success_timestamp_seconds` for remote backups (see
    [Remote backups](#status-events-metrics-and-alerts)).

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
| `Pending` | Validates that the backup exists, is `Completed` and belongs to the same instance (for a remote backup: reads and checks its manifest, see [Restoring a remote backup](#restoring-a-remote-backup)). Fails otherwise, and with reason `NewerDump` when the database is to be restored and the dump was written by `pg_dump` of a newer PostgreSQL major than the CNPG cluster runs (`status.postgresImage` of the backup, `postgresImage` of a remote manifest): a newer dump is not guaranteed to load into an older server. Waits for a PostgreSQL major upgrade of the instance to finish (except the upgrade's own rollback restore), for another restore of the instance to finish, while the Secret of SSH host keys is missing or unusable (`status.ssh.message` of the instance; Forgejo could not start again, see [ssh.md](ssh.md#when-something-is-wrong)), and while the Secrets of `spec.objectStorage` are missing or unusable (`ObjectStorageReady=False/CredentialsUnavailable` or `InvalidSpec`; message `Waiting for spec.objectStorage: ...`): no pod could start without them, and the instance's pipeline would not scale it down. The instance keeps serving meanwhile, and the restore starts once the Secrets are back. A bucket the operator cannot reach does not hold a restore. A restore already in `ScalingDown` when a Secret disappears times out (`ScaleDownTimeout`). |
| `ScalingDown` | The Forgejo pipeline turns on maintenance mode: `status.backup.activeRestore` is set, the Deployment is scaled to 0, the phase becomes `Restoring` and health checks and autopilot actions are suspended. The restore waits until no Forgejo pod exists, no backup of the instance is running and no Job or pod of another restore of the instance is still running. It fails after 15 minutes if that does not happen, for example because `spec.paused` is set or the reconcile is blocked earlier. |
| `Restoring` | Job `<restore>-restore` runs (see below); `status.jobStartTime` records when it was created. If its newest pod cannot start within 10 minutes, the Job is stopped and the restore fails. It reports "nothing was changed" only when no earlier pod got past `verify`. Otherwise the failure counts as one that may have changed the instance (see below). A Job that is not found a minute after `jobStartTime` fails the restore with `JobMissing` only after the operator has confirmed with the API server that it is gone. |
| `ScalingUp` | Maintenance ends and Forgejo is scaled back up. With `spec.highAvailability.enabled`, Valkey is emptied first (event `ValkeyReset`): its pods stop together and start again without data, which takes under a minute, and no Forgejo pod starts before one is Ready. Its queues held work created after the backup point, by row ID (webhook deliveries, PR checks, push updates, mirror syncs), which would act on unrelated rows once the restored database hands out those IDs again, and its cache state from after that point. The bootstrap init container runs `forgejo migrate` against the restored database, in a single pod until that pod is available. The restore waits up to 15 minutes for the Deployment to be available. It completes right away when the instance wants no pods (`spec.replicas: 0` without horizontal autoscaling; with an HPA, `minReplicas` applies). |
| `Completed` / `Failed` | Terminal. The restore Job is deleted (see [Jobs are deleted when they finish](#jobs-are-deleted-when-they-finish)). See below for when a failed restore ends maintenance. |

Each kind has its own Job suffix (`-backup`, `-cleanup`, `-restore`), so a restore may have
the same name as its backup. A backup or restore never uses a Job it does not control. If a
Job with its name already exists and belongs to something else, the object fails with
reason `JobNameConflict`. For a restore this happens before maintenance mode starts. A Job
that is being deleted, or that was left by an earlier object of the same name, is waited for.

The restore records how far its Job got in the condition `DataModified`. The controller
looks at every pod of the Job, not only the newest:

| `DataModified` | Meaning |
|----------------|---------|
| absent / `False` | Nothing was changed. No pod passed `verify`, or `restore-database` failed and its transaction was rolled back. |
| `Unknown` | A pod passed `verify`, and whether it committed anything is not known. |
| `True` | `restore-database` exited 0 (the transaction committed) or `restore-data` started (it empties the volume first). |

A failed restore whose `DataModified` is `False` ends maintenance, and Forgejo runs on the
untouched instance. A failed restore whose `DataModified` is `True` or `Unknown` **keeps
Forgejo scaled to zero**. Its message, the `Forgejo`'s `Ready=False/Maintenance` condition
and a `MaintenanceHeld` event say so. The phase is `Degraded`, not `Restoring`: this is an
outage that only a person ends, so `ForgejoDown` and `ForgejoNotReady` fire and the availability
SLI counts it as unplanned. The data volume may be empty or half-extracted, and
the database may already hold the backup. The hold ends when one of these happens:

* A new `ForgejoRestore` of the instance gets to `ScalingUp`. This is the usual fix: correct
  the cause, then restore again. The new restore takes over maintenance mode while it runs.
* You set `forgejo.lucas.icu/release-maintenance: "true"` on the failed restore. This
  starts Forgejo on whatever the restore left behind.
* You delete the failed restore.

During an upgrade rollback the same hold applies. The rollback is reported as failed, and
Forgejo stays scaled to zero.

The restore Job:

1. `verify` (Forgejo image, uid 1000) checks the files before anything is touched.
   `db.dump` must be a pg_dump custom archive and `data.tar.gz` must list cleanly.
2. `restore-database` (the running cluster's PostgreSQL image, `status.database.image`, uid
   1000) replaces the database **in one transaction**. The script sends `BEGIN`, then `DROP SCHEMA public CASCADE; CREATE SCHEMA
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
3. `restore-data` (Forgejo image, runs as root with only `CHOWN`, `FOWNER`, `DAC_OVERRIDE`
   and `FSETID`) deletes everything on the data volume except `lost+found`, extracts the
   archive, and `chown -R 1000:1000`. This step cannot be rolled back. A failure here
   leaves the database restored and the volume partly extracted, so the instance stays in
   maintenance (see above). Root is needed to replace files of any owner on any
   storage backend. In a namespace that enforces the `restricted` Pod Security Standard,
   data restores are rejected; use `baseline`.
4. `reset-admin-sync` (Forgejo image, uid 1000) runs instead of `restore-data` in a
   database-only restore. It mounts the data volume and removes
   `gitea/.admin-password-sync` (see below). A failure only logs a warning.

Admin password after a restore. Forgejo's bootstrap init container sets the admin user's
password from the `<forgejo>-admin` Secret when that Secret changed since it last did so. It
records this in `gitea/.admin-password-sync` on the data volume.

* A full restore (database and data) brings back the database and the marker from the same
  moment. If the Secret changed after the backup, the password is set from the Secret
  again. Otherwise the restored database keeps the admin password it had at backup time,
  including a password changed in the web UI.
* A database-only restore removes the marker, so the next pod sets the admin password from
  the current Secret. The data volume's marker belongs to the database that was replaced.
* A data-only restore keeps the current database, and the restored marker only causes a
  reset when the Secret changed after the backup.

Notes:

* Restore a backup into a Forgejo that runs the same or a newer version. Forgejo
  migrations only go forward, and an older binary refuses a newer database. The operator
  emits a `NewerBackup` warning event when `status.forgejoImage` of the backup is newer
  than the deployed image.
* Excluded content (search indexes, queues, caches) is rebuilt after the restore. No queued
  work from after the backup point survives a restore: a data restore empties the volume,
  including `gitea/queues` (the LevelDB queues without HA), a database-only restore removes
  `gitea/queues` in its `reset-admin-sync` step, and with HA Valkey is emptied before Forgejo
  starts (see `ScalingUp`). Undelivered webhooks are queued again from the restored database
  when Forgejo starts. The rollback of a failed upgrade is a restore too, so the older version
  never consumes queue items that the newer one wrote.
* `ForgejoRestore` objects carry the finalizer `forgejo.lucas.icu/finalizer`.
  Deleting one stops its Job. The object, and maintenance mode while it is `ScalingDown`
  or `Restoring`, stays until none of the Job's pods can still write. The `restore-data`
  step stops within seconds of the TERM signal. A pod stuck on a lost node keeps the
  deletion waiting until the pod is gone; force-delete the pod once the node is known to
  be down.
* Do not delete a `ForgejoRestore` while it is `Restoring` unless you mean to abort it.
  Once the pods are gone, maintenance ends and Forgejo starts on a database or data volume
  that may be half-restored. A `RestoreAborted` Warning event on the `Forgejo` reports this.
  To recover, run another restore.
* Metric: `forgejo_operator_restores_total{result}`.

### Jobs are deleted when they finish

A finished pod still names the PVCs it mounted, and Kubernetes' PVC protection keeps a
deleted claim `Terminating` until such pods are gone. Backup, restore and cleanup pods mount
the data PVC and the backup PVC, so keeping their Jobs would leave those claims `Terminating`
when an instance with `retainPolicy: Delete` is deleted (a manual backup or a restore is not
owned by the instance, so garbage collection would not remove them). The operator therefore
deletes each Job, with background propagation so that its pods go too, as soon as it has
written the outcome to status:

* `<backup>-backup`: when the backup is `Completed` or `Failed`. `status.message` keeps the
  failing container's output (termination message, or the tail of its log), and
  `databaseSizeBytes`, `dataSizeBytes`, `startTime` and `completionTime` the rest.
* `<backup>-cleanup` of a failed backup: when `FilesRemoved` is recorded. The cleanup Job of
  a deleted backup goes with the `ForgejoBackup`.
* `<backup>-upload`: when the upload is recorded in `status.remote` (`Uploaded` or `Failed`),
  together with Secret `<backup>-identity`, which handed the identity bundle to it.
* `<restore>-restore`: when the restore is `Completed` or `Failed`. It is kept while the
  restore is `ScalingUp`, because its completion time starts the 15-minute scale-up
  timeout; a restore that is `ScalingUp` completes right away when its instance is being
  deleted. `status.message` and `DataModified` keep what the Job did.

A Job that still runs when its object fails (for example a restore whose instance was
deleted) is deleted once it finishes. Finished Jobs left by earlier operator versions are
deleted on the next reconcile. Jobs are not given `ttlSecondsAfterFinished`: a Job removed
while the operator is down would be reported as `JobMissing`, failing a backup or restore
that succeeded. Use `kubectl get events` or the `ForgejoBackup`/`ForgejoRestore` status to
look at past runs, not the Jobs.

Status updates of `ForgejoBackup` and `ForgejoRestore` use optimistic locking, so a
reconcile that started from an outdated copy (still `Running`, say, after the Job was
recorded and deleted) cannot overwrite the recorded result with `JobMissing`; it retries
with the current object instead.

### Restoring into a new instance of the same cluster

A backup belongs to the instance named in its `spec.forgejoRef`. To recover after the
instance was deleted (`retainPolicy: Retain` keeps the backup PVC and the `ForgejoBackup`
objects, also when it was switched from `Delete` shortly before):

1. Recreate the `Forgejo` with the same name. It reuses `<name>-backups`, and the retained
   Secrets `<name>-secrets` and `<name>-ssh-host-keys`, so it keeps its SSH host key
   fingerprints.
2. Wait until it is `Running`.
3. Create a `ForgejoRestore` for the backup.

A restore never changes the SSH host keys: they belong to the instance, not to the backup.
After losing the namespace or the cluster, use a remote backup and `spec.restoreFrom` instead
([Disaster recovery](#disaster-recovery)), which brings the identity back as well.

## Remote backups

`spec.backup.remote` uploads every backup (scheduled, manual and pre-upgrade) to an
S3-compatible bucket once it is complete on the backup PVC, encrypted with a key that never
leaves the cluster except where you keep it:

```yaml
spec:
  backup:
    enabled: true
    retention: 7
    remote:
      s3:                                  # the S3Connection of spec.objectStorage
        endpoint: https://s3.eu-central-1.amazonaws.com
        bucket: acme-forgejo-backups       # another bucket than spec.objectStorage, or another prefix
        region: eu-central-1
        prefix: forgejo
        credentialsSecret: {name: backup-s3}   # AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
        # tls: {caSecret: {name: my-ca}}       # a private CA (https endpoints)
      encryption:
        keySecret: {name: my-backup-key, key: key}   # optional; default: generated "<name>-backup-key"
      # image: docker.io/rclone/rclone:1.75.2      # the default
```

The access key needs to list, read, write and delete objects below the prefix. The operator
never creates the bucket.

### What is uploaded

```
s3://<bucket>/<prefix>/<namespace>/<name>/<backup>-<uid>/
├── db.dump.bin        # the files of the backup PVC, encrypted (rclone crypt appends .bin)
├── data.tar.gz.bin    # (when the backup has a data archive)
├── identity.json.bin  # the instance's identity, see below
└── manifest.json.bin  # uploaded last: the backup is complete only when it exists
```

`<uid>` is the first 8 characters of the `ForgejoBackup`'s UID (`status.remote.path` has the
whole path). Every `ForgejoBackup` gets a directory of its own: a backup created again under an
old name, or one of the same name taken by an instance of the same namespace and name in another
cluster (a recovery drill while production runs, the original cluster coming back after a
recovery), never writes into, or cleans up, another backup's directory.

* **identity.json** holds Secret `<name>-secrets` (`SECRET_KEY`, `INTERNAL_TOKEN`, the JWT
  secrets, the metrics token, the admin sync salt), the admin Secret (`<name>-admin`, or
  `admin.existingSecret`), the Secret of SSH host keys, and the `Forgejo` object (metadata and
  spec, without status, for reference). Forgejo derives the keys of two-factor (TOTP) secrets,
  Actions secrets, push-mirror credentials and webhook secrets from `SECRET_KEY`: a database
  restored without it loses all of them. The backup encryption key itself is not part of it.
* **manifest.json** names the backup and its instance (with the UIDs of both), the reason and
  Forgejo image, the fingerprint of the encryption key and of `SECRET_KEY`, and the size and
  SHA-256 of every file before encryption. It embeds the manifest of the backup PVC, which records the bucket and
  subsystems of `spec.objectStorage`.
* **The object storage bucket is not copied.** Files that `spec.objectStorage` keeps in a bucket
  (LFS objects, attachments, avatars, packages, Actions data) are not on the data volume and so
  not in any backup. Protect that bucket with the object store's own versioning, replication or
  Object Lock (see [object-storage.md](object-storage.md#backups)); after a disaster, point
  `spec.objectStorage` of the recreated instance at the same bucket and prefix.

### How it is uploaded

When the backup Job has finished, the `ForgejoBackup` is `Completed`: restores and upgrades do
not wait for the upload. `status.remote` goes `Pending` → `Uploading` → `Uploaded` or `Failed`:

1. The controller checks the key Secret (at least 16 characters, no surrounding white space),
   the credentials and the CA, then writes the identity bundle into Secret
   `<backup>-identity`, controlled by the backup.
2. Job `<backup>-upload` runs one container of the rclone image (uid 1000, read-only root file
   system, no capabilities) with the backup PVC mounted read-only. rclone is configured
   entirely through environment variables: the access key comes from the credentials Secret
   (`secretKeyRef`), and the backup key from its Secret (`BACKUP_KEY`), which the script hands
   to `rclone obscure -` on stdin, so no secret is in the pod spec or on a command line.
3. The Job lists the backup's directory and stops if it holds a `manifest.json` already: it never
   writes into a complete backup. Each file is hashed (SHA-256), uploaded through rclone's crypt
   remote (streamed, multipart for large files, so nothing is staged), then read back from the
   bucket, decrypted, and hashed again; a mismatch fails the upload. `manifest.json` follows the
   same way, last.
4. The controller records the files with their sizes and checksums and `verified: true`, then
   deletes the Job and the identity Secret.

`status.remote` records the bucket (`s3`) and the key (`keySecret`) the Job writes with. A failed
upload never fails the backup: its copy on the backup PVC remains. When the Job failed or
vanished, the controller waits until its pods have stopped (a pod of a deleted Job may still
write), then looks at the backup's directory with that recorded connection, whatever
`spec.backup.remote` says by then:

* this backup's own complete manifest (the Job uploaded everything and lost only its result,
  e.g. a Job deleted before the operator saw it succeed): the upload is `Uploaded`;
* objects without a manifest: what the upload left, deleted;
* a manifest it cannot read, or one of another backup: left as it is, and the message says so.

Nothing else is deleted. A pod that stays terminating for more than 5 minutes (a lost node)
leaves the directory to remote retention. Take a new backup to upload again.

Encryption is rclone's [crypt](https://rclone.org/crypt/) format: XSalsa20-Poly1305 per 64 KiB
block, with keys derived from the backup key by scrypt; every block is authenticated, so a
changed or truncated file, or a wrong key, fails to decrypt. File and directory names are not
encrypted, so backups can be listed (and pruned) without the key.

### The key

Without `encryption.keySecret`, the operator generates 32 random bytes once into Secret
`<name>-backup-key`, key `key`, as 43 characters of unpadded base64url (no `=`, no newline,
like the instance's other generated secrets), annotated `forgejo.lucas.icu/backup-key:
Generated`, and records a `BackupKeyGenerated` Warning event. The operator never changes or
deletes that Secret, and it has no owner reference, also under `retainPolicy: Delete`: deleting
the instance must not take the key of its off-site backups with it. With `keySecret`, bring your
own key (at least 16 characters).

**Copy the key out of the cluster right away and keep it offline,** then say so on the Secret.
A remote backup cannot be restored without the key, and the cluster (or namespace) that holds
the Secret is what the backups protect against losing:

```console
$ kubectl get secret forgejo-backup-key -o jsonpath='{.data.key}' | base64 -d > forgejo-backup-key.txt
$ kubectl annotate secret forgejo-backup-key forgejo.lucas.icu/backup-key-exported=true
```

Until the generated Secret carries that annotation, `status.backup.remote.message` says that the
key has not been exported, and `BackupHealthy` is `False` with reason `BackupKeyNotExported` (with
scheduled backups; only a failed backup reports something else). A key Secret you created
yourself (`keySecret`, or `<name>-backup-key` restored from your copy) needs no annotation.

**A lost key is never replaced silently.** When `<name>-backup-key` disappears after it encrypted
backups (its fingerprint is in `status.backup.remote.keyFingerprint`, which is kept, or in a
`ForgejoBackup`'s `status.remote`), the operator does not generate a new one: uploads fail,
`BackupHealthy` is `False/BackupKeyMissing`, and a `BackupKeyMissing` Warning event repeats
hourly. Restore the Secret from your copy. To start over with a new key instead (the backups
encrypted with the lost one then cannot be restored), annotate the Forgejo with the lost key's
fingerprint:

```console
$ kubectl annotate fj forgejo forgejo.lucas.icu/backup-key-lost=692717aef72a8682
```

`status.backup.remote.keyFingerprint` identifies the key (16 hex digits derived from the
scrypt key, so it does not help guessing the key), and each backup's
`status.remote.keyFingerprint` and manifest record the key it was encrypted with. Changing the
key affects new backups only: keep every key that encrypted a backup you may restore.

### Status, events, metrics and alerts

```console
$ kubectl get fjb
NAME                       FORGEJO   REASON      PHASE       REMOTE     COMPLETED   AGE
forgejo-scheduled-202610   forgejo   Scheduled   Completed   Uploaded   2m          3m
$ kubectl get fjb forgejo-scheduled-202610 -o jsonpath='{.status.remote}'
{"phase":"Uploaded","location":"s3://acme-forgejo-backups/forgejo/git/forgejo/forgejo-scheduled-202610-3f2a9c1e/",
 "path":"git/forgejo/forgejo-scheduled-202610-3f2a9c1e","s3":{"endpoint":"https://s3.eu-central-1.amazonaws.com",...},
 "keySecret":{"name":"forgejo-backup-key","key":"key"},"keyFingerprint":"692717aef72a8682",
 "files":[{"name":"db.dump","sizeBytes":415604,"sha256":"10df..."},...],"verified":true,...}
```

* `status.backup.remote` of the `Forgejo`: `location` (the instance's directory),
  `keySecretName`, `keyFingerprint`, `lastUploadedBackupName`, `lastUploadTime`,
  `lastRetentionTime`, and `message` when the key is unusable or remote retention failed.
* `BackupHealthy` is `False`/`UploadFailed` when the latest scheduled backup was not uploaded,
  and `False`/`BackupKeyNotExported`, `BackupKeyMissing` or `BackupKeyUnusable` while the key is
  not exported, missing or unusable (see [The key](#the-key)).
* Events: `UploadStarted`, `BackupUploaded`, `UploadFailed` on the backup;
  `BackupUploaded`, `BackupUploadFailed`, `RemoteBackupPruned`, `RemoteRetentionFailed`,
  `BackupKeyGenerated`, `BackupKeyMissing` on the `Forgejo`.
* Metrics: `forgejo_operator_backup_uploads_total{reason,result}` and
  `forgejo_operator_backup_last_upload_success_timestamp_seconds`.
* Alerts: `ForgejoBackupUploadFailed` (an upload failed in the last hour) and, with scheduled
  backups, `ForgejoBackupUploadTooOld` and `ForgejoBackupUploadMissing`: the off-cluster copy
  is the recovery point of a disaster recovery, and it has the same recovery point objective
  (two schedule intervals) as the local backups (see
  [observability.md](observability.md#recovery-point-objective)).

### Remote retention

While scheduled backups are enabled, the operator keeps the newest `spec.backup.retention`
complete scheduled backups of the instance in the bucket and deletes older ones (manifest
first). It lists `<prefix>/<namespace>/<name>/` and reads the manifests, so it also prunes
backups whose `ForgejoBackup` is gone. A backup counts as the instance's when its manifest names
the instance's UID, or, after `spec.restoreFrom`, the UID of the instance that took the restored
backup and it completed no later than that backup (the backups taken before the disaster;
`status.restoredFrom.instanceUID` and `backupCompletionTime`). It runs after every upload of a
scheduled backup and at least hourly. It never deletes:

* backups of another instance, e.g. of the same namespace and name in another cluster: a
  recovery drill and production, or the original cluster back after a recovery, each keep their
  own;
* manual and pre-upgrade backups;
* a backup whose manifest it cannot read (e.g. encrypted with another key);
* a backup an unfinished `ForgejoRestore` of any namespace reads, or the `spec.restoreFrom`
  source of any `Forgejo` in the cluster that is being created from it (also before its
  `<name>-restore-from` exists: while it restores the identity and waits for its database). A
  source counts when it names the same bucket and prefix, whatever its endpoint is called (case,
  port, scheme, a short Service name): a false match only keeps a backup longer;
* an incomplete directory (no manifest) younger than 25 hours; older ones are failed uploads
  and are deleted.

Remote retention runs in the operator: it needs to reach the endpoint (see
[design.md](design.md#network-requirements)). A failure is reported in
`status.backup.remote.message` and a `RemoteRetentionFailed` event, and never holds up local
backups.

### Reading a remote backup by hand

Any rclone reads the backups (here with the key from the file you exported):

```console
$ export RCLONE_CONFIG_S3_TYPE=s3 RCLONE_CONFIG_S3_PROVIDER=Other RCLONE_CONFIG_S3_ENV_AUTH=true \
    RCLONE_CONFIG_S3_ENDPOINT=https://s3.eu-central-1.amazonaws.com RCLONE_CONFIG_S3_REGION=eu-central-1 \
    RCLONE_CONFIG_ENC_TYPE=crypt RCLONE_CONFIG_ENC_REMOTE=s3:acme-forgejo-backups/forgejo \
    RCLONE_CONFIG_ENC_FILENAME_ENCRYPTION=off RCLONE_CONFIG_ENC_DIRECTORY_NAME_ENCRYPTION=false \
    AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
$ export RCLONE_CONFIG_ENC_PASSWORD=$(rclone obscure - < forgejo-backup-key.txt)
$ rclone lsf enc:git/forgejo/                         # the backups of instance git/forgejo: <backup>-<uid>/
$ rclone cat enc:git/forgejo/<backup>-<uid>/manifest.json
$ rclone copy enc:git/forgejo/<backup>-<uid> ./<backup>     # db.dump, data.tar.gz, identity.json
```

## Restoring a remote backup

A `ForgejoRestore` with `spec.remote` instead of `spec.backupName` restores a remote backup by
its location; no `ForgejoBackup` object is needed:

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: ForgejoRestore
metadata:
  name: from-remote
spec:
  forgejoRef: {name: forgejo}
  remote:
    s3: {...}                     # the spec.backup.remote.s3 that uploaded it (same prefix)
    path: git/forgejo/forgejo-scheduled-20261022030000-3f2a9c1e   # <namespace>/<name>/<backup>-<uid>: status.remote.path
    # keySecret: {name: old-key}  # default: the instance's backup key
  restoreDatabase: true
  restoreData: true
```

* In `Pending` the operator reads `manifest.json` with its own client: the backup must be
  complete and decrypt with the key, otherwise the restore fails (a bucket it cannot reach is
  retried for 5 minutes). `status.remote` records the backup's name, image, whether it has a
  data archive, and the files with their checksums. A `NewerBackup` event warns about a newer
  image, and an `IdentityMismatch` event when the backup was taken by an instance with another
  `SECRET_KEY` (its two-factor secrets, Actions secrets and mirror credentials would not
  decrypt; to bring that instance back, use `spec.restoreFrom`).
* The restore Job gets a first init container, `download` (rclone), which downloads and
  decrypts `db.dump` and `data.tar.gz` into an `emptyDir` and checks their SHA-256 against the
  manifest. The pod requests ephemeral storage for them (`sizeLimit` of the `emptyDir` too),
  so the node of the data volume needs that much free space. The other steps and the
  maintenance handshake are those of any restore; nothing is changed before `verify` passed.
* The instance's Secrets are not changed.

## Disaster recovery

`spec.restoreFrom` creates an instance from a remote backup, in any namespace or cluster:
before Forgejo starts for the first time, the operator restores the identity Secrets, then the
database and the data volume. Only the bucket's credentials and the backup key are needed.

### Before a disaster

1. Configure `spec.backup.remote` (and scheduled backups).
2. Export the backup key (see [The key](#the-key)) and keep it offline, with the S3 credentials.
3. Keep the `Forgejo` manifest (e.g. in Git), without `spec.restoreFrom`. The last uploaded
   spec is also in every backup's `identity.json`.
4. Protect the object storage bucket with versioning or replication.

### Recovering

```console
$ kubectl create namespace git
$ kubectl -n git create secret generic backup-s3 --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
$ kubectl -n git create secret generic forgejo-backup-key --from-file=key=forgejo-backup-key.txt
$ rclone lsf enc:git/forgejo/        # pick a backup (see "Reading a remote backup by hand")
```

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata:
  name: forgejo                   # any name; the identity Secrets get this instance's names
spec:
  image: codeberg.org/forgejo/forgejo:17      # the backup's version or newer
  objectStorage: {...}            # the same bucket and prefix as before
  backup:
    enabled: true
    remote:
      s3: {...}                   # the same bucket; this instance uploads to <namespace>/<name>/
  restoreFrom:
    remote:
      s3: {...}                   # the bucket the backup is in, with its prefix
      path: git/forgejo/forgejo-scheduled-20261022030000-3f2a9c1e
      # keySecret: {name: forgejo-backup-key}   # default: the instance's backup key Secret
```

What the operator does (`status.restoredFrom`, condition `RestoredFromBackup`):

1. **Maintenance from the first pass.** The instance is in maintenance (`phase: Restoring`, no
   Forgejo pod) until the backup is restored, and no backup is taken.
2. **Identity** (`restoredFrom.phase: RestoringIdentity`), step `restore-identity`, before
   the `secrets` step would generate new values: the operator reads `manifest.json` and
   `identity.json` with its own S3 client, checks the identity's SHA-256 against the manifest,
   and creates `<name>-secrets`, `<name>-admin` (unless `admin.existingSecret` is used) and
   `<name>-ssh-host-keys` (unless `hostKeys.existingSecret` is used) with the backup's values,
   under the new instance's names and following its `retainPolicy`. A Secret that exists already
   is kept when it holds the backup's values; otherwise this is a conflict
   (`RestoredFromBackup=False/IdentityConflict`): the operator never overwrites a Secret. While
   the backup cannot be read (missing Secrets, wrong key, unreachable bucket), it retries every
   30 s with `RestoredFromBackup=False/RemoteBackupUnavailable`, and nothing else is created.
   When the bucket is unreachable or answers with a server error, the retries back off from
   30 s, doubling up to 5 minutes, and the condition keeps the last error meanwhile; changing
   `spec.restoreFrom.remote` reads at once.
3. **Database and data** (`RestoringData`): once the CNPG cluster is ready and the data volume
   exists, the operator creates `ForgejoRestore` `<name>-restore-from` (owned by the instance)
   for the backup, which restores it as described in
   [Restoring a remote backup](#restoring-a-remote-backup). It refuses to when the CNPG cluster
   `<name>-db` or the data PVC `<name>-data` is older than the `Forgejo` (kept by `retainPolicy:
   Retain` from an earlier instance of the same name, so it may hold data the restore would
   replace), or when `<name>-secrets` records the restore of another backup:
   `restoredFrom.phase` is `Failed`, `RestoredFromBackup=False/TargetNotEmpty`, and Forgejo stays
   scaled down (phase `Degraded`, as for any failed bootstrap). Remove `spec.restoreFrom` to start the instance on its data, or annotate the
   Forgejo with `forgejo.lucas.icu/restore-from-overwrite=true` to restore the backup over
   it.
4. **Start** (`Completed`): once the restore has restored everything (`ScalingUp`), the operator
   writes the backup's location into the annotation `forgejo.lucas.icu/restored-from` of
   `<name>-secrets`, and only then Forgejo starts on the restored database (the first pod
   migrates it alone when the image is newer), with the restored `SECRET_KEY`, admin password
   and SSH host keys. `RestoredFromBackup=True/Restored`. Remote retention takes over the
   scheduled backups the lost instance took up to the restored one (see
   [Remote retention](#remote-retention)).

Remote retention of the original instance protects the backup while the recovery runs only
when both run in the same cluster (see [Remote retention](#remote-retention)). A recovery into
another cluster, while the original instance still uploads, cannot be protected: pick a backup
that is not the oldest one kept (a newer upload would then prune it mid-recovery), or rely on
the bucket's versioning (see [Before a disaster](#before-a-disaster)).

If the `ForgejoRestore` fails, `restoredFrom.phase` is `Failed` and Forgejo stays scaled down;
the phase is then `Degraded` instead of `Restoring`, so `ForgejoDown` and `ForgejoNotReady` alert
and the availability SLI counts the hold as unplanned.
Delete `<name>-restore-from` to try again (the operator creates it anew), or remove
`spec.restoreFrom` to start the instance without the backup. `spec.restoreFrom` can only be set
when the `Forgejo` is created. **Remove it once `restoredFrom.phase` is `Completed`**, also from
the manifest you keep. A backup is never restored twice: the annotation on `<name>-secrets`
survives a lost status and, with `retainPolicy: Retain`, a deleted and re-created `Forgejo`, which
then completes at once (`RestoredFromBackup=True`, the message names the annotation); with
`retainPolicy: Delete`, deleting the `Forgejo` deletes its data too, and a re-created one with
`spec.restoreFrom` is a new recovery.

After the recovery: users sign in with their passwords and two-factor codes, the admin password
is the one of the backup's admin Secret (or the one changed in the UI before the backup), clients
see the same SSH host key fingerprints, and LFS objects and attachments come from the bucket of
`spec.objectStorage`. Re-create what lives outside the operator's Secrets yourself: Secrets of
`configFrom`, `admin.existingSecret`, `hostKeys.existingSecret`, and the CNPG database password
(the new cluster has its own).

## Restoring files by hand

To restore backup files that neither a `ForgejoBackup` nor a remote backup describes (for
example copied off the backup PVC, or downloaded with rclone as above), do by hand what the
restore Job does. First scale Forgejo to zero
(`spec.replicas: 0`). Then run the following in pods that mount the data PVC (as root) and
reach the database with the app user's credentials:

```console
$ psql -v ON_ERROR_STOP=1 -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION CURRENT_USER'
$ pg_restore --no-owner --no-acl --single-transaction --exit-on-error -d "$PGDATABASE" db.dump
$ find /data -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
$ tar -xzf data.tar.gz -C /data && chown -R 1000:1000 /data
```

Remote backups are logical dumps taken on a schedule. For a recovery point closer than the
last backup, also consider continuous archiving and point-in-time recovery of PostgreSQL with
CloudNativePG's own backup support (the barman-cloud plugin can use the same bucket under
another prefix). Use storage snapshots for very large data volumes.
