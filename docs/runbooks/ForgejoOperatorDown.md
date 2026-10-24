# ForgejoOperatorDown

| | |
|---|---|
| Severity | critical |
| Source | `config/prometheus/rules.yaml` (PrometheusRule `forgejo-operator-controller-manager-rules`) |
| Expression | `absent(controller_runtime_reconcile_total{controller="forgejo"})` |
| For | 10m |

## Meaning

Prometheus has had no metrics of the operator's `forgejo` controller for 10 minutes. Either the
operator does not run (or has not started its controllers, e.g. without leadership), or
Prometheus does not scrape it any more.

## Impact

Forgejo instances keep serving, but nothing is managed: no upgrades, scheduled backups, restores,
health checks, self-healing, volume expansion, runner registration. Every per-instance alert
except `ForgejoDown` goes silent (`ForgejoOperatorMetricsMissing` fires for every instance), and
the availability SLI has no samples.

## Diagnosis

```sh
OPNS=forgejo-operator-system
kubectl -n $OPNS get deploy,pods,svc,servicemonitor
kubectl -n $OPNS describe pod -l control-plane=controller-manager
kubectl -n $OPNS logs deploy/forgejo-operator-controller-manager --tail=100
kubectl -n $OPNS logs deploy/forgejo-operator-controller-manager --previous --tail=100
kubectl -n $OPNS get lease
```

```promql
up{namespace="forgejo-operator-system"}
controller_runtime_reconcile_total
```

## Likely causes

- The manager crash-loops (bad flag, `--runbook-base-url` that is not an http(s) URL, missing
  CRDs or RBAC after an incomplete upgrade), or is OOM-killed (`GOMEMLIMIT` and the memory limit).
- The Deployment is scaled to zero or its image cannot be pulled.
- No replica holds the leader lease (API server problems).
- The ServiceMonitor or the metrics Service is gone, or Prometheus may no longer `GET /metrics`.

## Mitigation

1. Fix the crash from the log (flags, CRDs: `make install` or the release's `install.yaml`, RBAC).
2. OOM: raise the memory limit in `config/manager/manager.yaml` and `GOMEMLIMIT` with it.
3. Scrape: re-apply `config/monitoring` (or `config/prometheus`) and check the target in
   Prometheus → Status → Targets.

## Escalation

Page the platform owner when the operator cannot be started again within the hour; escalate to
the operator's maintainers with the manager log for crashes you cannot explain.
