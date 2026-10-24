# ForgejoOperatorReconcileErrors

| | |
|---|---|
| Severity | warning |
| Source | `config/prometheus/rules.yaml` |
| Labels | `controller` (`forgejo`, `forgejobackup`, `forgejorestore` or `forgejorunner`) |
| Expression | `sum by (controller) (increase(controller_runtime_reconcile_errors_total{controller=~"forgejo\|forgejobackup\|forgejorestore\|forgejorunner"}[15m])) > 5` |
| For | 15m |

## Meaning

For 15 minutes, the controller named in the `controller` label has returned errors from more
than five reconciles per 15 minutes. Optimistic-lock conflicts are retried without an error, so
these are real failures of a step.

## Impact

The affected objects are not fully reconciled: depending on the failing step, rollouts,
backups, restores or runner registrations do not progress. Steps that do not depend on the
failing one keep running.

## Diagnosis

```sh
kubectl get fj,fjb,fjr,fjrn -A
kubectl get fj -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.status.conditions[?(@.type=="Degraded")].message}{"\n"}{end}'
kubectl get events -A --field-selector reason=ReconcileError
kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep -i error | tail -30
```

```promql
sum by (controller) (increase(controller_runtime_reconcile_errors_total[15m]))
```

`Degraded=True/ReconcileError` on a `Forgejo` names each failing step and its error.

## Likely causes

- A step's API write is refused: an admission webhook or policy (Kyverno, Gatekeeper) rejects an
  object, a quota is exhausted, a CRD version is missing.
- The operator lacks a permission (RBAC narrowed, `config/namespaced` without the namespace).
- A user-provided Secret or setting is invalid in a way the CRD validation cannot see.
- API server overload or etcd problems.

## Mitigation

Fix what the `ReconcileError` message names: adjust the policy, quota or RBAC, or the spec.
The controller retries with a capped backoff (at most 60s for `Forgejo`), so it recovers on its own.

## Escalation

Escalate to the cluster owners for webhook, quota or API server problems; to the operator's
maintainers when the error comes from the operator's own logic (include the message and the
object's YAML).
