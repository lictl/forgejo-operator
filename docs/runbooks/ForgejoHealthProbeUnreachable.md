# ForgejoHealthProbeUnreachable

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `increase(forgejo_operator_health_check_unreachable_total{namespace="<ns>", name="<name>"}[W]) > 0`, W = max(5m, 2 x healthCheckInterval) |
| For | 15m |

## Meaning

For 15 minutes the operator's probes of `/api/healthz` have not reached Forgejo: DNS errors,
dial timeouts, refused or reset connections, or an HTTP 401/403/502/503/504 that is not a health
document (answered by a proxy or mesh sidecar, not Forgejo). These probes do not count as
health failures, so they never trigger self-healing restarts.

## Impact

The operator is blind: `status.health`, `status.version` and `status.stats` are not updated,
instance-level self-healing cannot act, and the availability SLI counts the samples as **down**
(nothing shows that users can reach Forgejo), so the burn-rate alerts may fire although Forgejo
serves its users. A `ForgejoRunner` of the instance cannot read Forgejo's API either.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="Healthy")].message}{"\n"}'
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N        # are they Ready?
kubectl -n $NS get networkpolicy
kubectl -n $NS get svc $N-http -o wide
# From the operator's namespace (any pod with curl, e.g. a debug pod):
kubectl -n forgejo-operator-system run probe --rm -it --restart=Never --image=curlimages/curl -- \
  curl -sv http://$N-http.$NS.svc.cluster.local:3000/api/healthz
```

```promql
increase(forgejo_operator_health_check_unreachable_total{namespace="<ns>", name="<name>"}[15m])
```

## Likely causes

- A NetworkPolicy in `<ns>` denies ingress from the operator's namespace to the Forgejo pods'
  port 3000.
- A service mesh with STRICT mTLS (or an authorization policy) rejects the operator's
  plaintext request.
- DNS problems in the operator's pod, or the operator runs outside the cluster (`make run`)
  without access to `svc.cluster.local`.
- The Service has no endpoints right after a pod became Ready (transient, never 15m).

## Mitigation

1. Allow the operator: an ingress rule from namespace `forgejo-operator-system` to the pods
   labelled `app.kubernetes.io/instance: <name>` on TCP 3000.
2. Istio: `PERMISSIVE` mTLS for that port, or a sidecar for the operator; adjust
   AuthorizationPolicies to admit the operator's identity.
3. Check the operator pod's DNS (`--cluster-domain` / `CLUSTER_DOMAIN` if the cluster does not
   use `cluster.local`).

The alert resolves once the window W holds no unreachable probe; the `Healthy` condition turns
True with the next probe that gets through.

## Escalation

Escalate to the cluster network or mesh owners for policy changes. Escalate to the operator's
maintainers when the probe works from a pod in the operator's namespace but the operator still
reports `Unreachable` (include the condition message).
