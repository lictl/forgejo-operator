# ForgejoAvailabilityFastBurn

| | |
|---|---|
| Severity | critical (page) |
| Source | PrometheusRule `<name>`, group `forgejo-slo.<ns>.<name>` |
| Labels | `slo: availability`, `long`, `short` (the window pair that fired) |
| Expression | `forgejo_slo:availability_burn_rate{window=<long>} > B and forgejo_slo:availability_burn_rate{window=<short>} > B`, B = 14.4 (long 1h, short 5m at a 30d window) or 6 (6h/30m) |
| For | – (the short window debounces) |

## Meaning

Forgejo has been unavailable much more often than its availability objective allows, and still
is. The burn rate is the error ratio of the operator's availability samples divided by the
error budget (`100 - spec.monitoring.slo.availabilityTarget` percent): at a burn rate of 1 the
budget lasts exactly the SLO window, at 14.4 it is gone in 1/14.4 of it (about 2 days of a
30-day window), at 6 in 1/6.

The alert needs both windows of a pair above the threshold: the long one says the outage is
significant (14.4x over 1h spends 2% of a 30-day budget), the short one that it is still going
on, so the alert resolves within minutes of recovery (5m, respectively 30m, after the outage
ends). The windows scale with `spec.monitoring.slo.window` (`long`/`short` labels), but are
never shorter than 5m nor than three health-check intervals.

Samples (`forgejo_operator_availability_probes_total`, one per
`spec.autoPilot.selfHealing.healthCheckInterval` of time, 30s by default; a reconcile that comes
late, for example while a step fails, counts every interval it missed):

- **up**: `/api/healthz` passed;
- **down**: it failed, did not reach Forgejo (`Unreachable`), or no Forgejo pod was Ready while
  replicas are desired;
- **planned** (count as available): a restore holds the instance or brings it back, an
  orchestrated upgrade is in flight, `spec.paused`, zero desired replicas, or the instance has
  never been available yet. A failed restore or `spec.restoreFrom` that holds the instance at
  zero until a person acts (phase `Degraded`) is not planned.

The error ratio is unplanned down samples over all samples, and over these windows the
denominator is at least one sample per interval: time without samples counts as available. So
one failed probe never pages at the default interval, also not right after a restore or on a new
instance; from an interval of about 4.5 minutes on (at 99.5%), one failed sample stands for
enough downtime to page.

## Impact

Users cannot reach Forgejo (web, API, git over HTTP and SSH) for a significant share of the
recent past, and the availability objective will be missed if this continues for
`window / burn` (the description names the time). With `ForgejoDown` or `ForgejoNotReady`
firing as well, Forgejo is down right now.

## Diagnosis

1. Which window pair fired, and how fast is the burn?
   ```promql
   forgejo_slo:availability_burn_rate{namespace="<ns>", name="<name>"}
   ```
2. What did the samples see? `result="down"` with `planned="false"` are the ones that count:
   ```promql
   sum by (result, planned) (increase(forgejo_operator_availability_probes_total{namespace="<ns>", name="<name>"}[15m]))
   ```
3. Is Forgejo down now, and why? The Healthy condition tells the three kinds of down samples
   apart: `HealthCheckFailed` (Forgejo answers with failing checks), `Unreachable` (the
   operator cannot connect) and `NoReadyReplicas` (no pod is Ready):
   ```sh
   kubectl -n $NS get fj $N -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
   kubectl -n $NS get fj $N -o jsonpath='{.status.health}{"\n"}'
   kubectl -n $NS get pods -l app.kubernetes.io/instance=$N -o wide
   kubectl -n $NS events --for fj/$N | tail -20
   ```
4. Check the other alerts of the instance: `ALERTS{namespace="<ns>", forgejo="<name>"}`.

## Likely causes

- The database is down or refuses Forgejo (`DatabaseReady=False`, healthz `database:ping=fail`):
  CNPG cluster `<name>-db` unavailable, credentials rotated or revoked, a full database volume.
- With HA, Valkey has no primary (`ForgejoValkeyNoMaster`, healthz `cache:ping=fail`).
- Pods do not start: crash loop after a configuration change (config rollback should catch
  it), image pull errors, unschedulable pods (resources, node loss, PVC attachment).
- A Recreate rollout (one replica, any change of the pod template) that takes long, e.g. a slow
  `forgejo migrate`.
- The operator cannot reach Forgejo (`Healthy=Unknown/Unreachable`, `ForgejoHealthProbeUnreachable`):
  a NetworkPolicy or service mesh change. Forgejo may still serve users; the SLI counts it as down
  because nothing shows that users can reach it.
- A restore or upgrade that was not orchestrated by the operator (a manual scale-down, an image
  change outside `spec.image`) counts as unplanned.
- A failed `ForgejoRestore` (or the rollback of a failed upgrade) that changed the instance holds
  it at zero replicas (`Ready=False/Maintenance`, phase `Degraded`, `MaintenanceHeld` event), as
  does a failed `spec.restoreFrom`: see [ForgejoDown](ForgejoDown.md#mitigation).

## Mitigation

1. Restore service first, following the runbook of the specific alert that fires as well
   ([ForgejoDown](ForgejoDown.md), [ForgejoNotReady](ForgejoNotReady.md),
   [ForgejoHealthCheckFailing](ForgejoHealthCheckFailing.md),
   [ForgejoValkeyNoMaster](ForgejoValkeyNoMaster.md),
   [ForgejoVolumeAlmostFull](ForgejoVolumeAlmostFull.md)).
2. Database: `kubectl -n $NS get cluster.postgresql.cnpg.io $N-db` and the CNPG pod logs; fix
   credentials or capacity.
3. A bad configuration: revert the last change of the `Forgejo` spec (`kubectl -n $NS get fj $N -o yaml`,
   compare with your GitOps history); `status.autoPilot.recentActions` shows whether the
   operator already rolled it back.
4. Network: if `Healthy` is `Unknown/Unreachable` but the pods are Ready and users can reach
   Forgejo, fix the NetworkPolicy/mesh rule that shuts the operator out (see
   [ForgejoHealthProbeUnreachable](ForgejoHealthProbeUnreachable.md)).
5. Planned work that is not modelled as planned: run restores through `ForgejoRestore`, upgrades
   through `spec.image`, and pause with `spec.paused` or `spec.replicas: 0`; then the samples are
   planned and do not burn the budget.

The alert resolves by itself once the short window holds no down samples (5m or 30m at a
30-day window and the default interval after recovery).

## Escalation

Page the owner of the instance's database (CNPG) or of the cluster network when the cause is
there. Escalate to the operator's maintainers when the operator itself keeps Forgejo down (a
step error in `Degraded=ReconcileError`, a restore or upgrade stuck, pods scaled to zero without
a reason in status) and include the `Forgejo` object, the events and the operator log.
