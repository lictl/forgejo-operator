# ForgejoRunnersOffline

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Labels | `runner` (the `ForgejoRunner`) |
| Expression | `runner_jobs_waiting > 0 and runners_online == 0 and runners_desired > 0`, per `runner` |
| For | 10m |

## Meaning

For 10 minutes Forgejo Actions jobs have been waiting that a `ForgejoRunner` of the instance
could run (its labels satisfy their `runs-on`, its scope covers their repository), the runner
set wants at least one runner, and Forgejo reports none of its runners online.

## Impact

CI/CD of the affected repositories is stuck: workflows queue and never start.

## Diagnosis

```sh
R=<runner label of the alert>
kubectl -n $NS describe fjrn $R                       # Ready, Registered, Degraded conditions, events
kubectl -n $NS get pods -l forgejo.lucas.icu/runner=$R
kubectl -n $NS logs $R-0 -c runner --tail=50          # connection, registration
kubectl -n $NS logs job/$R-register                   # a registration in progress or failed
```

```promql
forgejo_operator_runners_desired{namespace="<ns>", name="<name>", runner="<runner>"}
forgejo_operator_runners_registered{namespace="<ns>", name="<name>", runner="<runner>"}
forgejo_operator_runners_online{namespace="<ns>", name="<name>", runner="<runner>"}
forgejo_operator_runner_jobs_waiting{namespace="<ns>", name="<name>", runner="<runner>"}
```

## Likely causes

- Runner pods cannot start: Pod Security (`Ready=False/PodSecurityBlocked`; the Docker executor
  needs a privileged namespace), resources, image pulls.
- Registration fails (`Registered=False/RegistrationFailed`, wrong `scope`), or waits for the
  instance (`WaitingForForgejo`: not Ready, upgrading, restoring, paused).
- Runners cannot reach `<name>-http` (NetworkPolicy, DNS), or were deleted in Forgejo
  (`unregistered runner` in the log; the operator re-registers them).

## Mitigation

Follow the troubleshooting table of [actions-runners.md](../actions-runners.md#troubleshooting):
label the namespace `pod-security.kubernetes.io/enforce=privileged` or use `executor: Host`, fix
`scope`, allow the runner pods to reach the instance, or wait for the instance to be Ready again.

## Escalation

Escalate to the CI owners when runners cannot be brought online; to the operator's maintainers
when the runners are registered and online in Forgejo's UI but the metric says 0 (include the
`ForgejoRunner` status and `Degraded` condition).
