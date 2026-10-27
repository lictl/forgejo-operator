# Development

Use Go 1.26.9, Docker/Buildx, Helm and kubectl. Never run e2e against a production
context. Unit/envtest does not contact your cluster; `make run`, `make install`,
`make deploy` and e2e do. For e2e set a dedicated Kind cluster and kubeconfig.

```bash
make manifests generate
make lint-config lint test test-rules
helm lint --strict charts/forgejo-operator
helm template forgejo-operator charts/forgejo-operator --include-crds
```

Generated CRDs and DeepCopy files come from Kubebuilder; edit API markers and
regenerate rather than editing generated files. Chart CRDs are copies of the four
files in `config/crd/bases`; refresh them when API schemas change. See the
[repository guide](../AGENTS.md) for layout and [contributing](../CONTRIBUTING.md)
for review expectations. `go.mod` pins the toolchain because Go 1.26.0 has a
net/url bug affecting the multi-host Sentinel URLs.
