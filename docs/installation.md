# Installation

No operator version has been tagged yet. Use a published version in these commands
once the first release appears; do not substitute an unverified `latest` tag.

The operator needs Kubernetes 1.30 or newer and a CloudNativePG installation for
operator-managed PostgreSQL. Check the [version policy](versions.md) and your
cluster's API compatibility. Install CloudNativePG following its upstream instructions
before creating the example instance. cert-manager, Gateway API, metrics-server and
Prometheus Operator are needed only for the corresponding optional features.

```bash
helm upgrade --install forgejo-operator \
  oci://ghcr.io/lictl/charts/forgejo-operator \
  --version <chart-version> -n forgejo-operator-system --create-namespace --wait
kubectl create namespace forgejo
kubectl apply -n forgejo -f config/samples/forgejo_v1alpha1_forgejo.yaml
kubectl get forgejos.forgejo.lucas.icu -n forgejo
```

Create the `forgejo` namespace first. The image mirror is
`git.lucas.icu/lictl/forgejo-operator`; the chart mirror is
`oci://git.lucas.icu/lictl/charts/forgejo-operator`. Set
`manager.image.repository` to select the image mirror. Both OCI packages must
be publicly readable before anonymous installation is advertised.

Default RBAC is cluster-wide and can access Secrets; see [permissions](permissions.md).
For selected namespaces use the Kustomize `config/namespaced` overlay, which narrows
both cache and permissions. Merely setting `WATCH_NAMESPACE` on the Helm Chart does not
narrow its RBAC.

Helm installs CRDs on first installation but does not upgrade or delete them.
Before a chart upgrade apply `config/crd/bases/*.yaml` from the matching release
tag, check the API server accepts them, then upgrade the chart. Avoid deleting
CRDs during uninstall because that removes user resources.
