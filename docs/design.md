# forgejo-operator design

A Kubernetes operator for [Forgejo](https://forgejo.org) that targets **Operator
Capability Level V (Auto Pilot)**. It runs the official image
`codeberg.org/forgejo/forgejo:<version>` unchanged and uses
[CloudNativePG](https://cloudnative-pg.io) (CNPG) for PostgreSQL.

## APIs (`forgejo.lucas.icu/v1alpha1`)

| Kind | Purpose |
|------|---------|
| `Forgejo` (`fj`) | One Forgejo instance: image, DB, storage, networking, backup schedule, upgrade policy, monitoring, HA, autoscaling, autopilot. Has `scale` subresource. |
| `ForgejoBackup` (`fjb`) | Immutable one-shot backup: `pg_dump` (custom format) + tar.gz of the data volume, written to the `<forgejo>-backups` PVC. Reasons: Manual, Scheduled, PreUpgrade. |
| `ForgejoRestore` (`fjr`) | Restores a completed backup into an instance. The instance is scaled to zero while it runs. |

See `api/v1alpha1/*_types.go` for every field and default.

## Reconcile pipeline

`internal/controller/forgejo_controller.go` runs an ordered list of steps for each
`Forgejo`. Each step is a `forgejo.Step` (`func(ctx, *forgejo.ReconcileContext) (StepResult, error)`)
owned by one feature package. Steps communicate through `ReconcileContext`
(`internal/forgejo/context.go`); status is mutated in memory and patched once at the end.

Each step names the earlier steps it **needs** (whose outputs it consumes). A step runs only
when all of them *completed* in this pass, i.e. returned neither an error nor `Stop: true`;
otherwise it is skipped. A step that fails or stops (e.g. waiting for the database) therefore
skips only its dependants, and the remaining steps still run, so a rejected Ingress or a
Valkey outage does not also stop scheduled backups, health checks or a restore. Steps 1–11
form a chain (each needs the previous one); the others need only what is listed. Step errors
are joined into `Degraded=ReconcileError` (one event per failed step) and returned, and the
controller's backoff is capped at the 60s resync so time-based work keeps going. An
optimistic-lock conflict ends the pass and retries after 1s. When backups are enabled but
`backup-schedule` did not complete, `BackupHealthy` becomes `Unknown/ReconcileBlocked`
naming the root cause instead of keeping its last value. When `upgrade` is skipped,
`rc.Upgrading` is taken from `status.upgrade` (`forgejo.UpgradeInFlight`), so independent
steps never mistake an upgrade in flight for an idle instance. While `spec.paused` is set no
step runs; only `phase: Paused` is written and exported in `forgejo_operator_instance_info`.

| # | Step | Package | Needs | Outputs |
|---|------|---------|-------|---------|
| 1 | maintenance | backup | – | `rc.Maintenance`, `rc.RestoreMigrating` while a restore is active; `status.backup.activeRestore` |
| 2 | restore-guard | controller | 1 | `rc.Maintenance` while a restore Job still runs |
| 3 | secrets | core | 2 | `<name>-secrets`, `<name>-admin` |
| 4 | database | core | 3 | CNPG `Cluster <name>-db`; `rc.DB`, `rc.DatabaseReady`, `status.database`, `DatabaseReady` (Stop until ready) |
| 5 | high-availability | autopilot | 4 | Valkey `<name>-valkey`; `rc.ValkeyURL` (Stop until ready) |
| 6 | storage | core | 5 | PVC `<name>-data` |
| 7 | upgrade | upgrade | 6 | `rc.TargetImage`, `rc.Layout`, `rc.Upgrading`, `status.upgrade`, `Upgrading` |
| 8 | tuning | autopilot | 7 | `rc.Tuning`, `rc.TuningEnv`, `rc.ResourcesOverride`, `rc.HPAManaged`, `status.autoPilot.tuning` |
| 9 | config | core | 8 | Secret `<name>-config` (`app.ini`); `rc.ConfigSecretName`, `rc.ConfigHash`, `rc.DesiredConfigHash` |
| 10 | config-guard | autopilot | 9 | may switch to `<name>-config-lkg` |
| 11 | workload | core | 10 | Deployment `<name>` |
| 12 | network | core | – | Services `<name>-http`, `<name>-ssh`; Ingress `<name>` |
| 13 | scaling | autopilot | 8 | HPA `<name>`, PDB `<name>` |
| 14 | monitoring | observability | – | ServiceMonitor, PrometheusRule, dashboard ConfigMap |
| 15 | backup-schedule | backup | 2 | scheduled `ForgejoBackup`s, retention, `status.backup`, `BackupHealthy` |
| 16 | health | observability | 2 | `/api/healthz`, version, stats → `status.health`, `status.version`, `status.stats`, `Healthy` |
| 17 | autopilot | autopilot | 11, 16 | storage expansion, vertical scaling, self-healing, LKG bookkeeping |
| — | status | core | always | `phase`, `Ready`, `Progressing`, `Degraded`, replicas, URLs, instance metrics |

## Informer cache and reads

The manager caches Secrets and ConfigMaps only with `app.kubernetes.io/managed-by=forgejo-operator`,
and Pods, Jobs, Deployments and Services only with `app.kubernetes.io/name=forgejo`
(`forgejo.CacheScopes`, applied in `cmd/main.go`); every object the operator creates of these
kinds carries `forgejo.Labels`. `managedFields` are stripped from all cached objects. Without
this the operator would hold every Secret (Helm releases included), ConfigMap and Pod of the
cluster in memory. The manager's client wraps the cache (`forgejo.NewScopedReader`) so that
callers need not know about the selectors: a `Get` that misses the cache is confirmed with a
live read (user-referenced Secrets such as `configFrom`, `admin.existingSecret`, external DB
credentials, the CNPG `-app` Secret, CNPG pods), and a `List` whose label selector does not
imply the cache's selector (e.g. all pods of a namespace, CNPG pods by `cnpg.io/cluster`, Job
pods by `job-name`) is served live. Prefer selectors that include `app.kubernetes.io/name` for
frequent lists. CNPG credential Secret rotations are watched through a separate,
metadata-only cache of Secrets labelled `cnpg.io/cluster`. `--watch-namespace` /
`WATCH_NAMESPACE` restricts all of these to the given namespaces. The manager runs with
`GOMEMLIMIT` at ~90% of its memory limit (`config/manager/manager.yaml`).

`ForgejoBackup` and `ForgejoRestore` have their own controllers
(`internal/controller/forgejobackup_controller.go`, `forgejorestore_controller.go`)
backed by `internal/backup`.

## Pod layout

The official root image normally runs s6 as root, which starts OpenSSH and `forgejo web`
as `git` (uid 1000). The `-rootless` image uses a different entrypoint and different paths.
Both ship the same binary at `/app/gitea/gitea`. Like the Canonical charm, the operator
bypasses both entrypoints and runs Forgejo directly as non-root, with one layout
(`forgejo.LayoutForImage`):

| Item | Value |
|------|-------|
| Command | `/app/gitea/gitea web --config /etc/forgejo/app.ini` |
| User | uid/gid 1000 (`git` in both images), `RUN_USER=git` |
| Data PVC | `/data` (`WORK_PATH`/`APP_DATA_PATH=/data/gitea`, repos `/data/git/repositories`, LFS `/data/git/lfs`, `HOME=/data/git`) |
| app.ini | Secret `<name>-config` mounted read-only at `/etc/forgejo` |
| HTTP | 3000 (`/api/healthz` for startup/readiness; tcpSocket liveness) |
| SSH | built-in server, `START_SSH_SERVER=true`, `SSH_LISTEN_PORT=2222`, Service port 22 |
| Temp | emptyDir at `/tmp` |

1. The operator renders the complete `app.ini` into `<name>-config`, including every secret
   (`SECRET_KEY`, `INTERNAL_TOKEN`, `JWT_SECRET`, `LFS_JWT_SECRET`) and the DB credentials, so
   Forgejo never needs to write it. Its hash goes on the pod template
   (`forgejo.lucas.icu/config-hash`), so config changes roll the pods.
2. Init container `init-permissions` (root, `CHOWN`/`FOWNER`/`DAC_OVERRIDE` only) creates
   the top-level directories on the PVC and chowns them to 1000:1000 (non-recursive).
3. Init container `bootstrap` (uid 1000) runs `forgejo migrate`, then creates or updates the
   admin user idempotently from `<name>-admin`.
4. Main container (uid 1000, `runAsNonRoot`, no privilege escalation, all capabilities dropped).

## Naming and labels

All names come from `internal/forgejo/names.go`. Every child resource carries
`forgejo.Labels(instance, component)`; Deployments/Services select with
`forgejo.SelectorLabels`. Data-bearing resources (data PVC, backup PVC, CNPG cluster)
get an owner reference only when `spec.storage.retainPolicy: Delete`.

## Optional integrations

CNPG, prometheus-operator (ServiceMonitor/PrometheusRule) and metrics-server are
optional. Use `rc.HasAPI(gvk)` before touching their objects and use
`unstructured.Unstructured` for their kinds, so the operator has no Go dependency on
their modules.

## Capability levels

| Level | Capability | How |
|-------|-----------|-----|
| I Basic install | Provision Forgejo + CNPG + secrets + admin + services/ingress from one CR | core |
| II Seamless upgrades | Image change → version gate (no silent downgrade) → pre-upgrade backup → single-replica Recreate rollout with `forgejo migrate` → verify | upgrade |
| III Full lifecycle | Scheduled/manual backups with retention, restore with maintenance mode, storage lifecycle, CNPG-managed DB failover | backup, core |
| IV Deep insights | Operator metrics, Forgejo `/metrics` + ServiceMonitor, PrometheusRule alerts, Grafana dashboard, health/stats in status, events, conditions | observability |
| V Auto pilot | Auto-rollback of failed upgrades, config rollback on crash-loop, health-based restarts, OOM-driven and usage-driven vertical right-sizing, HPA with Valkey HA, PVC auto-expansion, resource-derived tuning (GOMEMLIMIT, DB pool, workers) | autopilot, upgrade |
