# forgejo-operator

A Kubernetes operator that runs [Forgejo](https://forgejo.org) from install through backups,
upgrades, monitoring and autonomous operations. It targets **Operator Capability Level V
(Auto Pilot)**.

- It runs the official image `codeberg.org/forgejo/forgejo:<version>`, or the `-rootless`
  flavor, unchanged. It starts `forgejo web` directly as uid 1000, with the built-in SSH server,
  so pods pass the `restricted` Pod Security Standard.
- It supports the current Forgejo LTS release and the latest release, today **15 (LTS)** and
  **17**; new instances get the LTS. Older and newer versions still run and are reported as
  unsupported. See [docs/versions.md](docs/versions.md).
- It uses [CloudNativePG](https://cloudnative-pg.io) for PostgreSQL, or an existing database.
- Everything is driven by three resources in `forgejo.lucas.icu/v1alpha1`:

| Kind | Short name | Purpose |
|------|-----------|---------|
| `Forgejo` | `fj` | One instance: image, database, storage, networking, backup schedule, upgrade policy, monitoring, scaling, autopilot. |
| `ForgejoBackup` | `fjb` | A point-in-time backup: `pg_dump` of the database plus an archive of the data volume. |
| `ForgejoRestore` | `fjr` | Restores a completed backup into an instance, which is scaled to zero meanwhile. |

## Capability levels

| Level | What the operator does |
|-------|------------------------|
| **I Basic install** | One `Forgejo` resource provisions a CNPG cluster (or uses an external database), generated secrets, the admin account, the data volume, the Deployment, HTTP/SSH Services and an optional Ingress. It renders the whole `app.ini` and rolls pods when it changes. |
| **II Seamless upgrades** | Changing `spec.image` to a newer version is version-gated and preceded by a cold backup. Forgejo is migrated with a single pod and verified. Downgrades are refused, and upgrades can be cancelled or retried. |
| **III Full lifecycle** | On-demand and cron-scheduled backups with retention, and restores with maintenance mode. Data is retained on deletion by default (`retainPolicy`), and CNPG handles database failover. |
| **IV Deep insights** | Health, version and statistics in status. Conditions and events, operator metrics, Forgejo `/metrics` through a ServiceMonitor, a PrometheusRule with per-instance alerts, and a Grafana dashboard. |
| **V Auto pilot** | Automatic rollback of failed upgrades and of configs that make Forgejo crash. Restarts hung pods. Vertical right-sizing from OOM kills and usage. HPA with Valkey-backed HA. PVC auto-expansion. Tuning derived from resources (GOMEMLIMIT, DB pool). |

Details are in [docs/design.md](docs/design.md), [docs/versions.md](docs/versions.md), [docs/upgrades.md](docs/upgrades.md),
[docs/backup-restore.md](docs/backup-restore.md), [docs/observability.md](docs/observability.md)
and [docs/autopilot.md](docs/autopilot.md).

## Quick start (local or any other cluster)

Prerequisites: Kubernetes 1.30+, `kubectl`, `helm`, and CloudNativePG.

```bash
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm upgrade --install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace --wait
```

Optional:
- kube-prometheus-stack, for ServiceMonitor, PrometheusRule and dashboards.
- metrics-server, for the HPA and vertical right-sizing.

Build and deploy the operator. On a local cluster the image goes straight into the cluster's image
store, so no registry is needed:

```bash
make deploy-local
```

On other clusters, push an image and deploy it:

```bash
make docker-build docker-push deploy IMG=<registry>/forgejo-operator:<tag>
```

Create an instance:

```bash
kubectl create namespace forgejo
kubectl apply -n forgejo -f config/samples/forgejo_v1alpha1_forgejo.yaml
kubectl get fj -n forgejo -w
```

```
NAME             PHASE     VERSION               READY   REPLICAS   AGE
forgejo-sample   Running   15.0.10+gitea-1.22.0  True    1          70s
```

The admin credentials are in the Secret `<name>-admin`:

```bash
kubectl get secret -n forgejo forgejo-sample-admin -o jsonpath='{.data.password}' | base64 -d
```

## Everyday operations

```bash
# Back up now (a cron schedule is spec.backup.enabled + spec.backup.schedule)
kubectl apply -n forgejo -f config/samples/forgejo_v1alpha1_forgejobackup.yaml

# Restore it (Forgejo is scaled to zero while data is replaced)
kubectl apply -n forgejo -f config/samples/forgejo_v1alpha1_forgejorestore.yaml

# Upgrade: a backup is taken first; a failed rollout is rolled back automatically
kubectl patch fj forgejo-sample -n forgejo --type merge \
  -p '{"spec":{"image":"codeberg.org/forgejo/forgejo:17"}}'
kubectl get fj forgejo-sample -n forgejo -o jsonpath='{.status.upgrade}'

# Change app.ini; the pods roll, and a config that crash-loops is rolled back
kubectl patch fj forgejo-sample -n forgejo --type merge \
  -p '{"spec":{"config":{"ui":{"DEFAULT_THEME":"forgejo-dark"}}}}'
```

A minimal high-availability instance with autoscaling:

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata:
  name: git
spec:
  replicas: 2
  highAvailability:
    enabled: true          # Valkey for cache, sessions and queues
  storage:
    size: 50Gi
    accessModes: [ReadWriteMany]
  resources:
    requests: {cpu: 250m, memory: 512Mi}
    limits: {memory: 2Gi}
  autoscaling:
    horizontal: {enabled: true, minReplicas: 2, maxReplicas: 5}
    vertical: {enabled: true, maxAllowed: {memory: 4Gi}}
  backup:
    enabled: true
    schedule: "0 3 * * *"
    retention: 14
  database:
    cnpg: {instances: 3}
  ingress:
    enabled: true
    host: git.example.com
    tlsSecretName: git-example-com-tls
```

Upstream Forgejo says it is not fully ready for more than one replica: scheduled cron jobs run
on every replica. Use the HA mode knowing this, and with storage that every replica can mount.

## Development

```bash
make install                          # CRDs
go build -o bin/manager ./cmd
bin/manager --watch-namespace=dev     # run against your kubeconfig, scoped to one namespace
make manifests generate               # after changing API types or RBAC markers
go test ./internal/...                # unit tests (the envtest suite needs `make setup-envtest`)
make forgejo-versions                 # compare the Forgejo version table with upstream releases
```

The reconcile pipeline is a table of steps in `internal/controller/forgejo_controller.go`. Each
step is owned by one package: `core`, `backup`, `upgrade`, `observability` or `autopilot`, and
declares the steps it depends on. A failing step only skips the steps that depend on it. See
[docs/design.md](docs/design.md).

## License

Apache License 2.0.
