package forgejo

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// Operator metrics, served on the manager's /metrics endpoint. Feature packages update
// them; the observability package may register additional collectors.
var (
	InstanceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_instance_ready",
		Help: "1 when the Forgejo instance is Ready.",
	}, []string{"namespace", "name"})

	InstanceInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_instance_info",
		Help: "Static information about a Forgejo instance (always 1).",
	}, []string{"namespace", "name", "image", "version", "phase"})

	HealthCheckFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_health_check_failures_total",
		Help: "Failed /api/healthz probes performed by the operator.",
	}, []string{"namespace", "name"})

	HealthCheckUnreachable = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_health_check_unreachable_total",
		Help: "Health probes that never reached Forgejo (DNS, dial, reset, proxy replies); not counted as failures.",
	}, []string{"namespace", "name"})

	BackupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_last_success_timestamp_seconds",
		Help: "Unix time of the last successful backup.",
	}, []string{"namespace", "name"})

	BackupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_backups_total",
		Help: "Finished backups by result (succeeded/failed) and reason.",
	}, []string{"namespace", "name", "reason", "result"})

	BackupDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_duration_seconds",
		Help: "Duration of the last finished backup.",
	}, []string{"namespace", "name"})

	BackupSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_size_bytes",
		Help: "Size of the last successful backup by part (database/data).",
	}, []string{"namespace", "name", "part"})

	RestoreTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_restores_total",
		Help: "Finished restores by result.",
	}, []string{"namespace", "name", "result"})

	UpgradeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_upgrades_total",
		Help: "Finished upgrades by result (succeeded/failed/rolledback).",
	}, []string{"namespace", "name", "result"})

	AutoPilotActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_autopilot_actions_total",
		Help: "Autonomous actions taken by the operator, by type.",
	}, []string{"namespace", "name", "action"})

	VolumeUsedRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_volume_used_ratio",
		Help: "Used/capacity ratio of PVCs belonging to the instance.",
	}, []string{"namespace", "name", "claim"})

	Stats = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_stats",
		Help: "Forgejo statistics sampled by the operator (users, repositories, ...).",
	}, []string{"namespace", "name", "kind"})
)

func init() {
	metrics.Registry.MustRegister(
		InstanceReady, InstanceInfo, HealthCheckFailures, HealthCheckUnreachable,
		BackupLastSuccess, BackupTotal, BackupDuration, BackupSize, RestoreTotal,
		UpgradeTotal, AutoPilotActions, VolumeUsedRatio, Stats,
	)
}

// DeleteInstanceMetrics removes all series of a deleted instance.
func DeleteInstanceMetrics(namespace, name string) {
	l := prometheus.Labels{"namespace": namespace, "name": name}
	for _, v := range []*prometheus.GaugeVec{InstanceReady, InstanceInfo, BackupLastSuccess, BackupDuration, BackupSize, VolumeUsedRatio, Stats} {
		v.DeletePartialMatch(l)
	}
	for _, v := range []*prometheus.CounterVec{HealthCheckFailures, HealthCheckUnreachable, BackupTotal, RestoreTotal, UpgradeTotal, AutoPilotActions} {
		v.DeletePartialMatch(l)
	}
}

// SetInstanceInfo exports forgejo_operator_instance_info for the instance's current
// status.phase, replacing the series of its previous phase/image/version. image is used
// when status.image is not known yet.
func SetInstanceInfo(f *forgejov1.Forgejo, image string) {
	if f.Status.Image != "" {
		image = f.Status.Image
	}
	InstanceInfo.DeletePartialMatch(prometheus.Labels{"namespace": f.Namespace, "name": f.Name})
	InstanceInfo.WithLabelValues(f.Namespace, f.Name, image, f.Status.Version, string(f.Status.Phase)).Set(1)
}
