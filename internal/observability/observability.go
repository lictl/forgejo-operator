// Package observability provides Level IV insights: ServiceMonitor, PrometheusRule and
// dashboard resources, operator-side health probing of Forgejo and statistics sampling.
package observability

import (
	"context"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileMonitoring ensures ServiceMonitor, PrometheusRule and dashboard ConfigMap.
func ReconcileMonitoring(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}

// ReconcileHealth probes /api/healthz and /api/v1/version and samples statistics into
// status.health, status.version, status.stats and the Healthy condition.
func ReconcileHealth(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	return forgejo.StepResult{}, nil
}
