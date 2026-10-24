# ForgejoValkeyFailedOver

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `forgejo_operator_valkey_failovers_total{namespace="<ns>", name="<name>"}` increased in the last 15m (or its series appeared) |
| For | – (fires for 15 minutes after each failover) |
| Rendered | with `spec.highAvailability.enabled` |

## Meaning

The Sentinels promoted a replica to Valkey primary within the last 15 minutes: the old
primary's pod failed, was evicted or deleted (a drain, a rolling update), or its node was lost.
A `ValkeyFailedOver` Warning event names the old and the new primary.

## Impact

Forgejo's operations failed for the few seconds of the failover (its queues retry most of them).
Replication is asynchronous, so writes of the old primary's last second may be lost: a few
cache entries or queued items. Planned failovers (drains, rolling updates) are expected and
harmless.

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
increase(forgejo_operator_valkey_failovers_total{namespace="<ns>", name="<name>"}[1d])
```

Check why the old primary's pod went away: `kubectl -n $NS describe pod $N-valkey-<i>` (last
state, OOMKilled), node events, a drain in progress.

## Likely causes

- Node drain or rolling update of the Valkey StatefulSet (planned).
- The primary's container was OOM-killed (Valkey's `maxmemory` is 80% of its memory limit; a
  limit that is too low for the cache plus queues).
- Node failure or eviction under memory pressure.

## Mitigation

Nothing to do for a planned failover. For repeated unplanned ones, raise the `valkey`
container's memory (`spec.highAvailability.valkey.resources`), or fix the failing node.

## Escalation

Escalate when failovers repeat several times a day without planned maintenance (cluster owners
for nodes, the operator's maintainers when the operator's own updates roll Valkey unexpectedly).
