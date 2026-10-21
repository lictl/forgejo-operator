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
A step may return `Stop: true` (e.g. waiting for the database), after which only
`core.ReconcileStatus` runs.

| # | Step | Package | Outputs |
|---|------|---------|---------|
| 1 | secrets | core | `<name>-secrets`, `<name>-admin` |
| 2 | database | core | CNPG `Cluster <name>-db`; `rc.DB`, `rc.DatabaseReady`, `status.database`, `DatabaseReady` |
| 3 | high-availability | autopilot | Valkey `<name>-valkey`; `rc.ValkeyURL` |
| 4 | storage | core | PVC `<name>-data` |
| 5 | maintenance | backup | `rc.Maintenance` while a restore is active |
| 6 | upgrade | upgrade | `rc.TargetImage`, `rc.Layout`, `rc.Upgrading`, `status.upgrade`, `Upgrading` |
| 7 | tuning | autopilot | `rc.Tuning`, `rc.TuningEnv`, `rc.ResourcesOverride`, `status.autoPilot.tuning` |
| 8 | config | core | Secret `<name>-config` (`app.ini`); `rc.ConfigSecretName`, `rc.ConfigHash`, `rc.DesiredConfigHash` |
| 9 | config-guard | autopilot | may switch to `<name>-config-lkg` |
| 10 | workload | core | Deployment `<name>` |
| 11 | network | core | Services `<name>-http`, `<name>-ssh`; Ingress `<name>` |
| 12 | scaling | autopilot | HPA `<name>`, PDB `<name>` |
| 13 | monitoring | observability | ServiceMonitor, PrometheusRule, dashboard ConfigMap |
| 14 | backup-schedule | backup | scheduled `ForgejoBackup`s, retention, `status.backup`, `BackupHealthy` |
| 15 | health | observability | `/api/healthz`, version, stats → `status.health`, `status.version`, `status.stats`, `Healthy` |
| 16 | autopilot | autopilot | storage expansion, vertical scaling, self-healing, LKG bookkeeping |
| — | status | core | `phase`, `Ready`, `Progressing`, `Degraded`, replicas, URLs (always runs) |

`ForgejoBackup` and `ForgejoRestore` have their own controllers
(`internal/controller/forgejobackup_controller.go`, `forgejorestore_controller.go`)
backed by `internal/backup`.

## Pod layout (official root image)

The root image runs s6 as root and starts `sshd` (port 22) and `forgejo web` as `git`
(uid 1000). It reads `/data/gitea/conf/app.ini` (`GITEA_CUSTOM=/data/gitea`), and
`/etc/s6/gitea/setup` only generates app.ini when missing, then runs
`environment-to-ini`. So the operator:

1. Renders the whole `app.ini` (including secrets and DB credentials) into the Secret
   `<name>-config`, and puts its hash on the pod template (`forgejo.lucas.icu/config-hash`)
   so that config changes roll the pods.
2. Init container `init-config` (root): creates `/data/gitea/conf`, `/data/git`, copies
   app.ini in place, and fixes ownership (non-recursive) to 1000:1000.
3. Init container `bootstrap` (uid 1000): `forgejo migrate`, then creates or updates the
   admin user idempotently from `<name>-admin`.
4. Main container: the image's default entrypoint (s6) — no command override.

The `-rootless` flavor is supported through `forgejo.LayoutForImage` (paths under
`/var/lib/gitea`, config at `/etc/gitea/app.ini`, SSH on 2222, everything as uid 1000).

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
