# ForgejoOperatorMetricsMissing

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `absent(forgejo_operator_instance_ready{namespace="<ns>", name="<name>"})` |
| For | 15m |

## Meaning

Prometheus has no `forgejo_operator_instance_ready` series for this instance. The operator
exports it on every reconcile of every instance, so either the operator does not run, or
Prometheus does not scrape it, or scrapes it without `honorLabels: true` (then the series exist
with `exported_namespace` instead of `namespace`).

## Impact

Every per-instance alert except `ForgejoDown` reads operator metrics and is silent meanwhile:
backup and RPO alerts, health, volume, upgrade, version, runner, HA, autopilot and the SLO
burn-rate alerts. The availability SLI has no samples.

## Diagnosis

```sh
kubectl -n forgejo-operator-system get deploy,pods
kubectl -n forgejo-operator-system get servicemonitor,svc
kubectl -n $NS get fj $N                        # does the instance exist (and is it reconciled)?
```

```promql
forgejo_operator_instance_ready{name="<name>"}                        # any namespace label?
forgejo_operator_instance_ready{exported_namespace="<ns>", name="<name>"}  # scraped without honorLabels
up{job=~".*forgejo-operator.*"}
```

In Prometheus → Status → Targets, the operator's target
(`serviceMonitor/forgejo-operator-system/forgejo-operator-controller-manager-metrics-monitor/0`)
should be `UP`; `403 Forbidden` means Prometheus may not `GET /metrics`.

## Likely causes

- The operator is down: see [ForgejoOperatorDown](ForgejoOperatorDown.md).
- `config/prometheus` (or `config/monitoring`) was never applied, so nothing scrapes the
  operator.
- Prometheus selects ServiceMonitors by label (`release: kps`) and the operator's has none.
- Prometheus's ServiceAccount lacks `get` on the non-resource URL `/metrics` (the operator's
  `metrics-reader` ClusterRole).
- A custom scrape config without `honor_labels: true`.
- `--watch-namespace` excludes `<ns>`: the operator does not reconcile this instance at all.

## Mitigation

1. `kubectl apply -k config/monitoring` (ServiceMonitor with `honorLabels: true`, the operator
   alerts and the metrics-reader binding), or enable `../prometheus` in
   `config/default/kustomization.yaml`.
2. Add the labels your Prometheus selects on to the operator's ServiceMonitor.
3. Bind ClusterRole `forgejo-operator-metrics-reader` to Prometheus's ServiceAccount
   (`config/prometheus/metrics_reader_binding.yaml`).
4. With `--watch-namespace`, add `<ns>` or move the instance.

## Escalation

Escalate to the monitoring stack's owners for selector or RBAC changes in Prometheus. Escalate to
the operator's maintainers when the operator runs, is scraped (`up == 1`), and still exports no
series for a reconciled instance.
