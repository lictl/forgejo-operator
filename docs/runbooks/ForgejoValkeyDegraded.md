# ForgejoValkeyDegraded

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(forgejo_operator_valkey_ready_members{namespace="<ns>", name="<name>"}) < 3`, unless the phase is `Restoring` or `Paused` |
| For | 15m |
| Rendered | with `spec.highAvailability.enabled` |

## Meaning

For 15 minutes fewer than three of the instance's Valkey pods have been Ready. Valkey still has
a primary (otherwise [ForgejoValkeyNoMaster](ForgejoValkeyNoMaster.md) fires), but it runs
without its full redundancy.

## Impact

No user impact now. One more failure leaves Valkey without a replica to promote or without a
Sentinel majority, and then Forgejo goes down.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.highAvailability.valkey}{"\n"}'     # master, members, sentinels
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="ValkeyReady")]}{"\n"}'
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N,app.kubernetes.io/component=valkey -o wide
kubectl -n $NS logs $N-valkey-0 -c sentinel --tail=50          # every Sentinel decision (+sdown, +odown, +switch-master)
kubectl -n $NS logs $N-valkey-0 -c valkey --tail=50
kubectl -n $NS events | grep -E "$N-valkey|ValkeyFailedOver"
```

```promql
forgejo_operator_valkey_ready_members{namespace="<ns>", name="<name>"}
```

## Likely causes

- A Valkey pod is Pending (resources, node selectors, too few nodes for the spreading).
- A pod crash-loops (configuration, memory limit).
- A replica cannot resynchronise from the primary (network, memory), so its `valkey`
  container is not Ready.

## Mitigation

Bring the missing pod back: fix scheduling or resources (`spec.highAvailability.valkey.resources`),
delete a pod stuck on a lost node, check the logs of the not-Ready container.

## Escalation

Escalate to the cluster owners when the pod cannot be scheduled; to the operator's maintainers
when the pods run but a replica never becomes Ready (include the `valkey` container log).
