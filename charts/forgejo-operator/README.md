# forgejo-operator chart

Installs the controller and four CRDs. CloudNativePG must be installed separately before
creating a `Forgejo` resource. The default controller has cluster-wide privileges,
including access to Secrets; review [permissions](../../docs/permissions.md).

```bash
helm upgrade --install forgejo-operator . -n forgejo-operator-system --create-namespace --wait
```

Set `manager.image.repository` and `manager.image.tag` to use a mirror. Set
`rbac.volumeUsage.enabled=false` to omit the cluster-wide `pods/exec` binding.
The chart does not support namespace-scoped RBAC; use `config/namespaced` for that.
CRDs install from `crds/` once. Before upgrades, apply matching CRDs from the new
source tag because Helm does not upgrade them. Helm uninstall leaves CRDs and user
resources in place. See [installation](../../docs/installation.md).
