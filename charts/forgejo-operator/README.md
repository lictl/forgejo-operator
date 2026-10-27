# forgejo-operator Helm chart

This chart installs the controller and its four CRDs. It does not install a
Forgejo instance or CloudNativePG. The [installation guide](https://github.com/lictl/forgejo-operator/blob/main/docs/installation.md)
lists prerequisites and a first instance example; the [configuration guide](https://github.com/lictl/forgejo-operator/blob/main/docs/configuration.md)
explains controller image, RBAC and optional integrations.

Once a version has been released:

```sh
helm upgrade --install forgejo-operator \
  oci://ghcr.io/lictl/charts/forgejo-operator \
  --version <chart-version> \
  --namespace forgejo-operator-system --create-namespace --wait
```

The alternate OCI registry is
`oci://git.lucas.icu/lictl/charts/forgejo-operator`. The default controller
image is `ghcr.io/lictl/forgejo-operator`; set `manager.image.repository` to
use its `git.lucas.icu/lictl/forgejo-operator` mirror.

The chart grants cluster-wide access to managed resources by default. Set
`rbac.volumeUsage.enabled=false` to omit the `pods/exec` binding used for
volume measurements. `rbac.namespaced=true` is rejected because the generated
manager role includes cluster-scoped resources. For a restricted installation,
use the [namespaced Kustomize overlay](https://github.com/lictl/forgejo-operator/blob/main/docs/permissions.md).

Helm installs CRDs on first install but does not upgrade or delete them.
Before upgrading, apply the `config/crd/bases/` manifests from the matching
release tag. Do not delete CRDs on uninstall unless you intend to delete the
corresponding custom resources.

The chart is covered by the accompanying [MIT license](LICENSE).
