package forgejo

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// The API server applies most defaults through CRD markers. These helpers resolve the
// remaining optional pointers and guard against objects created before a default existed.

func BoolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func DurationOr(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

func Image(f *forgejov1.Forgejo) string {
	if f.Spec.Image != "" {
		return f.Spec.Image
	}
	return forgejov1.DefaultImage
}

func PostgresImage(f *forgejov1.Forgejo) string {
	if f.Spec.Database.CNPG.ImageName != "" {
		return f.Spec.Database.CNPG.ImageName
	}
	return forgejov1.DefaultPostgresImage
}

func DesiredReplicas(f *forgejov1.Forgejo) int32 {
	if f.Spec.Replicas == nil {
		return 1
	}
	return *f.Spec.Replicas
}

func DatabaseType(f *forgejov1.Forgejo) forgejov1.DatabaseType {
	if f.Spec.Database.Type == "" {
		return forgejov1.DatabaseTypeCNPG
	}
	return f.Spec.Database.Type
}

func SSHEnabled(f *forgejov1.Forgejo) bool { return BoolOr(f.Spec.Server.SSH.Enabled, true) }

func MonitoringEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.Monitoring.Enabled, true)
}

func SelfHealingEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.SelfHealing.Enabled, true)
}

func AutoTuningEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.AutoTuning.Enabled, true)
}

func StorageAutoExpandEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.StorageAutoExpand.Enabled, true)
}

func HorizontalAutoscalingEnabled(f *forgejov1.Forgejo) bool {
	h := f.Spec.Autoscaling.Horizontal
	return h != nil && h.Enabled && f.Spec.HighAvailability.Enabled
}

// HPAMinReplicas is the HPA's replica floor: minReplicas clamped to [1, maxReplicas].
func HPAMinReplicas(f *forgejov1.Forgejo) int32 {
	h := f.Spec.Autoscaling.Horizontal
	if h == nil {
		return 1
	}
	return min(max(h.MinReplicas, 1), max(h.MaxReplicas, 1))
}

func VerticalAutoscalingEnabled(f *forgejov1.Forgejo) bool {
	v := f.Spec.Autoscaling.Vertical
	return v != nil && v.Enabled
}

func BackupIncludeData(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.Backup.IncludeData, true)
}

func RetainData(f *forgejov1.Forgejo) bool {
	return f.Spec.Storage.RetainPolicy != forgejov1.RetainPolicyDelete
}

func QuantityOr(q resource.Quantity, def string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(def)
	}
	return q
}
