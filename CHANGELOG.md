# Changelog

Operator versions follow semantic versions; the Forgejo server version table
is maintained separately in `docs/versions.md`.

## Unreleased

## v0.1.0-rc.1

First public release candidate of forgejo-operator. This is a prerelease;
validate the installation and operational requirements before production use.

### Operator capabilities

- Manage a Forgejo installation with CloudNativePG databases, Valkey Sentinel,
  Gateway API routes, TLS, storage, backups, restores, and version-gated upgrades.
- Provide availability and recovery-point metrics, alert rules, dashboards,
  and runbooks, plus a declarative Forgejo Actions runner resource.

### Distribution

- Publish a multi-architecture controller image for linux/amd64 and linux/arm64
  and an OCI Helm Chart from independently built GitHub and Forgejo workflows.
- Include public installation, configuration, operations, troubleshooting,
  development, and contribution documentation. The operator's own code is MIT
  licensed, with required third-party notices retained.
