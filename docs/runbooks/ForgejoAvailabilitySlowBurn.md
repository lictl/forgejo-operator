# ForgejoAvailabilitySlowBurn

| | |
|---|---|
| Severity | warning (ticket) |
| Source | PrometheusRule `<name>`, group `forgejo-slo.<ns>.<name>` |
| Labels | `slo: availability`, `long`, `short` (the window pair that fired) |
| Expression | `forgejo_slo:availability_burn_rate{window=<long>} > B and forgejo_slo:availability_burn_rate{window=<short>} > B`, B = 3 (long 1d, short 2h at a 30d window) or 1 (3d/6h) |
| For | – |

## Meaning

Over the last day (burn rate 3) or three days (burn rate 1) Forgejo has been unavailable more
often than its availability objective allows, and the short window (2h or 6h) shows it has not
stopped. Typical sources are repeated short outages that each stay below the paging threshold of
[ForgejoAvailabilityFastBurn](ForgejoAvailabilityFastBurn.md): flapping health checks, frequent
Recreate rollouts, a database failing over again and again. At burn rate 3 the error budget of
the SLO window is gone in a third of it; at 1 it is used up exactly by the end of the window.

The SLI, planned downtime (counted as available) and the window scaling are the same as for
[ForgejoAvailabilityFastBurn](ForgejoAvailabilityFastBurn.md#meaning).

## Impact

No outage now necessarily, but users have seen errors or downtime repeatedly, and the
availability objective (`spec.monitoring.slo.availabilityTarget` over
`spec.monitoring.slo.window`) will be missed unless the cause is fixed. The remaining budget is
`forgejo_slo:availability_error_budget_remaining`.

## Diagnosis

1. When did the down samples happen? Look for a pattern (every rollout, every night, every
   failover):
   ```promql
   sum by (result, planned) (increase(forgejo_operator_availability_probes_total{namespace="<ns>", name="<name>"}[10m]))
   ```
2. Budget and burn:
   ```promql
   forgejo_slo:availability_error_budget_remaining{namespace="<ns>", name="<name>"}
   forgejo_slo:availability_burn_rate{namespace="<ns>", name="<name>"}
   ```
3. Health failures and unreachable probes, restarts and rollouts in the same period:
   ```promql
   increase(forgejo_operator_health_check_failures_total{namespace="<ns>", name="<name>"}[1d])
   increase(forgejo_operator_health_check_unreachable_total{namespace="<ns>", name="<name>"}[1d])
   increase(forgejo_operator_autopilot_actions_total{namespace="<ns>", name="<name>"}[1d])
   ```
   ```sh
   kubectl -n $NS events --for fj/$N
   kubectl -n $NS get fj $N -o jsonpath='{.status.autoPilot.recentActions}{"\n"}'
   kubectl -n $NS get pods -l app.kubernetes.io/instance=$N   # RESTARTS
   ```
4. The Grafana dashboard **Forgejo / &lt;ns&gt;/&lt;name&gt;**, row *Service level objectives*,
   shows the samples and burn rates over time.

## Likely causes

- Every change of the pod template restarts a single-replica instance (Recreate): frequent
  `spec` changes, vertical autoscaling resizing the pod, config rollbacks.
- Health checks that fail intermittently: a database close to its connection limit, Valkey
  blips, slow storage.
- Self-healing restarting pods repeatedly (`ForgejoAutoPilotActive`).
- Node maintenance (drains) of a single-replica instance.
- Unreachable probes after a network policy change (counted as down).

## Mitigation

1. Fix the recurring cause found above (database capacity, Valkey, a configuration that crashes).
2. Make routine changes cheaper: batch spec changes; with `spec.highAvailability.enabled` and
   two or more replicas, rollouts and node drains no longer take Forgejo down.
3. Model planned work as planned (`ForgejoRestore`, `spec.image` upgrades, `spec.paused`,
   `spec.replicas: 0`), so it does not burn the budget.
4. If the objective is stricter than the setup can deliver (for example 99.9% with a single
   replica), agree on a realistic `spec.monitoring.slo.availabilityTarget`.

## Escalation

Raise it with the service owner when the remaining error budget is below 25% or the cause is
structural (single replica, shared database). Escalate to the operator's maintainers when the
down samples coincide with operator actions you did not expect (restarts, rollbacks, scale-downs)
and include `status.autoPilot`, the events and the operator log.
