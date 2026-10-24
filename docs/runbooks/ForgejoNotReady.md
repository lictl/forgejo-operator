# ForgejoNotReady

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(forgejo_operator_instance_ready{namespace="<ns>", name="<name>"}) == 0`, unless the phase is `Restoring` or `Paused` (a failed restore that holds the instance is `Degraded` and alerts) |
| For | 10m |
| Rendered | with replicas > 0 or an HPA |

## Meaning

The operator has reported the instance as not `Ready` for 10 minutes. `Ready` is True when
Forgejo serves with the desired spec: the Deployment has its replicas available, the database
is ready, and no upgrade or restore holds it.

## Impact

Depends on the reason: with no available replica Forgejo is down (`ForgejoDown` fires as well);
during a long rollout, an upgrade held by a failure or a degraded database it may serve
partially. Automation that waits for `Ready` (runner registration, GitOps health checks) waits.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o wide
kubectl -n $NS get fj $N -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl -n $NS events --for fj/$N | tail -30
kubectl -n $NS get deploy,pods -l app.kubernetes.io/instance=$N
```

```promql
forgejo_operator_instance_ready{namespace="<ns>", name="<name>"}
forgejo_operator_instance_info{namespace="<ns>", name="<name>"}
```

The `Ready` condition's reason and the `DatabaseReady`, `Progressing`, `Upgrading` and
`Degraded` conditions name the blocker.

## Likely causes

- `DatabaseReady=False`: CNPG not ready, external database unreachable, credentials missing.
- `Progressing=True` for long: a rollout whose pods do not become available (image pull,
  scheduling, crash loop, slow migration).
- `Upgrading`: an upgrade in flight or a failed one holding the new image.
- `Degraded=ReconcileError`: a reconcile step fails (the message names it).
- `Ready=False/Maintenance` with phase `Degraded`: a failed restore (or a failed
  `spec.restoreFrom`) holds the instance at zero replicas until a person acts; see
  [ForgejoDown](ForgejoDown.md#mitigation).
- Missing user Secrets (`configFrom`, `admin.existingSecret`, object storage credentials).

## Mitigation

1. Fix what the condition names: database, Secrets, image, resources.
2. A failed upgrade: [ForgejoUpgradeFailed](ForgejoUpgradeFailed.md).
3. A reconcile step error: the `ReconcileError` event and the operator log have the details
   ([ForgejoOperatorReconcileErrors](ForgejoOperatorReconcileErrors.md)).
4. An instance you do not want to run: set `spec.replicas: 0` (the alert is not rendered then)
   or `spec.paused: true`.

## Escalation

Escalate to the operator's maintainers when the conditions do not explain the state (Ready
False with every prerequisite met) or a step keeps failing with an operator-side error.
