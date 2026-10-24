# ForgejoHealthCheckFailing

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `increase(forgejo_operator_health_check_failures_total{namespace="<ns>", name="<name>"}[10m]) > 3` |
| For | – |

## Meaning

More than three of the operator's probes of `GET /api/healthz` (through Service `<name>-http`,
every `spec.autoPilot.selfHealing.healthCheckInterval`) failed in the last 10 minutes: Forgejo
answered with failing checks (HTTP 424, `status: fail`), another error, or accepted the
connection and did not answer within 5s. Probes are only made while a pod is Ready.

## Impact

Forgejo is degraded: requests that need the failing dependency fail (the database for nearly
everything, Valkey for sessions and queues with HA). The kubelet's readiness probe uses the same
endpoint, so persistent failures take the pods out of the Service. Self-healing restarts pods
after `failureThreshold` consecutive failures.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.health}{"\n"}'     # checks: database:ping, cache:ping
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="Healthy")]}{"\n"}'
kubectl -n $NS events --for fj/$N | grep -E 'HealthCheck|PodRestarted'
kubectl -n $NS logs deploy/$N -c forgejo --since=15m | grep -iE 'error|fail' | tail -30
```

```promql
increase(forgejo_operator_health_check_failures_total{namespace="<ns>", name="<name>"}[10m])
```

## Likely causes

- `database:ping=fail`: PostgreSQL down, failing over, out of connections, or the role cannot
  log in.
- `cache:ping=fail` (HA): Valkey without a primary ([ForgejoValkeyNoMaster](ForgejoValkeyNoMaster.md)).
- A hung or overloaded Forgejo (no answer within 5s): CPU throttling, memory pressure, slow
  storage.

## Mitigation

1. Database: check the CNPG cluster `kubectl -n $NS get cluster.postgresql.cnpg.io $N-db` and
   its logs; raise `max_connections` (`spec.database.cnpg.parameters`) if connections run out.
2. Valkey: see [ForgejoValkeyNoMaster](ForgejoValkeyNoMaster.md).
3. Overload: raise `spec.resources` or enable vertical autoscaling; with HA add replicas.
4. Let self-healing act, or restart by hand: `kubectl -n $NS rollout restart deploy/$N`.

## Escalation

Escalate to the database owners for persistent `database:ping` failures. Escalate to the
operator's maintainers when probes fail while Forgejo answers healthz correctly from inside the
pod (`kubectl -n $NS exec deploy/$N -c forgejo -- wget -qO- http://127.0.0.1:3000/api/healthz`).
