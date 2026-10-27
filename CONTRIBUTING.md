# Contributing

Open an issue for behavior changes, especially API or permission changes.
Use a focused pull request against `main` with the affected CRD kind, an
example, and the checks you ran. For Go changes run `make lint test`; for
API markers run `make manifests generate` and update the Chart CRD copies.
For alert changes run `make test-rules`; for Chart changes run `helm lint
--strict charts/forgejo-operator` and render the chart. E2E must use a
dedicated Kind cluster. Never commit credentials, kubeconfigs, private logs
or production manifests. Existing file license notices must be retained.
