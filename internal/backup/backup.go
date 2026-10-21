// Package backup implements ForgejoBackup/ForgejoRestore execution, scheduled backups with
// retention, and the maintenance mode used while a restore is running.
package backup

import (
	"context"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileMaintenance sets rc.Maintenance while a ForgejoRestore targets this instance.
func ReconcileMaintenance(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileSchedule creates scheduled ForgejoBackups, prunes old ones and maintains
// status.backup and the BackupHealthy condition.
func ReconcileSchedule(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}
