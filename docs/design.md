# forgejo-operator design

A Kubernetes operator for [Forgejo](https://forgejo.org) that targets **Operator
Capability Level V (Auto Pilot)**. It runs the official image
`codeberg.org/forgejo/forgejo:<version>` (or its `-rootless` flavor) unchanged and uses
[CloudNativePG](https://cloudnative-pg.io) (CNPG) for PostgreSQL. Feature details live in
[versions.md](versions.md), [backup-restore.md](backup-restore.md), [upgrades.md](upgrades.md),
[autopilot.md](autopilot.md), [observability.md](observability.md), [networking.md](networking.md)
and [ssh.md](ssh.md).

## APIs (`forgejo.lucas.icu/v1alpha1`)

| Kind | Purpose |
|------|---------|
| `Forgejo` (`fj`) | One Forgejo instance: image, DB, storage, networking (Services, Ingress, Gateway API routes, cert-manager TLS), backup schedule, upgrade policy, monitoring, HA, autoscaling, autopilot. Has a `scale` subresource. |
| `ForgejoBackup` (`fjb`) | Immutable one-shot backup: `pg_dump` (custom format) + tar.gz of the data volume, written to the `<name>-backups` PVC. Reasons: Manual, Scheduled, PreUpgrade. |
| `ForgejoRestore` (`fjr`) | Restores a completed backup into an instance, which is in maintenance (scaled to zero) meanwhile. |

See `api/v1alpha1/*_types.go` for every field and default. `ForgejoBackup` and
`ForgejoRestore` have their own controllers (`internal/controller/forgejobackup_controller.go`,
`forgejorestore_controller.go`, backed by `internal/backup`); their Jobs are deleted once the
result is recorded in status.

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
  URLs, `status.image` (only once the Deployment has rolled the image out with every replica
  available), and the `instance_ready`/`instance_info` metrics. Then
  `upgrade.ReportVersionSupport` classifies the running version against the version table
  (`VersionSupported` condition, `forgejo_operator_version_supported`; see
  [versions.md](versions.md)). The instance is requeued at least every 60s.
- **Paused** (`spec.paused`): no step runs. Only `phase: Paused` and `Progressing=False` are
  written, and `instance_info{phase="Paused"}`, `instance_ready` and `version_supported` are
  still exported.
- **Deleted:** the instance's metric series and in-memory state are dropped
  (`forgejo.DeleteInstanceMetrics`, `backup.ForgetLastSuccess`,
  `observability.ForgetInstance`). An instance deleted during a pass is not a reconcile error.

| # | Step | Package | Needs | Outputs |
|---|------|---------|-------|---------|
| 1 | maintenance | backup | – | `rc.Maintenance`, `rc.MaintenanceReason`, `rc.RestoreMigrating`; `status.backup.activeRestore` |
| 2 | restore-guard | controller | 1 | `rc.Maintenance` while a restore Job or pod still runs that no `ForgejoRestore` holds maintenance for |
| 3 | secrets | core | 2 | Secrets `<name>-secrets` (app secrets, metrics token, admin sync salt) and `<name>-admin`; never rotated |
| 4 | backup-ownership | backup | – | owner references of the instance's `ForgejoBackup`s and `<name>-backups` PVC, per `spec.storage.retainPolicy` |
| 5 | database | core | 3 | CNPG `Cluster <name>-db`, or the external DB check; `rc.DB`, `rc.DatabaseReady`, `status.database`, `DatabaseReady`. Stops until CNPG is ready or the external credentials resolve (an unreachable external DB is only reported) |
| 6 | high-availability | autopilot | 5 | Valkey Secret, Deployment and Service `<name>-valkey`; `rc.ValkeyURL`. Stops while Valkey is not ready, except in maintenance |
| 7 | storage | core | 6 | PVC `<name>-data` (unless `spec.storage.existingClaim`) |
| 8 | ssh-host-keys | core | 3 | Secret `<name>-ssh-host-keys` (generated once); `rc.SSHHostKeys`, `rc.SSHHostKeysUnavailable`, `status.ssh` (see [SSH host keys](#ssh-host-keys)) |
| 9 | upgrade | upgrade | 7 | `rc.TargetImage`, `rc.Layout`, `rc.Upgrading`, `rc.ReplicasOverride`, `status.upgrade`, `Upgrading`; pre-upgrade `ForgejoBackup`, rollback `ForgejoRestore` |
| 10 | tuning | autopilot | 9 | `rc.Tuning`, `rc.TuningEnv`, `rc.ResourcesOverride`, `rc.HPAManaged`, `status.autoPilot.tuning` |
| 11 | config | core | 8, 10 | Secret `<name>-config` (`app.ini`, including `SSH_SERVER_HOST_KEYS`); `rc.ConfigSecretName`, `rc.ConfigHash`, `rc.DesiredConfigHash` |
| 12 | config-guard | autopilot | 11 | may switch `rc.ConfigSecretName`/`rc.ConfigHash` to `<name>-config-lkg` |
| 13 | workload | core | 12 | Deployment `<name>`; `status.configHash` |
| 14 | network | core | – | Services `<name>-http`, `<name>-ssh`; Ingress `<name>` (TLS Secret defaults to `<name>-tls` with `spec.tls.certManager`) |
| 15 | gateway | core | – | HTTPRoute `<name>`, TCPRoute `<name>-ssh` (v1, else v1alpha2) when the Gateway API CRDs exist; `RoutesReady` from the routes' `status.parents` (see [networking.md](networking.md)) |
| 16 | tls | core | – | cert-manager Certificate `<name>-tls` (Secret `<name>-tls`), ReferenceGrant `<name>-tls` for `spec.tls.gatewayNamespaces`; `TLSReady` from the Certificate's `Ready` |
| 17 | scaling | autopilot | 10 | HPA `<name>`, PDB `<name>` |
| 18 | monitoring | observability | – | ServiceMonitor, PrometheusRule `<name>`, ConfigMap `<name>-dashboard`; failures become `Degraded=MonitoringFailed`, not step errors |
| 19 | backup-schedule | backup | 2 | scheduled `ForgejoBackup`s, retention, backup PVC growth, `status.backup`, `BackupHealthy` |
| 20 | health | observability | 2 | `/api/healthz`, version and stats probes → `status.health`, `status.version`, `status.stats`, `Healthy` |
| 21 | autopilot | autopilot | 13, 20 | self-healing restarts, config rollback, last-known-good bookkeeping, OOM- and usage-driven vertical sizing, `status.autoPilot` |
| 22 | storage-autopilot | autopilot | – | volume measurement → `status.storage`, `StorageHealthy`, `volume_used_ratio`; grows `<name>-data` and the CNPG `storage`/`walStorage`; restarts Forgejo for a pending file system resize |

Steps 1–3, 5–7 and 9–13 form a chain, which `ssh-host-keys` (8) joins at `config`: like the
secrets it is identity, so it needs neither the database nor storage. It runs before `upgrade`,
which does not start an upgrade while the keys are unavailable. `network`, `gateway` and
`tls` render the spec only and need
nothing. `backup-ownership` and `storage-autopilot` need nothing either:
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
   pod is left, no backup runs, and no other restore's Job or pod is active. It gives up
   after 15 minutes.
3. When the Job succeeds, the restore moves to `ScalingUp` and maintenance ends. If it
   restored the database, `rc.RestoreMigrating` makes the first pod migrate it alone (see
   below). The restore waits up to 15 minutes for the Deployment to become available.
4. A failed restore that may have changed the instance (`DataModified` not `False`) keeps
   maintenance until a newer restore reaches `ScalingUp`, the annotation
   `forgejo.lucas.icu/release-maintenance=true` is set on it, or it is deleted.
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

A fourth signal freezes the workload as it is instead: `rc.SSHHostKeysUnavailable`, while the
Secret the pods serve their SSH host keys from is missing or unusable (see
[SSH host keys](#ssh-host-keys)).

## Informer cache and reads

The manager caches Secrets and ConfigMaps only with `app.kubernetes.io/managed-by=forgejo-operator`,
and Pods, Jobs, Deployments and Services only with `app.kubernetes.io/name=forgejo`
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
`--watch-namespace` / `WATCH_NAMESPACE` restricts all caches to the given namespaces. The
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
| SSH | built-in server (`START_SSH_SERVER=true`, `SSH_LISTEN_PORT=2222`), Service port 22 |
| SSH host keys | the pod's copy of Secret `<name>-ssh-host-keys` (or `spec.server.ssh.hostKeys.existingSecret`): emptyDir `ssh-host-keys-copy` (memory), read-only at `/etc/forgejo-ssh` in the `forgejo` container, listed in `SSH_SERVER_HOST_KEYS`; see [SSH host keys](#ssh-host-keys) |
| Temp | emptyDir at `/tmp` |

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
`DOMAIN`/`SSH_DOMAIN`/`ROOT_URL` defaults, the CNPG host, the Valkey URL and the operator's
probes. `forgejo.ClusterDomain` comes from `--cluster-domain`, else `CLUSTER_DOMAIN`, else
the `svc.<domain>` search entry of the operator pod's `/etc/resolv.conf`, else
`cluster.local` (e.g. an operator running outside the cluster).

## Network requirements

Besides the Kubernetes API (including `pods/exec`, used to measure volumes, and
`metrics.k8s.io` for vertical sizing), the operator's pod must reach:

- `<name>-http.<ns>.svc.<domain>:<spec.server.http.port>` (default 3000) for health, version
  and statistics probes. Self-healing depends on them. When NetworkPolicies or mesh mTLS
  shut the operator out, probes are counted as *unreachable*, not as failures.
- For an external database, its host and port (a TCP dial). Failing this sets
  `DatabaseReady=False/UnreachableFromOperator`, which pauses self-healing and scheduled
  backups, but Forgejo is still deployed.

The operator never connects to CNPG or Valkey itself. Forgejo pods and backup/restore Job
pods reach them in the instance's namespace.

## Naming, labels and ownership

All names come from `internal/forgejo/names.go`. Every child resource carries
`forgejo.Labels(instance, component)`, and Deployments and Services select with
`forgejo.SelectorLabels`. Data-bearing resources (data PVC, backup PVC, CNPG cluster,
scheduled and pre-upgrade backups) and the identity Secrets (`<name>-secrets`, `<name>-admin`,
`<name>-ssh-host-keys`) get an owner reference only with `spec.storage.retainPolicy: Delete`.
Manual backups never do.

## Optional integrations

CNPG, prometheus-operator (ServiceMonitor/PrometheusRule), metrics-server, Gateway API
(HTTPRoute, TCPRoute, ReferenceGrant) and cert-manager (Certificate) are optional.
Check `rc.HasAPI(gvk)` (or `rc.FirstAPI` for kinds served in several versions, such as TCPRoute
`v1`/`v1alpha2`) before touching their objects, and use `unstructured.Unstructured`
for their kinds, so the operator has no Go dependency on their modules.

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
| I Basic install | Forgejo, CNPG database, secrets, admin, Services, Ingress or Gateway API routes and cert-manager TLS from one CR; non-root pods that pass restricted Pod Security | core |
| II Seamless upgrades | Image change → version gate (no silent downgrade, supported upgrade paths) → cold pre-upgrade backup → single-pod `Recreate` rollout with `forgejo migrate` → verification; per-major rendering and support reporting from the version table | upgrade, forgejo |
| III Full lifecycle | Scheduled and manual backups with retention, restore with maintenance mode and migration hold, retain policy for data, CNPG-managed DB failover | backup, core |
| IV Deep insights | Operator metrics, Forgejo `/metrics` and ServiceMonitor, PrometheusRule alerts, Grafana dashboard, health and stats in status, events, conditions | observability |
| V Auto pilot | Auto-rollback of failed upgrades, config rollback on crash loops, health-based restarts, OOM- and usage-driven vertical sizing, HPA with Valkey HA, PVC auto-expansion (also while the database is down), resource-derived tuning (GOMEMLIMIT, DB pool, workers) | autopilot, upgrade |
