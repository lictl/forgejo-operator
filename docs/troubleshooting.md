# Troubleshooting

Check the controller first:

```bash
kubectl -n forgejo-operator-system get deploy,pods
kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager -c manager --tail=200
kubectl -n forgejo get forgejos,forgejobackups,forgejorestores,forgejorunners
kubectl -n forgejo describe forgejo <name>
```

`Ready=False` on the custom resource gives the failing condition and recent events.
If the controller cannot list a dependent API, install that optional CRD or disable
its matching feature; check [installation](installation.md) and
[permissions](permissions.md). A database or pod failure has focused checks in
[backup/restore](backup-restore.md), [upgrades](upgrades.md), and
[high availability](high-availability.md). Alert-specific recovery steps are in
[runbooks](runbooks/README.md). Do not paste Secret values into public issues;
redact logs and manifests before sharing.

## Broken instances

Phase `Broken` (condition `Broken=True`) means the operator found something it does not repair:
an identity Secret, the data volume or the CNPG cluster of an instance that has run is missing or
belongs to another instance, or a PostgreSQL major upgrade failed after it may have changed the
data. It then changes nothing about the instance until a person restores what is missing; pods
that still run keep serving, and scheduled backups and retention wait so that the backups from
before the incident are kept. The condition's message says what is missing and how to restore it;
the [ForgejoBroken](runbooks/ForgejoBroken.md) runbook has the details for every reason.
