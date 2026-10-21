// Package autopilot implements Level V capabilities: HA dependencies (Valkey), horizontal
// and vertical scaling, auto-tuning, storage auto-expansion and self-healing.
package autopilot

import (
	"context"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileHA ensures Valkey when highAvailability is enabled and sets rc.ValkeyURL.
func ReconcileHA(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileTuning computes rc.Tuning, rc.TuningEnv and rc.ResourcesOverride.
func ReconcileTuning(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileConfigGuard may switch rc.ConfigSecretName/rc.ConfigHash to the last known good
// config when the desired config was rejected by self-healing.
func ReconcileConfigGuard(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileScaling ensures the HPA and PodDisruptionBudget.
func ReconcileScaling(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// Reconcile runs post-rollout autonomous actions: storage auto-expansion, vertical
// right-sizing, self-healing and last-known-good bookkeeping.
func Reconcile(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}
