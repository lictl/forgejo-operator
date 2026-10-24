# forgejo-operator design

A Kubernetes operator for [Forgejo](https://forgejo.org) that targets **Operator
Capability Level V (Auto Pilot)**. It runs the official image
`codeberg.org/forgejo/forgejo:<version>` (or its `-rootless` flavor) unchanged and uses
[CloudNativePG](https://cloudnative-pg.io) (CNPG) for PostgreSQL. Feature details live in
[versions.md](versions.md), [backup-restore.md](backup-restore.md), [upgrades.md](upgrades.md),
[autopilot.md](autopilot.md), [observability.md](observability.md), [networking.md](networking.md),
[ssh.md](ssh.md), [object-storage.md](object-storage.md), [actions-runners.md](actions-runners.md),
[high-availability.md](high-availability.md) and [permissions.md](permissions.md).

## APIs (`forgejo.lucas.icu/v1alpha1`)

| Kind | Purpose |
|------|---------|
| `Forgejo` (`fj`) | One Forgejo instance: image, DB, storage (volume and S3 object storage), networking (Services, Ingress, Gateway API routes, cert-manager TLS), backup schedule, upgrade policy, monitoring, HA (Valkey with Sentinel, a single cron runner), autoscaling, autopilot. Has a `scale` subresource. |
| `ForgejoBackup` (`fjb`) | Immutable one-shot backup: `pg_dump` (custom format) + tar.gz of the data volume, written to the `<name>-backups` PVC, then (with `spec.backup.remote`) uploaded encrypted with the instance's identity to S3. Reasons: Manual, Scheduled, PreUpgrade. |
| `ForgejoRestore` (`fjr`) | Restores a completed backup, or a remote backup by its location, into an instance, which is in maintenance (scaled to zero) meanwhile. |
| `ForgejoRunner` (`fjrn`) | A set of Forgejo Actions runners for an instance in the same namespace: a StatefulSet of Forgejo Runner pods (Docker executor with a privileged dind sidecar, or Host executor), each runner registered under its own name and unregistered when scaled away or deleted. Has a `scale` subresource. |

See `api/v1alpha1/*_types.go` for every field and default. `ForgejoBackup` and
`ForgejoRestore` have their own controllers (`internal/controller/forgejobackup_controller.go`,
`forgejobackup_upload.go`, `forgejorestore_controller.go`, backed by `internal/backup`); their
Jobs are deleted once the result is recorded in status. So has `ForgejoRunner`
(`internal/controller/forgejorunner_controller.go`, backed by `internal/runner`; see
[ForgejoRunner](#forgejorunner)).

### Validation

Mistakes that the operator could only report later (as `Degraded`, a failed child object or a
setting that is silently ignored) are refused by the API server, through OpenAPI markers and CEL
rules (`+kubebuilder:validation:XValidation`) in `api/v1alpha1`. `api/v1alpha1/validation_test.go`
runs the generated CRDs through the API server's own CRD validation (every rule compiles within
the CEL cost budget), defaulting, schema and CEL validation, without a cluster, against the samples
and the mistakes below. The operator's runtime checks stay as a second line.

| Kind | Refused |
|------|---------|
| `Forgejo` | a name that is not a DNS-1035 label, longer than 56 (Service `<name>-valkey`), with CNPG longer than 47 (CNPG limits Cluster `<name>-db` to 50), or with `highAvailability.enabled` longer than 45 (pod label `controller-revision-hash=<name>-valkey-<hash>`); a name ending in `-valkey` (its PDB `<name>` would be the Valkey PDB of another instance); an empty `image`, `database.cnpg.imageName` or `highAvailability.valkey.image` (it would follow the operator's default); `replicas` > 1 or `autoscaling.horizontal.enabled` without `highAvailability.enabled` (also through the scale subresource); `minReplicas` > `maxReplicas`; type `External` without `external`, `external` without type `External`, and CNPG settings with `External` (except `imageName`, used by backup Jobs); `ingress.enabled` without a host (`ingress.host`, `server.domain`, `server.rootURL` or `gateway.hostnames`); Ingress and Gateway paths that Kubernetes or `HTTPPathMatch` refuse; `gateway.ssh` with SSH disabled; an issuer kind other than `Issuer`/`ClusterIssuer` for cert-manager's group; `nodePort` or `loadBalancerClass` on a Service type that ignores it; a `domain` or `rootURL` that is no host name or http(s) URL; an admin user name Forgejo refuses or reserves (e.g. `admin`); a `backup.schedule` that is no five-field cron expression or descriptor; `restoreFrom` set or changed after creation |
| `ForgejoBackup` | any change of `spec` |
| `ForgejoRestore` | any change of `spec`; neither or both of `backupName` and `remote`; `restoreDatabase` and `restoreData` both false |
| `ForgejoRunner` | a name longer than 52 (pod label `controller-revision-hash=<name>-<hash>`), with a `.` (pod host names), or ending in `-valkey` or `-cron` (its StatefulSet `<name>` would be an HA instance's Valkey or cron runner); a missing `executor`; a changed `forgejoRef`; both `scope.organization` and `scope.repository`; labels of the other executor's form; `docker` settings with `Host` and `host.image` with `Docker` |

`forgejoRef.name` of the other kinds must be a possible `Forgejo` name. Ports, percentages,
durations and pull policies have their ranges, forms and enums.

## Remote backups and disaster recovery

`internal/backup/remote*.go` and `bootstrap.go` (user documentation in
[backup-restore.md](backup-restore.md#remote-backups)):

- **Jobs move the data, the operator reads metadata.** Upload Job `<backup>-upload` and the
  `download` init container of a remote restore run a stock rclone (`spec.backup.remote.image`)
  configured through environment variables only: the S3 key and the backup key are
  `secretKeyRef`s, and the backup key reaches rclone's crypt remote through `rclone obscure -` on
  stdin. The operator reads the small files of a remote backup (`manifest.json`,
  `identity.json`) itself, with its SigV4 client (`internal/s3`) and an implementation of
  rclone's crypt file format (`internal/rclone`, checked against files rclone wrote), so the
  disaster recovery bootstrap, the validation of remote restores and remote retention need no
  Job.
- **Layout and completeness.** `s3://<bucket>/<prefix>/<namespace>/<name>/<backup>-<uid>/`
  (`<uid>`: the first 8 characters of the `ForgejoBackup`'s UID, so every `ForgejoBackup` writes
  to a directory of its own, also one created again under an old name or one of an instance of
  the same namespace and name in another cluster), file names not encrypted (`.bin` appended),
  `manifest.json` (with the UIDs of the backup and of its instance) uploaded last after every
  file was read back and its SHA-256 compared; a directory without a manifest is no backup. The
  upload Job never writes into a directory that holds a manifest.
- **Failed uploads.** `status.remote` records the connection and key the Job writes with. An
  upload whose Job failed or vanished is settled once its pods have stopped: its own complete
  manifest in the directory means only the Job's result was lost (`Uploaded`); objects without a
  manifest are removed; a manifest the operator cannot read or another backup's is left alone.
- **Ownership.** Remote retention prunes only scheduled backups whose manifest names the
  instance's UID, or the UID of the instance `spec.restoreFrom` recreated, up to the restored
  backup (`status.restoredFrom.instanceUID`, `backupCompletionTime`): two instances of the same
  namespace and name writing to the same bucket (a recovery drill, the original cluster back
  after a recovery) never prune each other's backups.
- **Identity.** The backup controller builds the identity bundle (the instance's Secrets and
  sanitised spec) in Go and hands it to the upload Job in Secret `<backup>-identity`, which it
  deletes with the Job.
- **Key management.** `<name>-backup-key` is generated once by the `secrets` step, carries no
  owner reference under any `retainPolicy`, and is never changed; an existing one (e.g. restored
  from an offline copy) is used as it is. Until the generated Secret is annotated
  `forgejo.lucas.icu/backup-key-exported=true`, `BackupHealthy` is
  `False/BackupKeyNotExported` and `status.backup.remote.message` says to export it. A key whose
  fingerprint status or a `ForgejoBackup` records is not generated again when its Secret goes
  missing (`BackupKeyMissing`) unless the Forgejo carries
  `forgejo.lucas.icu/backup-key-lost=<that fingerprint>`. Only fingerprints of the key and
  of `SECRET_KEY` appear in status and manifests.
- **Bootstrap.** With `spec.restoreFrom`, `maintenance` holds the instance at zero replicas
  until the restore is recorded, `restore-identity` creates the identity Secrets before anything
  generates them, and `restore-from` creates `ForgejoRestore <name>-restore-from`, which
  restores the database and the data volume through the normal restore handshake. It refuses
  (`RestoredFromBackup=False/TargetNotEmpty`) when the CNPG cluster or the data PVC is older than
  the `Forgejo`, or `<name>-secrets` records another restore, unless the Forgejo is annotated
  `forgejo.lucas.icu/restore-from-overwrite=true`. When the restore reaches `ScalingUp`,
  `restore-from` writes the location into the annotation `forgejo.lucas.icu/restored-from`
  of `<name>-secrets`, then sets `status.restoredFrom.phase=Completed`; `maintenance` lets
  Forgejo start only after that. The annotation outlives status (and, with `retainPolicy: Retain`,
  the `Forgejo`), so a lost status or a re-created instance completes the bootstrap from it
  instead of restoring the backup again.

## Reconcile pipeline

`internal/controller/forgejo_controller.go` runs a table of steps for each `Forgejo`. A step
is a `forgejo.Step` (`func(ctx, *forgejo.ReconcileContext) (StepResult, error)`) owned by one
feature package. Steps pass outputs through `ReconcileContext` (`internal/forgejo/context.go`);
status is changed in memory and patched once at the end, with an optimistic lock.

Each step lists the earlier steps it **needs**. It runs only when all of them *completed* in
this pass (returned neither an error nor `Stop: true`), otherwise it is skipped. A failure or
a wait therefore skips only the dependants; everything else still runs, so a rejected
Ingress, a Valkey outage or a database that cannot start does not also stop backups, health
checks, the restore handshake or volume expansion. Around the table:

- **Before:** `backup.ExportLastSuccess` re-exports the last-backup gauge (no API call), so
  it survives operator restarts and skipped steps.
- **Errors:** joined into `Degraded=ReconcileError` (one Warning event per failed step) and
  returned; the per-instance backoff is capped at the 60s resync, so time-based work keeps
  going. An optimistic-lock conflict ends the pass and retries after 1s.
- **Terminating namespace:** a create the API server refuses because the namespace is being
  deleted (`forgejo.IsNamespaceTerminating`) ends the pass without an error, event, status
  write or requeue, as kube-controller-manager's controllers do; the backup and restore
  controllers drop such a request too. Events the API server rejects for that reason are
  logged at V(1), not as errors (`forgejo.QuietTerminatingNamespaces` wraps the manager's
  logger, which the events.k8s.io broadcaster logs through).
- **Skipped steps:** when backups are enabled but `backup-schedule` did not complete,
  `BackupHealthy` becomes `Unknown/ReconcileBlocked` naming the root cause. When `upgrade` did
  not complete, `rc.Upgrading` is taken from `status.upgrade` (`forgejo.UpgradeInFlight`), so
  later steps never mistake an upgrade in flight for an idle instance.
- **After:** `core.ReconcileStatus` always runs: `phase`, `Ready`, `Progressing`, replicas,
  the cron runner (`status.highAvailability.cronRunner`), URLs, `status.image` (only once the Deployment has rolled the image out with every replica
  available), and the `instance_ready`/`instance_info` metrics. Then
  `upgrade.ReportVersionSupport` classifies the running version against the version table
  (`VersionSupported` condition, `forgejo_operator_version_supported`; see
  [versions.md](versions.md)). The instance is requeued at least every 60s.
- **Paused** (`spec.paused`): no step runs. Only `phase: Paused` and `Progressing=False` are
  written, and `instance_info{phase="Paused"}`, `instance_ready` and `version_supported` are
  still exported, as are planned availability samples (`observability.SampleWhilePaused`, from
  the pods' readiness), for which the instance is requeued every health-check interval.
- **Deleted:** the instance's metric series and in-memory state are dropped
  (`forgejo.DeleteInstanceMetrics`, `backup.ForgetLastSuccess`,
  `observability.ForgetInstance`). An instance deleted during a pass is not a reconcile error.

| # | Step | Package | Needs | Outputs |
|---|------|---------|-------|---------|
| 1 | maintenance | backup | – | `rc.Maintenance`, `rc.MaintenanceReason`, `rc.MaintenanceHeld`, `rc.RestoreMigrating`, `rc.RestoreScalingUp`; `status.backup.activeRestore`; completes `status.restoredFrom` from the `restored-from` annotation of `<name>-secrets` |
| 2 | restore-guard | controller | 1 | `rc.Maintenance` while a restore Job or pod still runs that no `ForgejoRestore` holds maintenance for |
| 3 | restore-identity | backup | 2 | with `spec.restoreFrom`, until done: Secrets `<name>-secrets`, `<name>-admin`, `<name>-ssh-host-keys` from the backup's `identity.json`; `status.restoredFrom`, `RestoredFromBackup`. Stops while the backup cannot be read or an existing Secret differs (see [backup-restore.md](backup-restore.md#disaster-recovery)) |
| 4 | secrets | core | 3 | Secrets `<name>-secrets` (app secrets, metrics token, admin sync salt) and `<name>-admin`; never rotated. With `spec.backup.remote` and no `keySecret`: `<name>-backup-key`, never regenerated over a lost key that encrypted backups |
| 5 | backup-ownership | backup | – | owner references of the instance's `ForgejoBackup`s and `<name>-backups` PVC, per `spec.storage.retainPolicy` |
| 6 | database | core | 4 | CNPG `Cluster <name>-db`, or the external DB check; `rc.DB`, `rc.DatabaseReady`, `status.database`, `DatabaseReady`. Stops until CNPG is ready or the external credentials resolve (an unreachable external DB is only reported) |
| 7 | high-availability | autopilot | 6 | Valkey password in `<name>-secrets`; StatefulSet, headless Service and PDB `<name>-valkey` (Valkey with Sentinel); `rc.ValkeyURL` (`redis+sentinel://`), `rc.NodeZones`. Empties Valkey (scaled to zero until no pod is left, then back to three) once for each restore in `ScalingUp` (`rc.ScalingUpRestore`) and when the password changes. Stops while no Valkey pod is Ready and while it empties Valkey, except in maintenance (see [high-availability.md](high-availability.md#valkey-with-sentinel)) |
| 8 | storage | core | 7 | PVC `<name>-data` (unless `spec.storage.existingClaim`) |
| 9 | restore-from | backup | 8 | with `spec.restoreFrom`: `ForgejoRestore <name>-restore-from` of the backup once the database is ready, unless the database or data volume predates the instance (`TargetNotEmpty`); `status.restoredFrom` follows it; at `ScalingUp` the annotation `restored-from` on `<name>-secrets`, then `Completed` |
| 10 | ssh-host-keys | core | 4 | Secret `<name>-ssh-host-keys` (generated once); `rc.SSHHostKeys`, `rc.SSHHostKeysUnavailable`, `status.ssh` (see [SSH host keys](#ssh-host-keys)) |
| 11 | object-storage | core | – | with `spec.objectStorage`: reads its Secrets, `rc.ObjectStorage` (digest of the credentials and CA), `rc.ObjectStorageUnavailable` (any failed answer of the endpoint, not `UnreachableFromOperator`), `ObjectStorageReady` from the operator's check of the bucket (see [object-storage.md](object-storage.md)). Stops only while the Secrets are unusable; a bucket the operator cannot reach is only reported |
| 12 | upgrade | upgrade | 8, 11 | `rc.TargetImage`, `rc.Layout`, `rc.Upgrading`, `rc.ReplicasOverride`, `status.upgrade`, `Upgrading`; pre-upgrade `ForgejoBackup`, rollback `ForgejoRestore` |
| 13 | tuning | autopilot | 12 | `rc.Tuning`, `rc.TuningEnv`, `rc.ResourcesOverride`, `rc.HPAManaged`, `status.autoPilot.tuning` |
| 14 | config | core | 10, 13 | Secret `<name>-config` (`app.ini`, including `SSH_SERVER_HOST_KEYS`, and with HA `app-web.ini`, the same without cron tasks; one hash covers both); `rc.ConfigSecretName`, `rc.ConfigHash`, `rc.DesiredConfigHash` |
| 15 | config-guard | autopilot | 14 | may switch `rc.ConfigSecretName`/`rc.ConfigHash` to `<name>-config-lkg` |
| 16 | workload | core | 15 | Deployment `<name>` and, with HA, the cron runner StatefulSet `<name>-cron` (see [Upgrade and migration holds](#upgrade-and-migration-holds)); `status.configHash` |
| 17 | network | core | – | Services `<name>-http`, `<name>-ssh`; Ingress `<name>` (TLS Secret defaults to `<name>-tls` with `spec.tls.certManager`) |
| 18 | gateway | core | – | HTTPRoute `<name>`, TCPRoute `<name>-ssh` (v1, else v1alpha2) when the Gateway API CRDs exist; `RoutesReady` from the routes' `status.parents` (see [networking.md](networking.md)) |
| 19 | tls | core | – | cert-manager Certificate `<name>-tls` (Secret `<name>-tls`), ReferenceGrant `<name>-tls` for `spec.tls.gatewayNamespaces`; `TLSReady` from the Certificate's `Ready` |
| 20 | scaling | autopilot | 13 | HPA `<name>`, PDB `<name>` |
| 21 | monitoring | observability | – | ServiceMonitor, PrometheusRule `<name>` (alerts with `runbook_url`, SLO recording rules and burn-rate alerts), ConfigMap `<name>-dashboard`; failures become `Degraded=MonitoringFailed`, not step errors |
| 22 | backup-schedule | backup | 2 | scheduled `ForgejoBackup`s, retention (also of remote copies, see [backup-restore.md](backup-restore.md#remote-retention)), backup PVC growth, `status.backup` (with `remote`), `BackupHealthy` (also `False` while the backup key is missing, unusable or not exported) |
| 23 | health | observability | 2 | `/api/healthz`, version and stats probes → `status.health`, `status.version`, `status.stats`, `Healthy`; one availability sample per interval (`forgejo_operator_availability_probes_total`), also while no pod is Ready, planned during maintenance (unless `rc.MaintenanceHeld`), `rc.RestoreScalingUp` and `rc.Upgrading` |
| 24 | valkey-status | autopilot | – | with HA: the Sentinels' view of Valkey → `status.highAvailability.valkey`, `ValkeyReady`, `forgejo_operator_valkey_*`, `ValkeyFailedOver` events (not for a new replication group, recognised by its pods' UIDs); asks at most every 10 s while the pods stay the same, every minute while unreachable; no prerequisites, so it stays current while the database is down |
| 25 | autopilot | autopilot | 16, 23 | self-healing restarts, config rollback, last-known-good bookkeeping, OOM- and usage-driven vertical sizing, `status.autoPilot` (the cron runner's pod counts like a server pod, except for the Deployment restart, which the server pods decide, and for the last known good config while it has not started a container for 5 minutes) |
| 26 | storage-autopilot | autopilot | – | volume measurement → `status.storage`, `StorageHealthy`, `volume_used_ratio`; grows `<name>-data` and the CNPG `storage`/`walStorage`; restarts Forgejo for a pending file system resize |

Steps 1–4, 6–8 and 12–16 form a chain, which `ssh-host-keys` (10) joins at `config`: like the
secrets it is identity, so it needs neither the database nor storage. It runs before `upgrade`,
which does not start an upgrade while the keys are unavailable. `restore-identity` (3) is a
no-op unless the instance is created from a remote backup (`spec.restoreFrom`): then it creates
the identity Secrets from the backup before `secrets` and `ssh-host-keys` would generate them,
and stops the chain while it cannot. `restore-from` (9) hands the database and the data volume to
a `ForgejoRestore` once both exist (see [backup-restore.md](backup-restore.md#disaster-recovery)).
`object-storage` (11) joins the chain at `upgrade`: pods reference its Secrets, so without them there is no upgrade, `app.ini`
or rollout. It needs nothing itself, so `ObjectStorageReady` stays current while the database is
down, and an upgrade does not start while the bucket is unusable
(`Upgrading=False/WaitingForObjectStorage`). `network`, `gateway` and `tls` render the spec only
and need nothing. `valkey-status` needs nothing: it reads the Valkey pods and asks their
Sentinels, so `ValkeyReady` stays current while the database step stops. `backup-ownership` and
`storage-autopilot` need nothing either:
ownership reads only the instance and its backups, and a full CNPG volume is a common reason
why `database` stops, so growing it must not depend on that step. `storage-autopilot` runs
last, so it sees this pass's maintenance and upgrade flags, and its restart comes after the
workload update.

## Maintenance and the restore handshake

1. A `ForgejoRestore` in `ScalingDown` or `Restoring` makes the `maintenance` step set
   `rc.Maintenance` and `status.backup.activeRestore=<restore>`. The workload goes to 0
   replicas, `phase` becomes `Restoring`, `Ready=False/Maintenance`, and health probing,
   self-healing and upgrades pause.
2. The restore controller starts Job `<restore>-restore` only after the persisted
   `status.backup.activeRestore` names it, the Deployment is at 0 (spec and status), no server
   or cron runner pod is left (nor a cron runner StatefulSet of the instance that still wants one), no backup runs, and no other restore's Job or pod is active. It gives up
   after 15 minutes.
3. When the Job succeeds, the restore moves to `ScalingUp` and maintenance ends. With HA the
   `high-availability` step first empties Valkey for it (its queues and cache are from after the
   backup point), holding the workload at zero meanwhile; a database-only restore's Job removes
   the LevelDB queues of the data volume. If it restored the database, `rc.RestoreMigrating`
   makes the first pod migrate it alone (see below). The restore waits up to 15 minutes for the
   Deployment to become available.
4. A failed restore that may have changed the instance (`DataModified` not `False`) keeps
   maintenance until a newer restore reaches `ScalingUp`, the annotation
   `forgejo.lucas.icu/release-maintenance=true` is set on it, or it is deleted. Such a
   hold, and a failed bootstrap of `spec.restoreFrom`, also set `rc.MaintenanceHeld`: `phase`
   is `Degraded` instead of `Restoring` (the outage alerts are not suppressed) and the
   availability samples are unplanned.
5. `restore-guard` keeps maintenance while any restore Job or pod of the instance still
   runs although no restore holds it, e.g. a Job orphaned by a deleted restore. Within 5
   minutes of a restore failing, it double-checks with a live read.

## Upgrade and migration holds

`forgejo migrate` (run by the `bootstrap` init container) takes no lock, so it must never
run in two pods at once. Three signals hold the workload at one pod with the `Recreate`
strategy and no HPA:

| Signal | Set when | Also |
|--------|----------|------|
| `rc.Upgrading` | the upgrade step runs a phase in flight (Pending, BackingUp, RollingOut, Verifying, RollingBack); or `forgejo.UpgradeInFlight(status.upgrade)` when that step did not complete | 0 replicas during the cold pre-upgrade backup; `phase: Upgrading`; scheduled backups are deferred; autopilot does not restart or resize; the upgrade pauses while a restore holds maintenance |
| `forgejo.UpgradeHoldsWorkload(f)` | a `Failed` upgrade left its new image deployed and that image was never verified | the upgrade step sets `rc.ReplicasOverride=1`; `phase: Degraded`; scheduled backups deferred; no autopilot restarts. Ends when the image becomes available (the upgrade turns `Succeeded`) or `spec.image` changes |
| `rc.RestoreMigrating` | a database restore is in `ScalingUp` | the Deployment carries `forgejo.lucas.icu/migration-hold` until a pod of its current template is available |

With HA, the cron runner `<name>-cron` (the one pod that runs Forgejo's cron tasks, see
[high-availability.md](high-availability.md#the-cron-runner)) is at zero replicas under each of
these signals, in maintenance, before the first rollout and while the server is scaled to zero,
so the migrating pod is the only Forgejo pod. Before the workload step rolls out a new image it
scales the cron runner to zero and waits until its pod has stopped (`WaitingForCronRunner`), and
`backup.ServerPodsRunning`, which restores and cold backups wait on, counts the cron runner's pods
(and a cron runner StatefulSet that still wants one). Every reader of `<name>-cron` goes through
`forgejo.CronRunner`, which ignores an object of that name that the instance does not control.

A fourth signal freezes the workload as it is instead: `rc.SSHHostKeysUnavailable`, while the
Secret the pods serve their SSH host keys from is missing or unusable (see
[SSH host keys](#ssh-host-keys)). A fifth one, `rc.ObjectStorageUnavailable` (the endpoint of
`spec.objectStorage` answered with a failure: `ObjectStorageReady=False`, but not
`UnreachableFromOperator`, which says nothing about the pods), only holds what replaces
pods: autopilot restarts, resizes and config rollbacks, and the start of an upgrade. Forgejo does
not start without its bucket, so a replacement pod would not come up, and a crash loop meanwhile
is not blamed on `app.ini` (see [object-storage.md](object-storage.md#status)).

## ForgejoRunner

`internal/controller/forgejorunner_controller.go` reconciles a `ForgejoRunner` in one pass with a
single optimistic-lock status patch (user documentation in [actions-runners.md](actions-runners.md)).
It does not join the `Forgejo` pipeline: it reads the instance (`status.image`, `Ready`, phase,
UID), and the instance's controller ignores the runner pods (`managedByOperator` skips the
`runner` and `runner-registration` components).

| Object | Content |
|--------|---------|
| Secret `<name>-tokens` | One runner secret per index (`token-<i>`, 40 hex), generated once; the UUID Forgejo gives the runner is derived from it (`runner.UUIDOf`). Kept for every wanted, recorded, registering or running index, dropped once a runner is unregistered |
| Secret `<name>-runner` | `config-<i>.yaml`: the operator's defaults, `spec.config` merged on top, then the keys it owns (`server.connections` with `token_url: file:`, labels, capacity, shutdown timeout). `<name>-runner` differs from every Secret of an instance, so a runner and an instance may share a name |
| Job `<name>-register` | `forgejo-cli actions register --secret-file --scope --name --keep-labels` for the runners that are not registered as wanted, with the instance's running image and `app.ini` Secret (read from its Deployment, so also the last-known-good copy), no data volume. Annotated with the scope and `<index>=<uuid>` pairs it registers; recorded into `status.runners` and deleted when it succeeds |
| StatefulSet `<name>` | Pods `<name>-<i>` (Parallel pod management) for the registered prefix of the wanted runners; pod `i` reads `config-$(POD_INDEX).yaml` (downward API, label `apps.kubernetes.io/pod-index`). Only the digest of the shared configuration is on the pod template, so scaling never rolls the other pods; a runner whose secret changed leaves the registered prefix (its pod is scaled away) until it is registered again |

- **Order.** Each pass: read the registration Job (live when the cache misses it) and the
  runner pods; ensure the two Secrets; collect a succeeded Job (its records are written to the
  status with the optimistic lock before the Job is deleted, so a conflict keeps the Job, the
  only trace of the registration, for the next pass); read Forgejo's runner list (IDs, online
  state, recorded runners Forgejo lost, which are flagged `missing` and registered again under
  the same UUID after a 2-minute grace while their pods keep running) and waiting jobs;
  unregister stale records (an index scaled away whose pod is gone, or a record whose UUID no
  longer matches its secret); start a registration Job when wanted runners are not registered as
  wanted and the instance is Ready and not paused, upgrading or restoring; update the
  StatefulSet; report `Ready`, `Registered`, `Degraded` and the metrics.
- **Forgejo's API** (`internal/runner/api.go`): `GET /api/v1/admin/actions/runners?visible=true`
  (all pages, by `X-Total-Count`), `GET` and `DELETE /api/v1/admin/actions/runners/{id}` and the
  waiting jobs of the scope, as the instance admin with basic authentication. It is only needed
  for unregistering and the online state; without it `Degraded=ForgejoAPIUnavailable`. The list
  is ordered by `last_online` (no other order is offered) and runners move between pages while it
  is read, so a read whose distinct runners do not add up to `X-Total-Count` is repeated, a list
  of several pages is read twice and merged, a recorded runner the list lacks is asked for by its
  id (only a 404 means gone), and runners are deleted by id (a 404 is "gone already").
- **Stale cache.** After its own status write the controller reads the `ForgejoRunner` live until
  the cache has that write (as the `Forgejo` controller does), so a pass right after a
  registration does not register the same runners again.
- **Deletion.** The finalizer deletes the StatefulSet and the registration Job, waits for their
  pods (grace period plus 2 minutes at most), then unregisters every runner whose UUID is recorded
  or derived from a secret of the `ForgejoRunner` (recorded ones by id; the others from the list,
  read again until it shows none of them); 5 minutes after the first failed attempt (kept in
  memory: an operator restart grants another 5 minutes) it gives up with a Warning event. Nothing is unregistered when the instance is gone or another instance (UID) holds
  the name.
- **Watches.** The `ForgejoRunner` (generation, labels, annotations, deletion), its StatefulSet,
  Job and Secrets, runner pods (a stopped pod lets its runner be unregistered), and `Forgejo`
  objects (generation, `status.image`, phase, `Ready`, deletion). Requeued every 60 s (Forgejo's
  runner list), every 10 s while registering, unregistering or rolling out.

## Informer cache and reads

The manager caches Secrets and ConfigMaps only with `app.kubernetes.io/managed-by=forgejo-operator`,
and Pods, Jobs, Deployments, StatefulSets and Services only with `app.kubernetes.io/name=forgejo`
(`forgejo.CacheScopes`, applied in `cmd/main.go`). Everything the operator creates carries
`forgejo.Labels`. `managedFields` are stripped from cached objects. Without this the operator
would hold every Secret (Helm releases included), ConfigMap and Pod of the cluster in memory.

The manager's client wraps the cache (`forgejo.NewScopedReader`) so that callers need not
know about the selectors:

- A `Get` that misses the cache is confirmed with a live read. This covers user Secrets
  (`configFrom`, `admin.existingSecret`, external DB credentials), the CNPG `-app` Secret and
  CNPG pods.
- A `List` whose label selector does not imply the cache's selector is served live, e.g.
  CNPG pods by `cnpg.io/cluster` or Job pods by `job-name`. Frequent lists should include
  `app.kubernetes.io/name`.

`rc.APIReader` reads live where a stale cache would be unsafe. CNPG credential rotations are
watched through a separate metadata-only cache of Secrets labelled `cnpg.io/cluster`.
`--watch-namespace` / `WATCH_NAMESPACE` restricts all caches to the given namespaces; the overlay
`config/namespaced` also restricts the permissions to them (see [permissions.md](permissions.md)). The
manager runs with `GOMEMLIMIT` at ~90% of its memory limit (`config/manager/manager.yaml`).

## Forgejo versions

`forgejo.Versions` (`internal/forgejo/versions.go`) is the one table of Forgejo majors: LTS flag,
release and end-of-life dates, the oldest major each one is upgraded to from directly, patch
advisories, and the **traits** that make the operator run majors differently (e.g.
`InternalToken`, `InternalSocket`). `forgejo.LayoutForImage` puts the traits of the image's major
into `rc.Layout.Traits`; rendering code reads them and never compares major numbers. The
support policy (current LTS + latest release), the `VersionSupported` condition and the
upgrade-path rule of the version gate are derived from the same table. See
[versions.md](versions.md).

## Pod layout

The root image normally runs s6 as root (OpenSSH plus `forgejo web` as `git`), and the
`-rootless` image has another entrypoint. Both ship the binary at `/app/gitea/gitea`, so the
operator bypasses both entrypoints and runs Forgejo directly as uid 1000, with one layout
(`forgejo.LayoutForImage`):

| Item | Value |
|------|-------|
| Command | `/app/gitea/gitea web --config /etc/forgejo/app.ini` |
| User | uid/gid 1000 (`git` in both images), `RUN_USER=git` |
| Data PVC | `/data` (`WORK_PATH`=`/data/gitea`, custom `/data/gitea/custom`, repos `/data/git/repositories`, LFS `/data/git/lfs`, `HOME=/data/git`) |
| app.ini | Secret `<name>-config` (or `-config-lkg`), read-only at `/etc/forgejo` |
| HTTP | 3000. Startup probe `/api/healthz` every 10s for up to 1h (360 failures); readiness `/api/healthz`; liveness is a TCP check, so a PostgreSQL failover or Valkey blip restarts nothing |
| Termination | grace period 75s: `[server] GRACEFUL_HAMMER_TIME=50s` lets requests, git operations and SSH sessions finish after SIGTERM; with HA the server pods first sleep 10s in a preStop hook, so their endpoint is gone before Forgejo stops listening |
| Cron runner (HA) | StatefulSet `<name>-cron` (pod `<name>-cron-0`, `Parallel`, so that a broken pod is still replaced on a template change): the same pod (labels `app.kubernetes.io/component: cron`, not selected by the Services), mounting the full `app.ini`; the server pods mount `app-web.ini` (no cron tasks) at the same path. The `--config` path must be the same in every pod: Forgejo writes it into the git hooks on the shared volume |
| Spreading (HA) | Without `spec.podTemplate.topologySpreadConstraints`: `ScheduleAnyway` constraints over hostname, and zone when the nodes have it ([high-availability.md](high-availability.md#spreading)) |
| SSH | built-in server (`START_SSH_SERVER=true`, `SSH_LISTEN_PORT=2222`), Service port 22 |
| SSH host keys | the pod's copy of Secret `<name>-ssh-host-keys` (or `spec.server.ssh.hostKeys.existingSecret`): emptyDir `ssh-host-keys-copy` (memory), read-only at `/etc/forgejo-ssh` in the `forgejo` container, listed in `SSH_SERVER_HOST_KEYS`; see [SSH host keys](#ssh-host-keys) |
| Temp | emptyDir at `/tmp` |
| Object storage | with `spec.objectStorage`: `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` as `secretKeyRef`s in the `bootstrap` and `forgejo` containers (`app.ini` has no key), a digest of the key and CA on the pod template (`forgejo.lucas.icu/object-storage`), and a private CA read-only at `/etc/forgejo-s3-ca` with `SSL_CERT_DIR=/etc/ssl/certs:/etc/forgejo-s3-ca`; see [object-storage.md](object-storage.md) |

1. The operator renders the complete `app.ini`, including every secret and the DB
   credentials, so Forgejo never writes it. Its hash
   (`forgejo.lucas.icu/config-hash` on the pod template) rolls the pods on change.
2. Init container `ssh-host-keys` (uid 1000, only while the operator manages host keys) copies
   the keys from the Secret (mounted at `/etc/forgejo-ssh-secret`, in this container only) into
   the pod's copy, and fails unless they match the digest of the keys the operator checked.
3. Optional init container `init-permissions`, only with the annotation
   `forgejo.lucas.icu/fix-permissions: "true"` on the `Forgejo`. It runs as root with
   only `CHOWN`, `FOWNER` and `DAC_OVERRIDE`, creates the top-level data directories and
   chowns them to 1000:1000 (not recursively). It is for volumes that ignore `fsGroup` (NFS,
   hostPath) or hold another user's directories.
4. Init container `bootstrap` (uid 1000) creates the top-level directories and fails with a
   clear message when one is not writable. It then runs `forgejo migrate` and creates the
   admin from `<name>-admin` (or `admin.existingSecret`), resetting its password only when
   the Secret changed.
5. Main container `forgejo`.

Every container except the opt-in `init-permissions` runs with `runAsNonRoot`, no privilege
escalation, a read-only root file system and all capabilities dropped. The pod sets
`fsGroup: 1000` (`OnRootMismatch`) and the `RuntimeDefault` seccomp profile, and does not
mount a service account token. The default pod therefore passes the **restricted** Pod
Security Standard.

## SSH host keys

Forgejo's built-in SSH server loads the files in `[server] SSH_SERVER_HOST_KEYS`. By default it
generates an RSA key on the data volume (`/data/gitea/ssh/gitea.rsa`), which changes with a
rebuilt volume, so the operator keeps the keys in a Secret instead (`internal/core/sshhostkeys.go`;
user documentation in [ssh.md](ssh.md)):

- **Generated once.** Without a Secret, the `ssh-host-keys` step generates ed25519, ECDSA P-256
  and RSA 3072 keys (OpenSSH format) into `<name>-ssh-host-keys`, annotated
  `forgejo.lucas.icu/ssh-host-keys-source: Generated`. A `<name>-ssh-host-keys` that
  exists already (kept by `retainPolicy: Retain`, or created by the user) is used as it is.
- **Served from a copy per pod.** The step parses the keys (`golang.org/x/crypto/ssh`, as
  Forgejo's SSH server does) and puts a digest of their file names and contents on the pod
  template (annotation `forgejo.lucas.icu/ssh-host-keys`, which rolls the pods when the
  keys change, and env of the init container). Init container `ssh-host-keys` copies the files
  from the Secret into emptyDir `ssh-host-keys-copy` (medium `Memory`) and checks them against
  that digest; the `forgejo` container serves the copy (read-only at `/etc/forgejo-ssh`, listed
  in `SSH_SERVER_HOST_KEYS`). A Secret volume is updated in running pods, and Forgejo, finding
  no key it can load after a container restart, would serve a random in-memory key (the SSH
  library generates one); the copy keeps a pod on the keys it started with, and the check keeps
  new pods from starting with keys other than the published ones.
- **Never rotated.** The operator only creates the Secret. When a Secret the pods serve keys
  from is missing or unusable, the step hands the live template's keys on as `rc.SSHHostKeys`,
  `Degraded=SSHHostKeysUnavailable` says so, and no new keys are generated until the user
  restores or recreates the Secret.
- **Held workload.** New pods cannot start meanwhile, and with `Recreate` any rollout would
  take Forgejo down, so `rc.SSHHostKeysUnavailable` holds everything that replaces pods: the
  `config` step does not write `app.ini` (running pods read it at a container restart), the
  `workload` step leaves the Deployment as it is, autopilot does not restart, resize or roll
  back (`replacementsHeld`), an upgrade does not start (`Upgrading=False/WaitingForSSHHostKeys`),
  and a `ForgejoRestore` waits in `Pending` while `status.ssh.message` is set. The HPA is left
  alone: pods it adds do not start, the running ones keep serving.
- `spec.server.ssh.hostKeys.existingSecret` serves the user's Secret (keys
  `ssh_host_{ed25519,ecdsa,rsa}_key`). SSH disabled, or `SSH_SERVER_HOST_KEYS` in
  `spec.config`, leaves host keys alone and clears `status.ssh`.
- `<name>-ssh-host-keys` follows `retainPolicy` like `<name>-secrets`: it is identity, not
  cache. Backup Jobs copy the served Secret's keys into `<backup>/ssh-host-keys/`; restores do
  not change them.

## Cluster domain

In-cluster names (`forgejo.ServiceHost`: `<svc>.<ns>.svc.<domain>`) are used for the
`DOMAIN`/`SSH_DOMAIN`/`ROOT_URL` defaults, the CNPG host, the Valkey pods' names in the Valkey
URL and the operator's probes. `forgejo.ClusterDomain` comes from `--cluster-domain`, else `CLUSTER_DOMAIN`, else
the `svc.<domain>` search entry of the operator pod's `/etc/resolv.conf`, else
`cluster.local` (e.g. an operator running outside the cluster).

## Network requirements

Besides the Kubernetes API (including `pods/exec` where ClusterRole
`forgejo-operator-volume-usage` is bound, used to measure volumes, `metrics.k8s.io` for vertical
sizing, and `namespaces` get for the Pod Security level of a `ForgejoRunner`'s namespace; every
permission is listed in [permissions.md](permissions.md)), the operator's pod must reach:

- `<name>-http.<ns>.svc.<domain>:<spec.server.http.port>` (default 3000) for health, version
  and statistics probes. Self-healing depends on them. When NetworkPolicies or mesh mTLS
  shut the operator out, probes are counted as *unreachable*, not as failures. A `ForgejoRunner`
  uses the same Service for Forgejo's runner API (unregistering, online state).
- For an external database, its host and port (a TCP dial). Failing this sets
  `DatabaseReady=False/UnreachableFromOperator`, which pauses self-healing and scheduled
  backups, but Forgejo is still deployed.
- With `spec.backup.remote`, `spec.restoreFrom` or a `ForgejoRestore` of a remote backup, the
  backup bucket's endpoint: the operator reads manifests and identity bundles, lists and deletes
  remote backups (retention) and removes failed uploads itself. Without it, uploads still work
  (the Jobs reach the bucket from the instance's namespace), but remote retention fails
  (`status.backup.remote.message`), and restores of remote backups and `spec.restoreFrom` wait.
- With `spec.objectStorage`, its S3 endpoint (a signed listing of the bucket, 5 s timeout, every
  minute). Failing this sets `ObjectStorageReady=False/UnreachableFromOperator`, which is only
  reported: autopilot and upgrades go on, and Forgejo is deployed.

- With HA, the Valkey pods' Sentinel port 26379 (pod IPs of the pods StatefulSet `<name>-valkey`
  controls, `AUTH` with the generated password, read-only `SENTINEL GET-MASTER-ADDR-BY-NAME` and
  `SENTINEL REPLICAS`, 1s dial and 3s conversation timeout, at most 256 KiB read per connection,
  at most every 10 s per instance while its Valkey pods stay the same, every minute while none
  answers), to report
  `ValkeyReady`, the roles and failovers. Failing this sets `ValkeyReady=Unknown/
  UnreachableFromOperator` while Valkey pods are Ready, and is only reported.

The operator never connects to CNPG or to Valkey's data port itself. Forgejo pods and
backup/restore Job pods reach them in the instance's namespace.

## Naming, labels and ownership

All names come from `internal/forgejo/names.go`. Every child resource carries
`forgejo.Labels(instance, component)`, and Deployments and Services select with
`forgejo.SelectorLabels`. Data-bearing resources (data PVC, backup PVC, CNPG cluster,
scheduled and pre-upgrade backups) and the identity Secrets (`<name>-secrets`, `<name>-admin`,
`<name>-ssh-host-keys`) get an owner reference only with `spec.storage.retainPolicy: Delete`.
Manual backups never do, and neither does the backup encryption key `<name>-backup-key`.
The objects of a `ForgejoRunner` are named in `internal/runner/names.go`, carry
`runner.Labels` (instance label = the runner's name, component `runner` or
`runner-registration`, `forgejo.lucas.icu/instance` = the Forgejo,
`forgejo.lucas.icu/runner` = the runner) and are controlled by the `ForgejoRunner`.

## Optional integrations

CNPG, prometheus-operator (ServiceMonitor/PrometheusRule), metrics-server, Gateway API
(HTTPRoute, TCPRoute, ReferenceGrant) and cert-manager (Certificate) are optional.
Check `rc.HasAPI(gvk)` (or `rc.FirstAPI` for kinds served in several versions, such as TCPRoute
`v1`/`v1alpha2`) before touching their objects, and use `unstructured.Unstructured`
for their kinds, so the operator has no Go dependency on their modules. Their objects are written
for whoever may edit a `Forgejo`; what that delegates (notably Certificates for any DNS name from
any `ClusterIssuer`) is described in [networking.md](networking.md#permissions).

The Gateway API and cert-manager objects are rendered from small typed mirrors with the CRDs'
defaults filled in (`group`, `kind`, `weight`, the path match), so an unchanged object compares
equal to the stored one and is not updated on every pass. They are read live (unstructured reads
bypass the cache) and not watched: the steps re-read them on every pass, every 15 s while
`RoutesReady` or `TLSReady` is not True. An object the spec does not ask for is deleted only when
the instance controls it, and checked once per operator process (`optionalAbsent`), so instances
that do not use a feature cost no request per pass.

## Capability levels

| Level | Capability | How |
|-------|-----------|-----|
| I Basic install | Forgejo, CNPG database, secrets, admin, S3 object storage, Services, Ingress or Gateway API routes and cert-manager TLS from one CR; non-root pods that pass restricted Pod Security; Forgejo Actions runners registered and unregistered without exec (`ForgejoRunner`) | core, runner |
| II Seamless upgrades | Image change → version gate (no silent downgrade, supported upgrade paths) → cold pre-upgrade backup → single-pod `Recreate` rollout with `forgejo migrate` → verification; per-major rendering and support reporting from the version table | upgrade, forgejo |
| III Full lifecycle | Scheduled and manual backups with retention, encrypted off-cluster copies with the instance's identity and remote retention, restore with maintenance mode and migration hold (also of remote backups), disaster recovery into a new namespace or cluster (`spec.restoreFrom`), retain policy for data, CNPG-managed DB failover | backup, core |
| IV Deep insights | Operator metrics, Forgejo `/metrics` and ServiceMonitor, PrometheusRule alerts with runbooks, availability and recovery point SLOs with burn-rate alerts, Grafana dashboard, health and stats in status, events, conditions; runners online and waiting jobs per `ForgejoRunner` | observability, runner |
| V Auto pilot | Auto-rollback of failed upgrades, config rollback on crash loops, health-based restarts, OOM- and usage-driven vertical sizing, HPA with HA (Valkey with Sentinel failover, a single cron runner, spreading), PVC auto-expansion (also while the database is down), resource-derived tuning (GOMEMLIMIT, DB pool, workers) | autopilot, upgrade |
