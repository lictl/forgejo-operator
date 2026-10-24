# ForgejoVersionUnsupported

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Labels | `major`, `support` (`Deprecated`, `EndOfLife`, `Future`, `Unknown`) |
| Expression | `max by (namespace, name, major, support) (forgejo_operator_version_supported{namespace="<ns>", name="<name>"}) == 0` |
| For | 1h |

## Meaning

The Forgejo major the instance runs has been outside the operator's support policy (the current
LTS and the latest release; today 15 LTS and 17) for an hour:

- `Deprecated`: still maintained upstream, but not by this operator's policy; its end of life
  is near.
- `EndOfLife`: upstream ships no more security fixes.
- `Future`: newer than the operator's version table (or not released yet).
- `Unknown`: the image tag has no version and Forgejo has not reported one.

## Impact

No immediate outage. An end-of-life version gets no security fixes; a deprecated one will be
end of life soon; with `Future` the operator renders the configuration of the newest major it
knows, which may not fit. Upgrades between majors must follow the supported paths.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o wide                     # SUPPORT column
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="VersionSupported")].message}{"\n"}'
kubectl -n $NS get fj $N -o jsonpath='{.spec.image} {.status.image} {.status.version}{"\n"}'
```

```promql
forgejo_operator_version_supported{namespace="<ns>", name="<name>"}
forgejo_operator_version_end_of_life_timestamp_seconds{namespace="<ns>", name="<name>"}
```

## Likely causes

- The instance was not upgraded after a new LTS or release moved the policy on.
- `spec.image` pins an old major on purpose.
- The operator is older than the Forgejo release (`Future`).
- A digest or `latest`-style tag before the first successful probe (`Unknown`).

## Mitigation

1. Upgrade along the path the condition message names (for example 16 → 17, or 14 → 15 → 17):
   change `spec.image`; the operator takes a pre-upgrade backup and rolls back on failure. Read
   [versions.md, Upgrading between majors](../versions.md#upgrading-between-majors) first.
2. `Future`: update the operator to a release that supports the major, or go back to a
   supported image.
3. `Unknown`: use a version tag, or wait for the first health probe to report the version.

## Escalation

Plan the upgrade with the service owner before the end-of-life date in the message; treat
`EndOfLife` as a security issue. Escalate to the operator's maintainers when a supported image
is classified wrongly.
