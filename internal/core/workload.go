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
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	restore, err := migratingRestore(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: f.Namespace}}
	err = apply(ctx, rc, dep, true, func() error {
		exists := dep.ResourceVersion != ""
		restartedAt := dep.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]
		hold := migrationHold(dep, restore)

		dep.Labels = mergeMaps(dep.Labels, podLabels(rc, nil))
		if hold != "" {
			dep.Annotations = mergeMaps(dep.Annotations, map[string]string{annotationMigrationHold: hold})
		} else {
			delete(dep.Annotations, annotationMigrationHold)
		}
		dep.Spec.Replicas = ptr.To(desiredReplicas(rc, exists, dep.Spec.Replicas, hold != ""))
		if !exists {
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: forgejo.SelectorLabels(f)}
		}
		dep.Spec.Strategy = deploymentStrategy(rc, hold != "")
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

// desiredReplicas: 0 in maintenance, an explicit override, 1 while upgrading, before the
// first successful rollout or while a restored database is migrated (migrations run with a
// single pod), the HPA's choice when it owns scaling, else spec.replicas.
func desiredReplicas(rc *forgejo.ReconcileContext, exists bool, current *int32, migrationHold bool) int32 {
	switch {
	case rc.Maintenance:
		return 0
	case rc.ReplicasOverride != nil:
		return *rc.ReplicasOverride
	case rc.Upgrading || rc.Instance.Status.Image == "":
		return 1
	case rc.HPAManaged:
		if exists && current != nil && *current > 0 {
			return *current
		}
		// An HPA never scales a target at 0 replicas (ScalingDisabled). Maintenance, a cold
		// pre-upgrade backup or a stopped rollback leave the Deployment at 0, so hand the
		// HPA a non-zero starting point; it raises the count to its minimum itself.
		if migrationHold {
			return 1
		}
		return hpaMinReplicas(rc.Instance)
	case migrationHold:
		return min(forgejo.DesiredReplicas(rc.Instance), 1)
	}
	return forgejo.DesiredReplicas(rc.Instance)
}

// hpaMinReplicas is the HPA's replica floor, clamped exactly like autopilot.ensureHPA.
func hpaMinReplicas(f *forgejov1.Forgejo) int32 {
	h := f.Spec.Autoscaling.Horizontal
	if h == nil {
		return 1
	}
	maxR := max(h.MaxReplicas, 1)
	return min(max(h.MinReplicas, 1), maxR)
}

// deploymentStrategy rolls HA instances without downtime, except while a migration must run
// in a single pod (upgrade or restored database), where no surge pod may start next to it.
func deploymentStrategy(rc *forgejo.ReconcileContext, migrationHold bool) appsv1.DeploymentStrategy {
	if rc.Instance.Spec.HighAvailability.Enabled && !rc.Upgrading && !migrationHold {
		zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
		return appsv1.DeploymentStrategy{
			Type:          appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one},
		}
	}
	return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
}

// annotationMigrationHold on the Deployment names the ForgejoRestore whose database the
// bootstrap init container is migrating. While it is set the Deployment runs one pod, so
// "forgejo migrate" (which takes no lock) never runs in several pods at once.
const annotationMigrationHold = "forgejo.lucas.icu/migration-hold"

// migratingRestore returns the name of a ForgejoRestore of this instance that restored the
// database and is now waiting for Forgejo to come back (phase ScalingUp). The restored
// schema may be older than the running image, so the first pod migrates it.
func migratingRestore(ctx context.Context, rc *forgejo.ReconcileContext) (string, error) {
	f := rc.Instance
	list := &forgejov1.ForgejoRestoreList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace)); err != nil {
		return "", fmt.Errorf("listing restores: %w", err)
	}
	for _, r := range list.Items {
		if r.Spec.ForgejoRef.Name == f.Name && r.Status.Phase == forgejov1.RestorePhaseScalingUp &&
			forgejo.BoolOr(r.Spec.RestoreDatabase, true) {
			return r.Name, nil
		}
	}
	return "", nil
}

// migrationHold decides the hold for the live Deployment: set while a restore is migrating,
// and kept after it (a restore gives up waiting after a timeout) until a pod of the current
// template is available, i.e. its bootstrap migration has finished.
func migrationHold(dep *appsv1.Deployment, restore string) string {
	if restore != "" {
		return restore
	}
	held := dep.Annotations[annotationMigrationHold]
	if held == "" || singlePodAvailable(dep) {
		return ""
	}
	return held
}

// singlePodAvailable reports whether the Deployment's current template has an available pod
// and no pods of older templates remain.
func singlePodAvailable(dep *appsv1.Deployment) bool {
	s := dep.Status
	return dep.Spec.Replicas != nil && *dep.Spec.Replicas > 0 && s.ObservedGeneration >= dep.Generation &&
		s.UpdatedReplicas > 0 && s.AvailableReplicas > 0 && s.Replicas == s.UpdatedReplicas
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
		corev1.EnvVar{Name: "ADMIN_SYNC_SALT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: forgejo.SecretsName(f)},
			Key:                  secretKeyAdminSyncSalt,
			Optional:             ptr.To(true),
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

// adminSyncMarker (on the data volume) holds a salted digest of the admin credentials last
// written to Forgejo, so the password is only reset when the admin Secret changes.
const adminSyncMarker = ".admin-password-sync"

// bootstrapScript migrates the database, then creates the admin or, when the admin Secret
// changed since the last sync, resets its password so that the Secret stays authoritative.
// The password only travels through the env. A failed migration is fatal; a failed admin
// sync (e.g. a password that does not meet PASSWORD_COMPLEXITY / MIN_PASSWORD_LENGTH /
// PASSWORD_CHECK_PWN) only logs a warning, so it can never keep Forgejo from starting.
func bootstrapScript(l forgejo.ImageLayout) string {
	return fmt.Sprintf(`set -eu
F=%s
C=%s
M=%s
"$F" migrate --config "$C"
want=""
if [ -n "${ADMIN_SYNC_SALT:-}" ]; then
  want=$(printf '%%s\n%%s\n%%s' "$ADMIN_SYNC_SALT" "$ADMIN_USERNAME" "$ADMIN_PASSWORD" | sha256sum | cut -d' ' -f1)
fi
mark() {
  if [ -n "$want" ]; then printf '%%s\n' "$want" > "$M" || true; fi
}
users=$("$F" admin user list --config "$C")
if printf '%%s\n' "$users" | awk -v u="$ADMIN_USERNAME" 'NR > 1 && tolower($2) == tolower(u) { found = 1 } END { exit !found }'; then
  if [ -n "$want" ] && [ -f "$M" ] && [ "$(cat "$M")" = "$want" ]; then
    echo "admin password is in sync with the Secret"
  elif "$F" admin user change-password --config "$C" --username "$ADMIN_USERNAME" --password="$ADMIN_PASSWORD" --must-change-password=false; then
    mark
  else
    echo "WARNING: could not set the admin password from the Secret (password policy?); keeping the current password" >&2
  fi
elif "$F" admin user create --config "$C" --admin --username "$ADMIN_USERNAME" --password="$ADMIN_PASSWORD" --email "$ADMIN_EMAIL" --must-change-password=false; then
  mark
else
  echo "WARNING: could not create the admin user $ADMIN_USERNAME" >&2
fi
`, l.CLI, l.ConfigPath, l.WorkPath+"/"+adminSyncMarker)
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
