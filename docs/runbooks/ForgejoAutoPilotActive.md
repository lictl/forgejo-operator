# ForgejoAutoPilotActive

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `sum by (namespace, name) (increase(forgejo_operator_autopilot_actions_total{namespace="<ns>", name="<name>"}[1h])) >= 5` |
| For | – |

## Meaning

The operator took at least five autonomous actions on the instance in the last hour: pod
restarts (`PodRestarted`), configuration rollbacks (`ConfigRolledBack`), resizes
(`ResourcesScaled`), volume expansions (`StorageExpanded`) or upgrade rollbacks. Each action
fixes a symptom; this many in an hour usually hides a persistent problem.

## Impact

Every restart or resize of a single-replica instance is a short outage (and down samples for the
availability SLO). The underlying problem keeps coming back.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{range .status.autoPilot.recentActions[*]}{.time} {.type}: {.message}{"\n"}{end}'
kubectl -n $NS events --for fj/$N | grep -E 'PodRestarted|ConfigRolledBack|ResourcesScaled|StorageExpanded'
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N            # RESTARTS, OOMKilled
```

```promql
sum by (action) (increase(forgejo_operator_autopilot_actions_total{namespace="<ns>", name="<name>"}[1h]))
```

## Likely causes

- `PodRestarted` over and over: health checks that keep failing (database, Valkey) or a pod
  that hangs ([ForgejoHealthCheckFailing](ForgejoHealthCheckFailing.md)).
- `ConfigRolledBack`: a `spec.config` change that crash-loops is applied again and again (for
  example by GitOps reverting the rollback).
- `ResourcesScaled`: OOM kills or memory growth beyond `autoscaling.vertical.maxAllowed`.
- `StorageExpanded`: a volume growing quickly ([ForgejoVolumeAlmostFull](ForgejoVolumeAlmostFull.md)).

## Mitigation

Fix the cause the action type points to: the database or Valkey, the configuration in Git, the
memory limits (`spec.resources`, `autoscaling.vertical`), the data growth. To stop the operator
from acting while you investigate, set `spec.autoPilot.selfHealing.enabled: false` (restarts) or
`spec.paused: true` (everything).

## Escalation

Escalate to the operator's maintainers when actions repeat although their trigger is gone (for
example restarts while healthz passes), with `status.autoPilot` and the operator log.
