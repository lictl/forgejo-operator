# ForgejoVersionAdvisory

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Labels | `major` |
| Expression | `max by (namespace, name, major) (forgejo_operator_version_advisory{namespace="<ns>", name="<name>"}) == 1` |
| For | 1h |

## Meaning

The instance runs a supported major, but a patch release that the operator's version table
recommends replacing: it misses critical security fixes (for example 15.0.7) or upstream
advises against it (for example 17.0.0). The `VersionSupported` condition stays `True` and names
the minimum patch release.

## Impact

Known vulnerabilities or bugs of that patch release apply to the instance.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="VersionSupported")].message}{"\n"}'
kubectl -n $NS get fj $N -o jsonpath='{.spec.image} {.spec.imagePullPolicy} {.status.version}{"\n"}'
kubectl -n $NS events --for fj/$N | grep VersionAdvisory
```

```promql
forgejo_operator_version_advisory{namespace="<ns>", name="<name>"}
```

## Likely causes

- `spec.image` pins an old patch release (`:15.0.7`).
- A floating tag (`:17`) with `imagePullPolicy: IfNotPresent`: nodes keep the patch release
  they pulled first.

## Mitigation

1. Pin `spec.image` to the patch release the message names (`N.0.P` or newer).
2. With a floating tag, set `imagePullPolicy: Always` and restart the pods
   (`kubectl -n $NS rollout restart deploy/$N`), or pin the patch release.

## Escalation

Treat advisories about security fixes like any other security patch. Escalate to the operator's
maintainers if the advisory is wrong for the running version (include `status.version`).
