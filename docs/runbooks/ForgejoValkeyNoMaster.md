# ForgejoValkeyNoMaster

| | |
|---|---|
| Severity | critical |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(forgejo_operator_valkey_master_available{namespace="<ns>", name="<name>"}) == 0`, unless the phase is `Restoring` or `Paused` |
| For | 2m |
| Rendered | with `spec.highAvailability.enabled` |

## Meaning

For two minutes the Valkey replication group of the instance (StatefulSet `<name>-valkey`, three
pods, each a Valkey server and a Sentinel) has had no Ready primary that its Sentinels agree on
(`ValkeyReady=False/NoMaster` or `NotReady`). A normal failover takes 2-10 seconds, so this is
not a failover in progress.

## Impact

Forgejo keeps its cache and its queues in Valkey, and `/api/healthz` pings the cache: about 30s
into the outage every server pod turns NotReady and Forgejo is down for everyone
(`ForgejoDown`, burn-rate alerts). Queued work (webhooks, mirror syncs, indexing) waits.

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
forgejo_operator_valkey_master_available{namespace="<ns>", name="<name>"}
forgejo_operator_valkey_ready_members{namespace="<ns>", name="<name>"}
```

## Likely causes

- Two or more Valkey pods are down (node loss, eviction, OOM): no Sentinel majority (quorum 2),
  so no failover.
- The primary failed and no replica can be promoted (replicas not in sync, all recreated empty).
- Valkey pods cannot be scheduled (resources, anti-affinity on too few nodes).
- The operator is emptying Valkey (event `ValkeyReset`, annotation
  `forgejo.lucas.icu/valkey-reset` on StatefulSet `<name>-valkey`), after a restore or a
  changed `valkey-password`: all pods stop, then start as a new group, normally within a minute.
  For longer, an old pod is not going away (stuck `Terminating` on a lost node).
- `ValkeyReady=Unknown/UnreachableFromOperator` is a different case: the operator cannot reach
  the Sentinels on port 26379 (NetworkPolicy); the gauge is then 1 while the pods are Ready.

## Mitigation

1. Get the Valkey pods running: free node resources, uncordon nodes, delete pods stuck on a
   lost node (`kubectl -n $NS delete pod $N-valkey-<i> --force --grace-period=0` once the node is
   confirmed down).
2. Once two Sentinels are back they elect a primary; the init container of a recreated pod
   joins as a replica or forces the failover. Forgejo follows the new primary on its own.
3. If every pod was recreated empty, the group starts with pod 0 as primary: cache and queued
   work are lost, Forgejo recreates most tasks.

## Escalation

Page the cluster owners for node or scheduling problems. Escalate to the operator's maintainers
when the pods are Running and Ready but the Sentinels name no primary for minutes (include the
Sentinel logs of all three pods and `status.highAvailability.valkey`).
