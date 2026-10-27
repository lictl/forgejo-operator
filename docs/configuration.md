# Configuration map

The operator is configured at two levels. Helm `manager.*`, `rbac.*`, `metrics.*`,
`prometheus.*` and `networkPolicy.*` configure the controller deployment; read the
[chart values](../charts/forgejo-operator/values.yaml) and
[permissions](permissions.md). `rbac.volumeUsage.enabled=false` removes the optional
cluster-wide `pods/exec` binding.

A `Forgejo` custom resource configures one instance. Start with the
[small sample](../config/samples/forgejo_v1alpha1_forgejo.yaml); the full schema is
in [the generated CRD](../config/crd/bases/forgejo.lucas.icu_forgejos.yaml).
`spec.image` selects the **Forgejo server** image, not this controller's image.
Use `spec.database.cnpg` or an external database; use `spec.storage` for data
retention and size. For networking use [networking](networking.md), for backups
[backup and restore](backup-restore.md), and for runner configuration
[Actions runners](actions-runners.md). Do not put passwords in resource YAML: use
Kubernetes Secret references described in each topic.
