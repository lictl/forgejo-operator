# ForgejoDown

| | |
|---|---|
| Severity | critical |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(up{namespace="<ns>", job="<name>-http"}) == 0 or absent(up{...})`, unless the phase is `Restoring` or `Paused` (a failed restore that holds the instance is `Degraded` and alerts) |
| For | 5m |
| Rendered | only while the ServiceMonitor exists, and with replicas > 0 or an HPA |

## Meaning

Prometheus has not been able to scrape `/metrics` of any Forgejo pod behind Service
`<name>-http` for 5 minutes: no pod is in the Service's endpoints, or none answers. This is the
only alert that does not depend on the operator's own metrics.

## Impact

Forgejo is most likely down for every user: web UI, API, git over HTTP and SSH. CI runners
cannot fetch jobs. The availability SLI records down samples, so
[ForgejoAvailabilityFastBurn](ForgejoAvailabilityFastBurn.md) follows soon.

## Diagnosis

```sh
kubectl -n $NS get fj $N                                   # PHASE, READY
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N -o wide
kubectl -n $NS get endpointslices -l kubernetes.io/service-name=$N-http
kubectl -n $NS describe fj $N | sed -n '/Conditions/,$p'
kubectl -n $NS logs deploy/$N -c forgejo --tail=100
kubectl -n $NS logs deploy/$N -c bootstrap --tail=50       # migration / admin bootstrap
```

```promql
up{namespace="<ns>", job="<name>-http"}
forgejo_operator_instance_info{namespace="<ns>", name="<name>"}    # phase
```

If the pods are Ready but `up` is 0, check the scrape itself in Prometheus → Status → Targets
(`serviceMonitor/<ns>/<name>/0`): a `401` means the metrics token in `<name>-secrets` and the
rendered `app.ini` disagree.

## Likely causes

- The database is unreachable: pods fail their readiness probe (`/api/healthz`,
  `database:ping=fail`), `DatabaseReady=False`.
- Pods crash-loop: configuration error, failed migration in the `bootstrap` init container,
  out of memory.
- Pods cannot start: image pull error, unschedulable, data PVC not attachable.
- A failed restore holds the instance at zero replicas: `ForgejoRestore` failed after it changed
  the database or the data volume (also the rollback of a failed upgrade), or `spec.restoreFrom`
  failed (`status.restoredFrom.phase: Failed`, e.g. `TargetNotEmpty`). The phase is `Degraded`,
  `Ready=False/Maintenance` names the restore, and a `MaintenanceHeld` (or `RestoreFromFailed`,
  `TargetNotEmpty`) event was emitted. The operator does not end this hold by itself.
- The instance was scaled to zero outside the operator (it scales it back; a long gap points
  to a held workload, e.g. a failed upgrade, `ssh-host-keys` unavailable, see `Degraded`).
- Only the scrape fails (Forgejo serves): metrics token mismatch, NetworkPolicy blocking
  Prometheus.

## Mitigation

1. Database: check CNPG `kubectl -n $NS get cluster.postgresql.cnpg.io $N-db` and its pods; free
   space or restore credentials. Forgejo recovers on its own once the database answers.
2. Crash loop after a configuration change: revert the change in `spec.config` /
   `spec.configFrom`. With `spec.autoPilot.selfHealing.configRollback` (default) the operator
   rolls back to the last known good `app.ini` (`ConfigRolledBack` event).
3. Failed upgrade: see [ForgejoUpgradeFailed](ForgejoUpgradeFailed.md).
4. Held by a failed restore (`kubectl -n $NS get fjr`; the `Ready` message names it): the data
   volume may be half-extracted and the database may already hold the backup. Fix the cause in
   the restore's message and run a new `ForgejoRestore`, or start Forgejo on what is there with
   `kubectl -n $NS annotate fjr <restore> forgejo.lucas.icu/release-maintenance=true` (or
   delete the restore). A failed `spec.restoreFrom`: follow `status.restoredFrom.message` (delete
   the failed `<name>-restore-from` to retry, or remove `spec.restoreFrom`). See
   [backup-restore.md](../backup-restore.md).
5. Unschedulable pods: free resources or fix node selectors/tolerations; a lost node with a
   ReadWriteOnce volume needs the old pod force-deleted once the node is confirmed down.
6. Scrape-only failure: compare `kubectl -n $NS get secret $N-secrets -o jsonpath='{.data.metrics-token}'`
   with the ServiceMonitor's credentials, and allow Prometheus to reach port 3000.

## Escalation

Page the database or cluster owners when the cause is CNPG or the nodes. Escalate to the
operator's maintainers when Forgejo stays at zero replicas without a restore, upgrade or pause
explaining it (`kubectl -n $NS get deploy $N -o jsonpath='{.spec.replicas}'`), with the
`Forgejo` object, events and operator log.
