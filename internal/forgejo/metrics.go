package forgejo

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

const (
	labelName      = "name"
	labelNamespace = "namespace"
	labelResult    = "result"
	labelMajor     = "major"
)

// Operator metrics, served on the manager's /metrics endpoint. Feature packages update
// them; the observability package may register additional collectors.
var (
	InstanceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_instance_ready",
		Help: "1 when the Forgejo instance is Ready.",
	}, []string{labelNamespace, labelName})

	InstanceInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_instance_info",
		Help: "Static information about a Forgejo instance (always 1).",
	}, []string{labelNamespace, labelName, "image", "version", "phase"})

	HealthCheckFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_health_check_failures_total",
		Help: "Failed /api/healthz probes performed by the operator.",
	}, []string{labelNamespace, labelName})

	HealthCheckUnreachable = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_health_check_unreachable_total",
		Help: "Health probes that never reached Forgejo (DNS, dial, reset, proxy replies); not counted as failures.",
	}, []string{labelNamespace, labelName})

	// AvailabilityProbes is the availability SLI: one sample per health-check interval,
	// also while no pod is Ready (see observability.sampleAvailability).
	AvailabilityProbes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_availability_probes_total",
		Help: "Availability samples, one per health-check interval: result is up (/api/healthz passed) or down " +
			"(failed, unreachable, or no Ready pod while replicas are desired); planned is true during restores, " +
			"orchestrated upgrades, spec.paused, zero desired replicas and before the instance was first available.",
	}, []string{labelNamespace, labelName, labelResult, "planned"})

	BackupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_last_success_timestamp_seconds",
		Help: "Unix time of the last successful backup.",
	}, []string{labelNamespace, labelName})

	BackupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_backups_total",
		Help: "Finished backups by result (succeeded/failed) and reason.",
	}, []string{labelNamespace, labelName, "reason", labelResult})

	BackupDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_duration_seconds",
		Help: "Duration of the last finished backup.",
	}, []string{labelNamespace, labelName})

	BackupSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_size_bytes",
		Help: "Size of the last successful backup by part (database/data).",
	}, []string{labelNamespace, labelName, "part"})

	BackupUploadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_backup_uploads_total",
		Help: "Finished uploads of backups to spec.backup.remote by result (succeeded/failed) and reason.",
	}, []string{labelNamespace, labelName, "reason", labelResult})

	BackupLastUploadSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_backup_last_upload_success_timestamp_seconds",
		Help: "Unix time of the last backup uploaded to spec.backup.remote and verified.",
	}, []string{labelNamespace, labelName})

	RestoreTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_restores_total",
		Help: "Finished restores by result.",
	}, []string{labelNamespace, labelName, labelResult})

	UpgradeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_upgrades_total",
		Help: "Finished upgrades by result (succeeded/failed/rolledback).",
	}, []string{labelNamespace, labelName, labelResult})

	AutoPilotActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_autopilot_actions_total",
		Help: "Autonomous actions taken by the operator, by type.",
	}, []string{labelNamespace, labelName, "action"})

	VolumeUsedRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_volume_used_ratio",
		Help: "Used/capacity ratio of PVCs belonging to the instance.",
	}, []string{labelNamespace, labelName, "claim"})

	Stats = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_stats",
		Help: "Forgejo statistics sampled by the operator (users, repositories, ...).",
	}, []string{labelNamespace, labelName, "kind"})

	VersionSupported = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_version_supported",
		Help: "1 when the running Forgejo major is within the operator's support policy, else 0; " +
			"support is Supported, Deprecated, EndOfLife, Future or Unknown.",
	}, []string{labelNamespace, labelName, labelMajor, "support"})

	VersionEndOfLife = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_version_end_of_life_timestamp_seconds",
		Help: "Unix time of the upstream end of life of the running Forgejo major (when it is in the operator's version table).",
	}, []string{labelNamespace, labelName, labelMajor})

	ValkeyMasterAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_valkey_master_available",
		Help: "1 when Valkey has a primary Forgejo can use (ValkeyReady is True), else 0. Only exported while highAvailability is enabled.",
	}, []string{labelNamespace, labelName})

	ValkeyReadyMembers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_valkey_ready_members",
		Help: "Ready pods of the Valkey replication group (3 when healthy). Only exported while highAvailability is enabled.",
	}, []string{labelNamespace, labelName})

	ValkeyFailovers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgejo_operator_valkey_failovers_total",
		Help: "Changes of the Valkey primary seen by the operator (Sentinel failovers).",
	}, []string{labelNamespace, labelName})

	CronRunnerReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_cron_runner_ready",
		Help: "1 when the cron runner pod (Deployment <name>-cron) is Ready, else 0. Only exported while it should run.",
	}, []string{labelNamespace, labelName})

	VersionAdvisory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgejo_operator_version_advisory",
		Help: "1 when the running Forgejo patch release is older than the one an advisory of the operator's version table " +
			"recommends (e.g. missing security fixes), else 0.",
	}, []string{labelNamespace, labelName, labelMajor})
)

func init() {
	metrics.Registry.MustRegister(
		InstanceReady, InstanceInfo, HealthCheckFailures, HealthCheckUnreachable, AvailabilityProbes,
		BackupLastSuccess, BackupTotal, BackupDuration, BackupSize, BackupUploadTotal, BackupLastUploadSuccess, RestoreTotal,
		UpgradeTotal, AutoPilotActions, VolumeUsedRatio, Stats, VersionSupported, VersionEndOfLife, VersionAdvisory,
		ValkeyMasterAvailable, ValkeyReadyMembers, ValkeyFailovers, CronRunnerReady,
	)
}

// DeleteInstanceMetrics removes all series of a deleted instance.
func DeleteInstanceMetrics(namespace, name string) {
	l := prometheus.Labels{labelNamespace: namespace, labelName: name}
	for _, v := range []*prometheus.GaugeVec{InstanceReady, InstanceInfo, BackupLastSuccess, BackupLastUploadSuccess, BackupDuration, BackupSize,
		VolumeUsedRatio, Stats, VersionSupported, VersionEndOfLife, VersionAdvisory, ValkeyMasterAvailable, ValkeyReadyMembers,
		CronRunnerReady} {
		v.DeletePartialMatch(l)
	}
	for _, v := range []*prometheus.CounterVec{HealthCheckFailures, HealthCheckUnreachable, AvailabilityProbes, BackupTotal, BackupUploadTotal, RestoreTotal, UpgradeTotal,
		AutoPilotActions, ValkeyFailovers} {
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
	InstanceInfo.DeletePartialMatch(prometheus.Labels{labelNamespace: f.Namespace, labelName: f.Name})
	InstanceInfo.WithLabelValues(f.Namespace, f.Name, image, f.Status.Version, string(f.Status.Phase)).Set(1)
}
