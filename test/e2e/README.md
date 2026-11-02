# Release lifecycle E2E

Run `make test-e2e` with Docker, Kind, kubectl and Git installed. The runner
creates a fresh Kind cluster with its own kubeconfig, refuses an existing cluster
of the same name, and deletes its cluster on success or failure. It never merges
into the caller's kubeconfig. `KIND`, `KIND_CLUSTER` and `KIND_NODE_IMAGE` can select
the binary, disposable cluster name and node image. Do not run the Go suite
directly against an existing cluster.

GitHub CI runs both Kubernetes 1.30.10 (the documented minimum minor) and 1.37.0
using digest-pinned node images. The suite builds and loads the exact checkout's
manager image. No cert-manager, webhooks or other optional operators are installed.
The lifecycle fixture uses a single disposable PostgreSQL 17 database via
`spec.database.type: External` and the Kind storage provisioner.

Besides manager readiness and authenticated metrics, the lifecycle test verifies:

- Forgejo 15 creates a real repository commit and issue; a Git clone reads the file.
- A manual backup includes both the database and data volume. Restoring it removes
  subsequent repository and issue changes, and the original content is cloned again.
- A major upgrade from Forgejo 15 to 17 completes with a cold safety backup and
  preserves the repository and issue.
- An unavailable newer image triggers automatic rollback. Restarting the manager
  during that rollout preserves the upgrade identity and backup name; its completed
  restore recovers the previous image and content.
- Scheduled retention prunes the oldest scheduled backup, keeping the newest,
  manual and pre-upgrade backups.
- Deleting and recreating a `retainPolicy: Retain` instance preserves the data PVC
  and backup PVC UIDs, identity and admin Secret UIDs and contents, and both kinds
  of data. A second restore proves the retained archive is still usable after recreation.

Lifecycle waits and subprocesses have deadlines; the whole Go suite is limited to
45 minutes. A failure prints resource summaries and manager logs, and the runner
exports Kind diagnostics before deleting its cluster. After successful cluster
deletion, kubeconfigs are removed even when tests failed; failure diagnostics stay
in the printed temporary folder locally. If cluster deletion fails, the runner
returns a failure (preserving an earlier test failure), keeps the private kubeconfig
and diagnostics, and prints a retry command naming the cluster. They may contain fixture credentials in process logs, so review
and redact them before sharing. The suite never uses production credentials.

These tests are a release gate, not a claim that every supported feature is covered.
The [checklists](checklists/capability-levels.md) still cover CNPG installation and
PostgreSQL major upgrades, HA, SSH, remote/object storage disaster recovery, runners,
Gateway/Ingress/TLS, observability and resource tuning. Those integrations need their
own local-cluster acceptance evidence before advertising them for a release.
