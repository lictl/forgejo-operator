# ForgejoCronRunnerDown

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(forgejo_operator_cron_runner_ready{namespace="<ns>", name="<name>"}) == 0` |
| For | 15m |
| Rendered | with `spec.highAvailability.enabled`; the gauge only exists while the cron runner should run |

## Meaning

With HA, Forgejo's cron tasks run in a single pod, `<name>-cron-0` (StatefulSet `<name>-cron`),
and in none of the server pods. That pod has not been Ready for 15 minutes while it should run
(not during upgrades, restores, migration holds or at zero replicas, when it is stopped on
purpose).

## Impact

No cron task runs: mirror updates, scheduled Actions workflows, cleanups, repository health
checks, `sync_external_users`. Web, API and git keep working. Missed runs catch up once the
runner is back.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.highAvailability.cronRunner}{"\n"}'
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N,app.kubernetes.io/component=cron -o wide
kubectl -n $NS describe pod $N-cron-0
kubectl -n $NS logs $N-cron-0 -c forgejo --tail=50
kubectl -n $NS events --for fj/$N | grep -E 'CronRunner|WaitingForCronRunner'
```

```promql
forgejo_operator_cron_runner_ready{namespace="<ns>", name="<name>"}
```

## Likely causes

- The pod's node is lost: the pod stays `Terminating` and no replacement starts (a Warning
  `CronRunnerStuck` names the pod and node). This is deliberate: two runners must never run.
- The pod crash-loops or is not Ready for the same reasons as a server pod (database, Valkey,
  configuration).
- The pod is Pending: it is placed on a node that runs a server pod when the data volume is
  ReadWriteOnce.

## Mitigation

1. Lost node: once the node is confirmed down (or its containers stopped), force-delete the pod:
   `kubectl -n $NS delete pod $N-cron-0 --force --grace-period=0`. Deleting the Node object or
   the `node.kubernetes.io/out-of-service` taint does the same.
2. Otherwise fix what keeps the pod from being Ready, as for a server pod
   ([ForgejoNotReady](ForgejoNotReady.md)).

## Escalation

Escalate to the cluster owners for lost nodes; to the operator's maintainers when the cron
runner is scaled to zero although the instance runs normally (include
`status.highAvailability.cronRunner` and the events).
