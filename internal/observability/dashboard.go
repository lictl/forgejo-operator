package observability

import (
	"context"
	_ "embed"
	"strings"

	corev1 "k8s.io/api/core/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// dashboardJSON is a Grafana dashboard template. Placeholders are replaced per instance;
// queries use the hidden constant variables $namespace, $forgejo and $job.
//
//go:embed dashboard.json
var dashboardJSON string

const (
	// Labels/annotations understood by the Grafana dashboard sidecar (kube-prometheus-stack).
	dashboardLabel       = "grafana_dashboard"
	dashboardFolderAnnot = "grafana_folder"
	dashboardFolder      = "Forgejo"
)

func renderDashboard(f *forgejov1.Forgejo) string {
	return strings.NewReplacer(
		"__UID__", dashboardUID(f),
		"__TITLE__", "Forgejo / "+f.Namespace+"/"+f.Name,
		"__NAMESPACE__", f.Namespace,
		"__NAME__", f.Name,
		"__JOB__", forgejo.HTTPServiceName(f),
	).Replace(dashboardJSON)
}

func reconcileDashboard(ctx context.Context, rc *forgejo.ReconcileContext, want bool) error {
	f := rc.Instance
	cm := &corev1.ConfigMap{}
	cm.Namespace = f.Namespace
	cm.Name = forgejo.DashboardName(f)
	if !want {
		return deleteIfOwned(ctx, rc, cm)
	}
	if err := validateLabels(specLabels(f.Spec.Monitoring.GrafanaDashboard)); err != nil {
		return err
	}
	_, err := rc.CreateOrUpdate(ctx, cm, true, func() error {
		labels := objectLabels(f, specLabels(f.Spec.Monitoring.GrafanaDashboard))
		labels[dashboardLabel] = "1"
		cm.Labels = labels
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		if _, ok := cm.Annotations[dashboardFolderAnnot]; !ok {
			cm.Annotations[dashboardFolderAnnot] = dashboardFolder
		}
		cm.Data = map[string]string{
			"forgejo-" + f.Namespace + "-" + f.Name + ".json": renderDashboard(f),
		}
		return nil
	})
	return err
}
