// Package upgrade orchestrates Forgejo image changes: version gating, pre-upgrade backup,
// rollout verification and automatic rollback.
package upgrade

import (
	"context"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Reconcile decides rc.TargetImage and rc.Upgrading.
func Reconcile(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	rc.TargetImage = forgejo.Image(rc.Instance)
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	return forgejo.StepResult{}, nil
}
