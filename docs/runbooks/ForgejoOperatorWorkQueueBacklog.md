# ForgejoOperatorWorkQueueBacklog

| | |
|---|---|
| Severity | warning |
| Source | `config/prometheus/rules.yaml` |
| Labels | `name` (the work queue: `forgejo`, `forgejobackup`, `forgejorestore` or `forgejorunner`) |
| Expression | `max by (name) (workqueue_depth{name=~"forgejo\|forgejobackup\|forgejorestore\|forgejorunner"}) > 20` |
| For | 15m |

## Meaning

More than 20 objects have waited in one of the operator's work queues for 15 minutes: the
controller does not keep up. Each controller has a single worker, so one slow reconcile delays
all instances behind it.

## Impact

Reactions are delayed for every object of that kind: rollouts, backups, health checks and
self-healing happen late. Time-based work (scheduled backups, probes) drifts.

## Diagnosis

```promql
workqueue_depth{name="<name>"}
histogram_quantile(0.99, sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket[10m])))
rate(workqueue_adds_total{name="<name>"}[10m])
rate(rest_client_requests_total{code=~"429|5.."}[10m])
```

```sh
kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=15m | tail -50
kubectl -n forgejo-operator-system top pod
```

## Likely causes

- Slow reconciles: health probes or Forgejo API calls that run into their timeouts (an
  unreachable instance), slow API server responses, client-side throttling.
- Many objects changing at once (a mass update, a namespace with many instances).
- CPU throttling of the operator pod.

## Mitigation

1. Fix unreachable instances ([ForgejoHealthProbeUnreachable](ForgejoHealthProbeUnreachable.md)),
   so their probes stop running into timeouts.
2. Give the operator more CPU (`config/manager/manager.yaml`).
3. If the API server throttles, check its load; the backlog drains once it answers normally.

## Escalation

Escalate to the operator's maintainers when the queue grows without load or slow requests
explaining it (include the reconcile-time histogram and the log).
