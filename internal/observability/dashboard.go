package observability

import (
	"context"
	_ "embed"
	"encoding/json"
	"strconv"
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

// renderDashboard fills in the instance. Strings are placed into JSON strings of the
// template, escaped. Thresholds are JSON numbers: the template holds them as strings
// ("__SLO_OBJECTIVE__"), so that the template is valid JSON as well. Both backup age stats
// turn orange at one and a half schedule intervals and red from the RPO (two intervals) on,
// where the RPO alerts fire. An SLO the CRD would have refused falls back to the defaults.
func renderDashboard(f *forgejov1.Forgejo) string {
	s, err := sloOf(f)
	if err != nil {
		s, _ = sloOf(&forgejov1.Forgejo{})
	}
	rpo := int64(backupRPO(f).Seconds())
	return strings.NewReplacer(
		"__UID__", jsonText(dashboardUID(f)),
		"__TITLE__", jsonText("Forgejo / "+f.Namespace+"/"+f.Name),
		"__NAMESPACE__", jsonText(f.Namespace),
		"__NAME__", jsonText(f.Name),
		"__JOB__", jsonText(forgejo.HTTPServiceName(f)),
		"__SLO_WINDOW__", jsonText(s.windowName),
		"__RUNBOOKS__", jsonText(runbookBase(f)),
		`"__SLO_OBJECTIVE__"`, s.objective,
		`"__RPO_WARNING__"`, strconv.FormatInt(rpo*3/4, 10),
		`"__RPO__"`, strconv.FormatInt(rpo, 10),
	).Replace(dashboardJSON)
}

// jsonText is s escaped for the inside of a JSON string.
func jsonText(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	quoted := strings.TrimSuffix(b.String(), "\n")
	return quoted[1 : len(quoted)-1]
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
