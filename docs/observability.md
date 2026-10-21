# Observability

This page covers the operator's Level IV ("deep insights") features:

- Prometheus metrics from Forgejo and from the operator.
- One ServiceMonitor, one PrometheusRule and an optional Grafana dashboard for each `Forgejo`.
- Health, version and statistics in `status`.
- Conditions and events.

Code: `internal/observability` (steps `monitoring` and `health`), `internal/forgejo/metrics.go`
(operator metrics) and `config/prometheus` (the operator's own ServiceMonitor and alerts).

## Prerequisites

Everything below works with [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack).
Each integration is optional:

- **No prometheus-operator CRDs:** the operator skips the ServiceMonitor and PrometheusRule and
  still serves its own metrics.
- **No Grafana sidecar:** the dashboard ConfigMap is harmless.

A test setup like the one used for development:

```yaml
# helm upgrade --install kps prometheus-community/kube-prometheus-stack -n monitoring --create-namespace -f values.yaml
prometheus:
  prometheusSpec:
    # Select ServiceMonitors/PrometheusRules from every namespace, without a release label.
    serviceMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    podMonitorSelectorNilUsesHelmValues: false
grafana:
  sidecar:
    dashboards:
      enabled: true
      label: grafana_dashboard
      labelValue: "1"
      searchNamespace: ALL
      folderAnnotation: grafana_folder   # dashboards land in a "Forgejo" folder
      provider:
        foldersFromFilesStructure: true
```

If your Prometheus selects objects by label (for example `release: kps`), add that label to
each object with `spec.monitoring.serviceMonitor.labels` and `spec.monitoring.prometheusRule.labels`.

### Scraping the operator

Enable `../prometheus` in `config/default/kustomization.yaml` (the `[PROMETHEUS]` sections).
This deploys two objects:

- `config/prometheus/monitor.yaml`, a ServiceMonitor for the operator's HTTPS metrics endpoint.
- `config/prometheus/rules.yaml`, the operator-level alerts.

The ServiceMonitor sets **`honorLabels: true`**, and this is required. Operator metrics
identify an instance by their own `namespace` and `name` labels. Without `honorLabels`,
Prometheus renames `namespace` to `exported_namespace`, and the generated alerts and the
dashboard no longer match. If you scrape the operator some other way (a custom scrape config, a
PodMonitor), keep `honor_labels: true`.

## Per-instance resources

The `monitoring` step creates these objects. Each gets the standard labels plus
`spec.monitoring.<resource>.labels`, and has the `Forgejo` as its controller owner.

| Object | Name | Created when |
|--------|------|--------------|
| ServiceMonitor | `<name>` | ServiceMonitor CRD present, `spec.monitoring.enabled` (default true) and `spec.monitoring.serviceMonitor.enabled` (default true) |
| PrometheusRule | `<name>` | PrometheusRule CRD present, `spec.monitoring.enabled` and `spec.monitoring.prometheusRule.enabled` (default true) |
| ConfigMap (Grafana dashboard) | `<name>-dashboard` | `spec.monitoring.enabled` and `spec.monitoring.grafanaDashboard.enabled` (default **false**) |

Turning a toggle off deletes the object again, but only if this instance controls it.

```yaml
spec:
  monitoring:
    enabled: true            # /metrics in app.ini, protected by a generated bearer token
    statsInterval: 5m        # status.stats sampling period
    serviceMonitor:
      interval: 60s          # default; every scrape runs COUNT(*) queries in Forgejo's database
      labels: {release: kps}
    prometheusRule:
      labels: {release: kps}
    grafanaDashboard:
      enabled: true
      labels: {}
```

### ServiceMonitor

The ServiceMonitor scrapes port `http` of Service `<name>-http` at path `/metrics`. It
authenticates with `authorization: {type: Bearer, credentials: <name>-secrets/metrics-token}`.

- **Labels:** prometheus-operator sets `job="<name>-http"`, `namespace`, `service`, `pod` and `instance`.
- **Token:** the core step generates it and renders it as `[metrics] TOKEN` in app.ini. Requests
  without it get `401`.
- **Exposure:** Forgejo serves `/metrics` on its public HTTP listener, so block `/metrics` at your
  ingress or proxy if the token alone is not enough for you.

### Grafana dashboard

The dashboard ConfigMap carries the label `grafana_dashboard: "1"` and the annotation
`grafana_folder: Forgejo`. Its data key is `forgejo-<namespace>-<name>.json`.

| | |
|---|---|
| Title | `Forgejo / <namespace>/<name>` |
| UID | `forgejo-<namespace>-<name>`, or a hash when that is longer than 40 characters |
| Hidden constants | `$namespace`, `$forgejo` and `$job`; edit these to reuse the JSON for another instance |
| Template | `internal/observability/dashboard.json` |

| Row | Panels |
|-----|--------|
| Overview | Forgejo targets up, Ready, version, health check failures (1h), age of the last successful backup, data volume usage |
| Forgejo statistics | Users, repositories, organizations, issues, plus their trends (open/closed issues, comments, mirrors) |
| Operator | Health check failures, autopilot actions by type, volume usage by PVC, backups by reason/result, backup size and duration |
| Runtime | Forgejo RSS and heap, CPU, goroutines (per pod) |

## Metrics

### Forgejo (`/metrics`, scraped through the ServiceMonitor)

Forgejo 16 keeps the `gitea_` prefix.

| Metric | Meaning |
|--------|---------|
| `gitea_build_info{version,goversion,...}` | Always 1; carries the running version |
| `gitea_users`, `gitea_organizations`, `gitea_repositories`, `gitea_teams` | Object counts |
| `gitea_issues`, `gitea_issues_open`, `gitea_issues_closed`, `gitea_comments`, `gitea_labels`, `gitea_milestones` | Issue counts |
| `gitea_mirrors`, `gitea_releases`, `gitea_stars`, `gitea_watches`, `gitea_follows`, `gitea_webhooks`, `gitea_hooktasks`, `gitea_publickeys`, `gitea_oauths`, `gitea_loginsources`, `gitea_projects`, `gitea_projects_boards`, `gitea_attachments`, `gitea_accesses`, `gitea_updatetasks` | Other counts |
| `go_*`, `process_*` | Go runtime and process metrics |

Forgejo 16 exports no pull-request count, so `status.stats.pullRequests` stays 0. The
high-cardinality `gitea_issues_by_label` and `gitea_issues_by_repository` are only exported when
you enable `[metrics] ENABLED_ISSUE_BY_LABEL` / `ENABLED_ISSUE_BY_REPOSITORY` through `spec.config`.

### Operator (manager `/metrics`)

Every series has the labels `namespace` and `name` (the `Forgejo` object). The operator removes
all of an instance's series when the instance is deleted.

| Metric | Type | Extra labels | Meaning |
|--------|------|--------------|---------|
| `forgejo_operator_instance_ready` | gauge | | 1 when the Ready condition is True |
| `forgejo_operator_instance_info` | gauge | `image`, `version`, `phase` | Always 1 |
| `forgejo_operator_health_check_failures_total` | counter | | Failed `/api/healthz` probes made by the operator. Exported at 0 from the first reconcile |
| `forgejo_operator_stats` | gauge | `kind` (users, organizations, repositories, issues) | Same values as `status.stats` |
| `forgejo_operator_backup_last_success_timestamp_seconds` | gauge | | Unix time of the last successful backup |
| `forgejo_operator_backups_total` | counter | `reason`, `result` | Finished backups |
| `forgejo_operator_backup_duration_seconds` | gauge | | Duration of the last finished backup |
| `forgejo_operator_backup_size_bytes` | gauge | `part` (database/data) | Size of the last successful backup |
| `forgejo_operator_restores_total` | counter | `result` | Finished restores |
| `forgejo_operator_upgrades_total` | counter | `result` (succeeded/failed/rolledback) | Finished upgrades |
| `forgejo_operator_autopilot_actions_total` | counter | `action` | Autonomous actions, by type |
| `forgejo_operator_volume_used_ratio` | gauge | `claim` | Used/capacity of each PVC |

The endpoint also serves the standard controller-runtime metrics:

- `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total` and
  `controller_runtime_reconcile_time_seconds`, each with a `controller` label: `forgejo`,
  `forgejobackup` or `forgejorestore`.
- `workqueue_*`
- `rest_client_*`
- Go runtime metrics.

## Alerts

### Per instance (`PrometheusRule <name>`)

All alerts carry the labels `severity`, `namespace` and `forgejo: <name>`. Their descriptions
include the `kubectl` commands to start investigating with.

| Alert | Severity | Expression (simplified) | For |
|-------|----------|-------------------------|-----|
| `ForgejoDown` | critical | `max(up{job="<name>-http"}) == 0` or the target is absent; suppressed while the phase is Restoring or Paused. Only rendered when the ServiceMonitor is enabled and replicas > 0 | 5m |
| `ForgejoNotReady` | warning | `forgejo_operator_instance_ready == 0`; suppressed while Restoring or Paused | 10m |
| `ForgejoHealthCheckFailing` | warning | `increase(forgejo_operator_health_check_failures_total[10m]) > 3` | – |
| `ForgejoBackupFailed` | warning | A `result="failed"` backup in the last hour | – |
| `ForgejoBackupTooOld` | warning | `time() - backup_last_success_timestamp > maxAge`. Only rendered when `spec.backup.enabled` | 10m |
| `ForgejoBackupMissing` | warning | No successful backup recorded at all. Only rendered when `spec.backup.enabled` | maxAge |
| `ForgejoVolumeAlmostFull` | warning | `forgejo_operator_volume_used_ratio > 0.9` | 10m |
| `ForgejoUpgradeFailed` | warning | An upgrade with `result!="succeeded"` (failed or rolled back) in the last hour | – |
| `ForgejoAutoPilotActive` | warning | At least 5 autopilot actions in the last hour | – |

`maxAge` is the longest gap between two runs of `spec.backup.schedule` plus half of that gap,
and the slack is at least 1h. For example:

| Schedule | maxAge |
|----------|--------|
| `0 3 * * *` (the default) | 36h |
| `0 3 * * 1-5` (weekdays only) | 108h, because of the weekend gap |
| `0 * * * *` | 2h |

A counter series appears with value 1 on its first increment, and `increase()` cannot see that
first step. The backup and upgrade alerts therefore also fire when a failure series appeared
within the window.

### Operator (`config/prometheus/rules.yaml`)

| Alert | Severity | Condition |
|-------|----------|-----------|
| `ForgejoOperatorDown` | critical | No `controller_runtime_reconcile_total{controller="forgejo"}` for 10m |
| `ForgejoOperatorReconcileErrors` | warning | More than 5 reconcile errors in 15m, sustained for 15m |
| `ForgejoOperatorWorkQueueBacklog` | warning | Work queue depth above 20 for 15m |

## Status

### `status.health`

The `health` step fills `status.health` by probing `GET /api/healthz` through
`http://<name>-http.<namespace>.svc.cluster.local:<port>` with a 5s timeout.

| Field | Meaning |
|-------|---------|
| `healthy` | Last probe returned 200 with `status` pass or warn. False while probes are suspended |
| `lastCheckTime` | Time of the last probe |
| `lastHealthyTime` | Time of the last successful probe |
| `consecutiveFailures` | Failed probes in a row against Ready pods. Reset by a success or while probes are suspended |
| `message` | `all checks pass`, the failing checks (`status fail: database:ping=fail`), the connection error, or why probing is suspended |
| `checks` | Map from check name to status, e.g. `database:ping: pass`, `cache:ping: pass` |

How probing behaves:

- **Rate:** at most once per `spec.autoPilot.selfHealing.healthCheckInterval` (default 30s). The
  operator requeues the instance accordingly.
- **Suspended:** while no pod of the instance is Ready and not terminating, or while a restore
  holds the instance in maintenance. The Healthy condition is then Unknown (`NoReadyReplicas` /
  `Maintenance`), nothing is counted, and the message names the failing checks of the last probe.
- **Why only Ready pods count:** `consecutiveFailures` feeds self-healing, which restarts pods
  that stay Ready but unhealthy, so a pod that is not serving traffic must not add to it.
- **Effect of the readiness probe:** the pod's readiness probe also uses `/api/healthz`. In an
  outage that affects every pod, such as the database being unreachable, the pods leave the
  Service after about 30s. The operator records the failures until then and then reports Unknown
  together with the last failing checks. The Ready condition, `ForgejoNotReady` and the kubelet's
  `Unhealthy` events cover the rest of the outage.

### `status.version`

The version comes from `GET /api/v1/version` after each successful probe, for example
`16.0.6+gitea-1.22.0`. It is also shown in the `VERSION` column of `kubectl get fj`.

### `status.stats`

`users`, `organizations`, `repositories`, `issues` and `lastUpdated` are sampled from
Forgejo's `/metrics` with the bearer token. Sampling happens at most every
`spec.monitoring.statsInterval` (default 5m), only after a successful probe, and only while
monitoring is enabled. The values are also exported as `forgejo_operator_stats`.

## Conditions

| Type | Set by | Meaning |
|------|--------|---------|
| `Ready` | core | Forgejo is serving with the desired spec |
| `Progressing` | core | A rollout, upgrade or restore is in flight |
| `Degraded` | core / orchestrator | Something is wrong but the operator keeps trying (e.g. `ReconcileError` names the failing step) |
| `DatabaseReady` | core | The PostgreSQL backend is reachable/healthy |
| `Upgrading` | upgrade | A version upgrade is being orchestrated |
| `BackupHealthy` | backup | State of scheduled backups |
| `StorageHealthy` | autopilot | PVC capacity/usage |
| `Healthy` | observability | Result of the operator's `/api/healthz` probe. Reasons: `HealthCheckPassed`, `HealthCheckFailed` (message includes the streak), `NoReadyReplicas`, `Maintenance` |

## Events

The observability steps emit these events on the `Forgejo` object:

| Reason | Type | When |
|--------|------|------|
| `HealthCheckFailed` | Warning | The probe outcome changes from passing (or never probed) to failing |
| `HealthCheckRecovered` | Normal | The probe outcome changes from failing to passing |
| `VersionDetected` | Normal | `/api/v1/version` reports a different version than `status.version` |

Other packages emit their own events:

- Autopilot actions appear as events named after the action (`PodRestarted`,
  `ConfigRolledBack`, `ResourcesScaled`, `StorageExpanded`, ...) and are also listed in
  `status.autoPilot.recentActions`.
- `ReconcileError` (Warning) names a failing reconcile step.
- Backup, maintenance, HA and config events include `BackupScheduled`, `BackupPruned`,
  `MaintenanceStarted`, `MaintenanceEnded`, `LastKnownGoodConfig` and `ConfigRetry`.

## How to look at it

```sh
# Summary, conditions and health
kubectl get fj -A
kubectl describe fj <name> -n <ns>
kubectl get fj <name> -n <ns> -o jsonpath='{.status.health}{"\n"}{.status.stats}{"\n"}'
kubectl get fj <name> -n <ns> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl events -n <ns> --for fj/<name>

# Generated monitoring objects
kubectl get servicemonitor,prometheusrule,configmap -n <ns> -l app.kubernetes.io/instance=<name>
```

`forgejo` is both a short name of `Forgejo` and the category of all three kinds, so
`kubectl get forgejo <name>` is ambiguous. Use `fj` (or `forgejos`) when you name an object.

Useful PromQL:

```promql
up{namespace="<ns>", job="<name>-http"}                                   # Forgejo targets
max(gitea_users{namespace="<ns>", job="<name>-http"})                    # users
forgejo_operator_instance_ready{namespace="<ns>", name="<name>"}
increase(forgejo_operator_health_check_failures_total{namespace="<ns>", name="<name>"}[1h])
time() - forgejo_operator_backup_last_success_timestamp_seconds{namespace="<ns>", name="<name>"}
ALERTS{forgejo="<name>"}
```

Grafana: open **Dashboards → Forgejo → Forgejo / &lt;ns&gt;/&lt;name&gt;**.
