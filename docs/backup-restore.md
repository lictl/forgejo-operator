# Backup and restore

The operator backs Forgejo up as two artifacts:

1. A **PostgreSQL logical dump** (`pg_dump --format=custom`) of the Forgejo database.
2. A **gzip-compressed tar archive of the data volume**: repositories, LFS objects,
   attachments, avatars, packages and the JWT signing key.

It also keeps a copy of the SSH host keys from their Secret (see [ssh.md](ssh.md)).

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
`Ready` condition. Events are recorded on the backup and on the `Forgejo` object. Status is
the record of a finished backup: the Job is deleted once its outcome is written there (see
[Jobs are deleted when they finish](#jobs-are-deleted-when-they-finish)).

What happens:

* The backup waits while a `ForgejoRestore` of the same instance is in progress. It fails
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
  already grown by hand, is ignored. An `existingClaim` is never resized. Plan for about
  `(retention + 1) ×` the size of one backup, plus manual and pre-upgrade backups, which
  retention does not prune.
* A Job `<backup>-backup` runs one pod:
  * Init container `dump-database` uses the instance's PostgreSQL image
    (`spec.database.cnpg.imageName`, also for external databases). It connects with
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
                        # timestamps, sizes, excluded paths, tar warnings, host key files
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
  `spec.highAvailability.enabled`): disable horizontal autoscaling as well. Alternatively,
  take CSI `VolumeSnapshot`s of the data volume, which are crash-consistent.
* Forgejo's own `forgejo dump` is not used. Its database export has known long-standing
  bugs, and it produces a single archive that cannot be restored selectively.

### Deleting a backup

`ForgejoBackup` objects carry the finalizer `forgejo.lucas.icu/finalizer`. Deleting
one stops its Job if it is still running, then runs a cleanup Job `<backup>-cleanup` that
removes `<pvc>/<backup-name>/`. If the files of a failed backup were already removed
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
  pruned, and neither is a backup that an unfinished restore is reading. Pruning deletes the `ForgejoBackup`, and
  the finalizer then removes its files. Independently of retention, the files of every
  failed backup are removed right after it fails (see [Files](#files)); its object stays.
* A tick that falls while the instance is being upgraded (or while a failed upgrade still
  holds its new image's migration) is deferred until the upgrade has settled, so a dump is
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
    than two scheduled runs.
  * `False`/`BackupFailed`: the latest scheduled backup failed.
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
| `Pending` | Validates that the backup exists, is `Completed` and belongs to the same instance. Fails otherwise. Waits for another restore of the instance to finish, and while the Secret of SSH host keys is missing or unusable (`status.ssh.message` of the instance; Forgejo could not start again, see [ssh.md](ssh.md#when-something-is-wrong)). |
| `ScalingDown` | The Forgejo pipeline turns on maintenance mode: `status.backup.activeRestore` is set, the Deployment is scaled to 0, the phase becomes `Restoring` and health checks and autopilot actions are suspended. The restore waits until no Forgejo pod exists, no backup of the instance is running and no Job or pod of another restore of the instance is still running. It fails after 15 minutes if that does not happen, for example because `spec.paused` is set or the reconcile is blocked earlier. |
| `Restoring` | Job `<restore>-restore` runs (see below); `status.jobStartTime` records when it was created. If its newest pod cannot start within 10 minutes, the Job is stopped and the restore fails. It reports "nothing was changed" only when no earlier pod got past `verify`. Otherwise the failure counts as one that may have changed the instance (see below). A Job that is not found a minute after `jobStartTime` fails the restore with `JobMissing` only after the operator has confirmed with the API server that it is gone. |
| `ScalingUp` | Maintenance ends and Forgejo is scaled back up. The bootstrap init container runs `forgejo migrate` against the restored database, in a single pod until that pod is available. The restore waits up to 15 minutes for the Deployment to be available. It completes right away when the instance wants no pods (`spec.replicas: 0` without horizontal autoscaling; with an HPA, `minReplicas` applies). |
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
and a `MaintenanceHeld` event say so. The data volume may be empty or half-extracted, and
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
* Excluded content (search indexes, queues, caches) is rebuilt after the restore.
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

### Restoring into a new instance (disaster recovery)

A backup belongs to the instance named in its `spec.forgejoRef`. To recover after the
instance was deleted (`retainPolicy: Retain` keeps the backup PVC and the `ForgejoBackup`
objects, also when it was switched from `Delete` shortly before):

1. Recreate the `Forgejo` with the same name. It reuses `<name>-backups`, and the retained
   Secrets `<name>-secrets` and `<name>-ssh-host-keys`, so it keeps its SSH host key
   fingerprints.
2. Wait until it is `Running`.
3. Create a `ForgejoRestore` for the backup.

A restore never changes the SSH host keys: they belong to the instance, not to the backup. When
the instance is recreated in another cluster, create `<name>-ssh-host-keys` from the backup's
`ssh-host-keys/` before the `Forgejo` (see [ssh.md](ssh.md#backups-and-disaster-recovery)).

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
      image: codeberg.org/forgejo/forgejo:15
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
