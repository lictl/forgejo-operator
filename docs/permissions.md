# Permissions

What the operator may do in the cluster, why, and how to narrow it. The operator runs as
ServiceAccount `forgejo-operator-controller-manager` in `forgejo-operator-system`. How it works
inside is in [design.md](design.md).

| Object | Content | Bound |
|--------|---------|-------|
| ClusterRole `forgejo-operator-manager-role` | Everything the controllers need ([below](#manager-role)); generated from the `+kubebuilder:rbac` markers into `config/rbac/role.yaml` (`make manifests`) | Cluster-wide (`config/default`), or per watched namespace ([namespaced install](#namespaced-install)) |
| ClusterRole `forgejo-operator-volume-usage` | `pods/exec` get, create, for volume measurement only (`config/rbac/volume_usage_role.yaml`) | Cluster-wide by the kustomize component `config/components/volume-usage` (part of `config/default`), per namespace, or not at all ([below](#volume-measurement-podsexec)) |
| Role `forgejo-operator-leader-election-role` | `leases` get, create, update and core `events` create, patch, in `forgejo-operator-system` only | Always |
| ClusterRole `forgejo-operator-metrics-auth-role` | `tokenreviews` and `subjectaccessreviews` create, to authenticate and authorize scrapes of the operator's `/metrics` | Always |
| ClusterRole `forgejo-operator-cluster-reader` | The cluster-scoped reads of manager-role (`config/namespaced/cluster_reader_role.yaml`) | Namespaced install only |

The other ClusterRoles of `config/rbac` (`forgejo-operator-forgejo-editor-role` and the admin,
editor and viewer roles of each kind) are for people, not for the operator; nothing binds them.

## manager-role

Every verb is used by the code; nothing is granted on `*`, and no rule uses `resourceNames`
(the names of an instance's objects are not known in advance). The operator never deletes data:
the data and backup PVCs, the CNPG Cluster and the identity Secrets go only through Kubernetes'
garbage collection under `spec.storage.retainPolicy: Delete`. The one exception is the rollback
of a failed PostgreSQL major upgrade, which deletes the CNPG Cluster to create it again on the old
major, and only while its cold pre-upgrade backup, read again right before, can restore it (it
exists, is `Completed` with a dump of the old major, and its volume exists); never after a revert
whose `pg_upgrade` did not start ([upgrades.md](upgrades.md#postgresql-major-upgrades)).

| API group | Resources | Verbs | Used for |
|-----------|-----------|-------|----------|
| core | `secrets` | get, list, watch, create, update, patch, delete | The instance's generated Secrets (`<name>-secrets`, also patched with the Valkey password, `-admin`, `-ssh-host-keys`, `-backup-key`), `app.ini` (`<name>-config`, `-config-lkg`), identity Secrets of a disaster recovery, a `ForgejoRunner`'s `<name>-tokens` and `<name>-runner`, and the identity bundle `<backup>-identity` of an upload Job, deleted with the Job (the only Secret the operator deletes). Reads of Secrets the spec names (`configFrom`, `admin.existingSecret`, external database, S3 and backup keys) and CNPG's `-app` Secret. list and watch feed the cache, which holds only Secrets labelled `app.kubernetes.io/managed-by=forgejo-operator`, and the metadata-only cache of CNPG Secrets (label `cnpg.io/cluster`); RBAC cannot express that filter |
| core | `configmaps` | get, list, watch, create, update, delete | Grafana dashboard `<name>-dashboard` |
| core | `services` | get, list, watch, create, update, delete | `<name>-http`, `<name>-ssh`, `<name>-valkey` (headless); delete: Valkey when HA is turned off |
| core | `persistentvolumeclaims` | get, list, watch, create, update, patch | Data and backup PVCs; owner references under `retainPolicy` and automatic expansion (patch). Never deleted |
| core | `pods` | get, list, watch, delete | Server, CNPG, Job (also CNPG's major upgrade Job) and runner pods; delete: a hung pod (self-healing) or a failing pod of an upgrade's new image |
| core | `namespaces` | get | The Pod Security label of a `ForgejoRunner`'s namespace (Docker executor) |
| core | `nodes`, `persistentvolumes` | get, list, watch | Placing backup and restore Jobs where node-local volumes can be attached; schedulable nodes and their memory (HA warnings, vertical sizing); whether nodes carry `topology.kubernetes.io/zone` (HA spreading) |
| `storage.k8s.io` | `storageclasses` | get, list, watch | `allowVolumeExpansion` before growing a volume |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete | Forgejo `<name>`; patch: autopilot restarts and vertical sizing |
| `apps` | `statefulsets` | get, list, watch, create, update, delete | Valkey `<name>-valkey` and the cron runner `<name>-cron` with HA (update: scaling the cron runner down before a new image; delete: both when HA is turned off), and `ForgejoRunner` pods |
| `batch` | `jobs` | get, list, watch, create, delete | Backup, upload, restore and runner registration Jobs, deleted once their result is recorded; CNPG's major upgrade Job is only read |
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch, create, update, delete | HPA `<name>` |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, update, delete | PDBs `<name>` and `<name>-valkey` |
| `networking.k8s.io` | `ingresses` | get, list, watch, create, update, delete | Ingress `<name>` |
| `events.k8s.io` | `events` | create, patch | Events on the operator's objects |
| `metrics.k8s.io` | `pods` | list | Usage of the Forgejo pods for vertical sizing |
| `postgresql.cnpg.io` | `clusters` | get, list, watch, create, update, patch, delete | CNPG Cluster `<name>-db`; patch: storage growth; delete: only the rollback of a failed PostgreSQL major upgrade (above) |
| `monitoring.coreos.com` | `servicemonitors`, `prometheusrules` | get, create, update, delete | Read live, never listed or watched |
| `gateway.networking.k8s.io` | `httproutes`, `tcproutes`, `referencegrants` | get, create, update, delete | Read live, never listed or watched (see [networking.md](networking.md#permissions)) |
| `cert-manager.io` | `certificates` | get, create, update, delete | Read live, never listed or watched (see [networking.md](networking.md#permissions)) |
| `forgejo.lucas.icu` | `forgejos` | get, list, watch, patch | patch: removes the `retry-upgrade` annotation |
| | `forgejobackups` | get, list, watch, create, update, patch, delete | Scheduled, pre-upgrade and pre-restore backups, retention (delete), the finalizer (update), owner references under `retainPolicy` (patch) |
| | `forgejorestores` | get, list, watch, create, update | Rollback restores and `spec.restoreFrom`; the finalizer |
| | `forgejorunners` | get, list, watch, patch | The finalizer |
| | `*/status` | patch | Status, always one merge patch with an optimistic lock |
| | `*/finalizers` | update | Owner references with `blockOwnerDeletion` on the children of each kind |

`internal/forgejo/rbac_test.go` fails when the markers grant a resource that is not classified
as namespaced or cluster-scoped, when `pods/exec` appears in manager-role, or when the
cluster-scoped rules and `cluster_reader_role.yaml` differ.

### Cluster-wide write access

With `config/default` the operator may create, change and delete Secrets, Deployments, Jobs,
Services and the other kinds above **in every namespace**, and delete pods there. That is what an
operator that serves instances in any namespace needs: RBAC can restrict a rule only by
resource name, not by label or name prefix, and the operator's objects are named after
instances it does not know in advance. What bounds it is the code: it writes only in the
namespace of the object it reconciles, changes or deletes only objects it controls (an object of
the same name that belongs to someone else is reported as a conflict, not taken over), and its
cache holds only labelled Secrets, Pods, Jobs, Deployments, Services and ConfigMaps. Whoever controls the operator's ServiceAccount (or
its image) can still read and write every Secret of the cluster. Where that is too much, use the
[namespaced install](#namespaced-install).

People who may edit a `Forgejo` direct some of that access: they decide which Secrets of their
namespace the instance reads (`configFrom`, `existingSecret`, S3 keys) and, through
`spec.tls.certManager`, which certificates are requested from which issuer
([networking.md](networking.md#permissions)).

## Volume measurement (pods/exec)

The storage autopilot measures the data and CNPG volumes with `df` and `du` inside the containers
that mount them ([autopilot.md](autopilot.md#storage)); nothing else execs into pods. Exec into
a pod is powerful (it reaches everything the container can), so it is kept out of manager-role
in ClusterRole `forgejo-operator-volume-usage`, and bound separately:

| Choice | How | Effect |
|--------|-----|--------|
| Everywhere (default) | `config/default` includes the component `config/components/volume-usage`, a ClusterRoleBinding | Volumes are measured and expanded in every namespace |
| Some namespaces | Remove `components: [../components/volume-usage]` from `config/default/kustomization.yaml`, and bind the ClusterRole in each namespace that wants it: `kubectl -n <ns> create rolebinding forgejo-operator-volume-usage --clusterrole=forgejo-operator-volume-usage --serviceaccount=forgejo-operator-system:forgejo-operator-controller-manager` | Measured only there |
| Nowhere | Remove the component and bind nothing | `StorageHealthy=Unknown/MeasurementUnavailable` on every instance (the message names the ClusterRole), no automatic expansion, `forgejo_operator_volume_used_ratio` not exported, so the `ForgejoVolumeAlmostFull` alert cannot fire; one log line per claim. Everything else works |

The operator notices a binding added later at its next attempt (failed attempts back off up to
an hour per claim; restart the operator to retry at once).

## Namespaced install

The overlay `config/namespaced` makes the operator manage only the namespaces it lists, with
its permissions confined to them:

- `WATCH_NAMESPACE` (on the manager Deployment) restricts the operator's caches and watches to
  those namespaces; instances elsewhere are ignored.
- manager-role is bound by a RoleBinding in each of those namespaces instead of the
  ClusterRoleBinding, and so is `forgejo-operator-volume-usage` (drop that RoleBinding to keep
  `pods/exec` out of a namespace).
- ClusterRole `forgejo-operator-cluster-reader` grants the cluster-scoped reads, which a
  RoleBinding cannot: `nodes`, `persistentvolumes` and `storageclasses` get, list, watch, and
  `namespaces` get.

To use it, edit `config/namespaced/kustomization.yaml` (`WATCH_NAMESPACE`, comma-separated) and
`config/namespaced/rolebindings.yaml` (two RoleBindings per namespace; the example uses
`forgejo`), create the namespaces, then:

```bash
make manifests
cd config/manager && ../../bin/kustomize edit set image controller=<registry>/forgejo-operator:<tag> && cd ../..
bin/kustomize build config/namespaced | kubectl apply -f -
```

Adding a namespace later means a new RoleBinding pair and a new `WATCH_NAMESPACE` (the operator
restarts).

What it does **not** cover:

- Within a watched namespace the operator keeps every verb of manager-role, Secrets included.
- The cluster-scoped reads above stay cluster-wide (read-only), and so do the leader-election
  Role (in the operator's namespace) and the metrics token and access reviews.
- The CRDs are cluster-scoped: `Forgejo` objects can still be created in any namespace; outside
  `WATCH_NAMESPACE` nothing reconciles them.
- It is one operator for a fixed set of namespaces, not one operator per tenant: two operators
  must not watch the same namespace, and the overlay's names (ClusterRoles, namespace
  `forgejo-operator-system`) are those of a single installation.
- Gateways and issuers outside the watched namespaces are still used: routes attach to Gateways
  in other namespaces, and Certificates may name any `ClusterIssuer` (see
  [networking.md](networking.md#permissions)).

## Checking a deployed operator

```bash
SA=system:serviceaccount:forgejo-operator-system:forgejo-operator-controller-manager
kubectl auth can-i --list --as=$SA -n <namespace>
kubectl auth can-i create pods --subresource=exec --as=$SA -n <namespace>   # yes only where volume-usage is bound
```
