// Package core reconciles the Level I/II building blocks of a Forgejo instance:
// generated secrets, the database (CloudNativePG or external), the data PVC, app.ini,
// the Deployment, Services/Ingress and the summarized status.
package core

import (
	"context"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileSecrets ensures "<name>-secrets" (SECRET_KEY, INTERNAL_TOKEN, JWT secrets,
// metrics token) and the admin credentials secret exist. Never rotates existing values.
func ReconcileSecrets(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileDatabase ensures the CNPG Cluster (or validates the external DB secret) and
// sets rc.DB / rc.DatabaseReady and status.database. Stops the reconcile until ready.
func ReconcileDatabase(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileStorage ensures the data PVC.
func ReconcileStorage(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileConfig renders app.ini into "<name>-config" and sets rc.ConfigSecretName,
// rc.ConfigHash and rc.DesiredConfigHash.
func ReconcileConfig(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileWorkload ensures the Forgejo Deployment.
func ReconcileWorkload(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileNetwork ensures the HTTP/SSH Services and the Ingress.
func ReconcileNetwork(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileStatus summarizes phase, Ready/Progressing conditions, replicas and URLs.
// It always runs last, even when an earlier step stopped the reconcile.
func ReconcileStatus(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}
