package core

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	containerName     = "forgejo"
	dataVolume        = "data"
	configVolume      = "config"
	tmpVolume         = "tmp"
	healthzPath       = "/api/healthz"
	revisionHistory   = 5
	progressDeadline  = 600
	terminationPeriod = 60
)

// ReconcileWorkload ensures the Forgejo Deployment.
func ReconcileWorkload(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if rc.ConfigHash == "" {
		return forgejo.StepResult{Stop: true, RequeueAfter: dbNotReadyRequeue}, nil
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: f.Namespace}}
	err := apply(ctx, rc, dep, true, func() error {
		exists := dep.ResourceVersion != ""
		restartedAt := dep.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]

		dep.Labels = mergeMaps(dep.Labels, podLabels(rc, nil))
		dep.Spec.Replicas = ptr.To(desiredReplicas(rc, exists, dep.Spec.Replicas))
		if !exists {
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: forgejo.SelectorLabels(f)}
		}
		dep.Spec.Strategy = deploymentStrategy(rc)
		dep.Spec.RevisionHistoryLimit = ptr.To[int32](revisionHistory)
		dep.Spec.ProgressDeadlineSeconds = ptr.To[int32](progressDeadline)

		annotations := mergeMaps(f.Spec.PodTemplate.Annotations, map[string]string{forgejo.AnnotationConfigHash: rc.ConfigHash})
		if restartedAt != "" {
			annotations[forgejo.AnnotationRestartedAt] = restartedAt
		}
		dep.Spec.Template.ObjectMeta = metav1.ObjectMeta{
			Labels:      podLabels(rc, f.Spec.PodTemplate.Labels),
			Annotations: annotations,
		}
		dep.Spec.Template.Spec = podSpec(rc)
		return nil
	})
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("deployment: %w", err)
	}
	f.Status.ConfigHash = rc.ConfigHash
	return forgejo.StepResult{}, nil
}

// desiredReplicas: 0 in maintenance, an explicit override, 1 while upgrading or before
// the first successful rollout (migrations run with a single pod), the HPA's choice when
// it owns scaling, else spec.replicas.
func desiredReplicas(rc *forgejo.ReconcileContext, exists bool, current *int32) int32 {
	switch {
	case rc.Maintenance:
		return 0
	case rc.ReplicasOverride != nil:
		return *rc.ReplicasOverride
	case rc.Upgrading || rc.Instance.Status.Image == "":
		return 1
	case rc.HPAManaged && exists && current != nil:
		return *current
	}
	return forgejo.DesiredReplicas(rc.Instance)
}

func deploymentStrategy(rc *forgejo.ReconcileContext) appsv1.DeploymentStrategy {
	if rc.Instance.Spec.HighAvailability.Enabled && !rc.Upgrading {
		zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
		return appsv1.DeploymentStrategy{
			Type:          appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one},
		}
	}
	return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
}

func podLabels(rc *forgejo.ReconcileContext, extra map[string]string) map[string]string {
	l := mergeMaps(extra, forgejo.Labels(rc.Instance, forgejo.ComponentServer))
	if tag := forgejo.ImageTag(rc.TargetImage); tag != "" && len(validation.IsValidLabelValue(tag)) == 0 {
		l[forgejo.LabelVersion] = tag
	}
	return l
}

// podSpec builds the pod per docs/design.md "Pod layout". Fields the API server would
// default are set explicitly so that unchanged specs compare equal and cause no update.
func podSpec(rc *forgejo.ReconcileContext) corev1.PodSpec {
	f, l := rc.Instance, rc.Layout
	pt := f.Spec.PodTemplate
	resources := f.Spec.Resources
	if rc.ResourcesOverride != nil {
		resources = *rc.ResourcesOverride
	}
	pullPolicy := imagePullPolicy(f.Spec.ImagePullPolicy, rc.TargetImage)
	mounts := []corev1.VolumeMount{
		{Name: dataVolume, MountPath: l.DataMountPath},
		{Name: configVolume, MountPath: l.ConfigDir, ReadOnly: true},
		{Name: tmpVolume, MountPath: l.TmpPath},
	}
	baseEnv := []corev1.EnvVar{
		{Name: "GITEA_WORK_DIR", Value: l.WorkPath},
		{Name: "GITEA_CUSTOM", Value: l.CustomPath},
		{Name: "GITEA_APP_INI", Value: l.ConfigPath},
		{Name: "HOME", Value: l.HomePath},
		{Name: "USER", Value: l.RunUser},
		{Name: "TMPDIR", Value: l.TmpPath},
		{Name: "GITEA_TEMP", Value: l.TmpPath},
	}

	ports := []corev1.ContainerPort{{Name: "http", ContainerPort: l.HTTPContainerPort, Protocol: corev1.ProtocolTCP}}
	if forgejo.SSHEnabled(f) {
		ports = append(ports, corev1.ContainerPort{Name: "ssh", ContainerPort: l.SSHContainerPort, Protocol: corev1.ProtocolTCP})
	}
	healthz := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: healthzPath, Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP,
	}}

	main := corev1.Container{
		Name:            containerName,
		Image:           rc.TargetImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{l.CLI, "web", "--config", l.ConfigPath},
		Env:             dedupeEnv(baseEnv, rc.TuningEnv, f.Spec.Env),
		Ports:           ports,
		Resources:       resources,
		VolumeMounts:    mounts,
		StartupProbe: &corev1.Probe{
			ProbeHandler: healthz, PeriodSeconds: 10, TimeoutSeconds: 5, SuccessThreshold: 1, FailureThreshold: 30,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: healthz, PeriodSeconds: 10, TimeoutSeconds: 5, SuccessThreshold: 1, FailureThreshold: 3,
		},
		// healthz also pings the database and cache; a TCP check avoids restarting every
		// pod during a PostgreSQL failover or a Valkey blip.
		LivenessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")}},
			PeriodSeconds: 10, TimeoutSeconds: 3, SuccessThreshold: 1, FailureThreshold: 6,
		},
		SecurityContext:          nonRootSecurityContext(l),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}

	initPerms := corev1.Container{
		Name:            "init-permissions",
		Image:           rc.TargetImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/bin/sh", "-c", initPermissionsScript(l)},
		Resources:       resources,
		VolumeMounts:    []corev1.VolumeMount{{Name: dataVolume, MountPath: l.DataMountPath}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To[int64](0),
			RunAsGroup:               ptr.To[int64](0),
			RunAsNonRoot:             ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"},
			},
		},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}

	bootstrapEnv := append(dedupeEnv(baseEnv, f.Spec.Env),
		corev1.EnvVar{Name: "ADMIN_USERNAME", Value: AdminUsername(f)},
		corev1.EnvVar{Name: "ADMIN_EMAIL", Value: adminEmail(f)},
		corev1.EnvVar{Name: "ADMIN_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretName(f)},
			Key:                  AdminPasswordKey(f),
		}}},
	)
	bootstrap := corev1.Container{
		Name:                     "bootstrap",
		Image:                    rc.TargetImage,
		ImagePullPolicy:          pullPolicy,
		Command:                  []string{"/bin/sh", "-c", bootstrapScript(l)},
		Env:                      bootstrapEnv,
		Resources:                resources,
		VolumeMounts:             mounts,
		SecurityContext:          nonRootSecurityContext(l),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}

	claim := f.Spec.Storage.ExistingClaim
	if claim == "" {
		claim = forgejo.DataPVCName(f)
	}
	return corev1.PodSpec{
		InitContainers: []corev1.Container{initPerms, bootstrap},
		Containers:     []corev1.Container{main},
		Volumes: []corev1.Volume{
			{Name: dataVolume, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}},
			{Name: configVolume, VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: rc.ConfigSecretName, DefaultMode: ptr.To[int32](0o440)},
			}},
			{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup:             ptr.To(l.GID),
			FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		ImagePullSecrets:              f.Spec.ImagePullSecrets,
		ServiceAccountName:            pt.ServiceAccountName,
		DeprecatedServiceAccount:      pt.ServiceAccountName,
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		NodeSelector:                  pt.NodeSelector,
		Tolerations:                   pt.Tolerations,
		Affinity:                      pt.Affinity,
		TopologySpreadConstraints:     pt.TopologySpreadConstraints,
		PriorityClassName:             pt.PriorityClassName,
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		TerminationGracePeriodSeconds: ptr.To[int64](terminationPeriod),
	}
}

func adminSecretName(f *forgejov1.Forgejo) string {
	if f.Spec.Admin.ExistingSecret != "" {
		return f.Spec.Admin.ExistingSecret
	}
	return forgejo.AdminSecretName(f)
}

func nonRootSecurityContext(l forgejo.ImageLayout) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptr.To(l.UID),
		RunAsGroup:               ptr.To(l.GID),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// initPermissionsScript creates the top-level data directories owned by the Forgejo
// user. It is not recursive, so it stays fast on large volumes.
func initPermissionsScript(l forgejo.ImageLayout) string {
	ssh := l.HomePath + "/.ssh"
	dirs := []string{l.DataMountPath, l.WorkPath, l.CustomPath, l.HomePath, ssh, l.RepoRoot, l.LFSPath}
	return fmt.Sprintf(`set -eu
for d in %s; do
  mkdir -p "$d"
  chown %d:%d "$d"
done
chmod 0700 %s
`, strings.Join(dirs, " "), l.UID, l.GID, ssh)
}

// bootstrapScript migrates the database, then creates the admin or resets its password so
// that the admin Secret stays authoritative. The password only travels through the env.
func bootstrapScript(l forgejo.ImageLayout) string {
	return fmt.Sprintf(`set -eu
F=%s
C=%s
"$F" migrate --config "$C"
users=$("$F" admin user list --config "$C")
if printf '%%s\n' "$users" | awk -v u="$ADMIN_USERNAME" 'NR > 1 && tolower($2) == tolower(u) { found = 1 } END { exit !found }'; then
  "$F" admin user change-password --config "$C" --username "$ADMIN_USERNAME" --password "$ADMIN_PASSWORD" --must-change-password=false
else
  "$F" admin user create --config "$C" --admin --username "$ADMIN_USERNAME" --password "$ADMIN_PASSWORD" --email "$ADMIN_EMAIL" --must-change-password=false
fi
`, l.CLI, l.ConfigPath)
}

// dedupeEnv concatenates env lists; a later variable replaces an earlier one with the
// same name in place.
func dedupeEnv(lists ...[]corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	index := map[string]int{}
	for _, list := range lists {
		for _, e := range list {
			if i, ok := index[e.Name]; ok {
				out[i] = e
				continue
			}
			index[e.Name] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// imagePullPolicy mirrors the API server default so that specs compare equal.
func imagePullPolicy(p corev1.PullPolicy, image string) corev1.PullPolicy {
	if p != "" {
		return p
	}
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	if tag := forgejo.ImageTag(image); tag == "" || tag == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}
