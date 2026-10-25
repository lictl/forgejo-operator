package runner

import (
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	dockerCLI  = "docker"
	capDropAll = "ALL"
	// dindCommand prepares cgroups for a nested daemon, then runs its arguments (the docker:dind image).
	dindCommand = "dind"
)

// podMTUScript starts dockerd ("$@") with the MTU of the pod's network interface on the networks it
// creates: the default bridge (--mtu) and the network Forgejo Runner creates for every job
// (default-network-opt, Docker 24 and later). Docker uses 1500 otherwise, larger than many pod
// networks (1450 on VXLAN overlays such as flannel's, the k3s default), where the job containers'
// full-size packets are dropped and downloads stall. Without a readable MTU, dockerd starts as given.
const podMTUScript = `mtu=$(cat /sys/class/net/eth0/mtu 2>/dev/null)
case "$mtu" in
''|*[!0-9]*) exec "$@" ;;
esac
exec "$@" --mtu="$mtu" --default-network-opt=bridge=com.docker.network.driver.mtu="$mtu"`

// setsMTU reports whether spec.docker.args choose dockerd's MTU themselves.
func setsMTU(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--mtu") || strings.Contains(a, "com.docker.network.driver.mtu") {
			return true
		}
	}
	return false
}

// Container and volume names of the runner pods.
const (
	ContainerRunner  = "runner"
	ContainerDocker  = "docker"
	ContainerInstall = "install-runner"

	volConfig        = "config"
	volTokens        = "tokens"
	volData          = "data"
	volTmp           = "tmp"
	volCache         = "cache"
	volDockerSocket  = "docker-socket"
	volDockerStorage = "docker-storage"
	volRunnerBin     = "runner-bin"

	runnerBinDir = "/opt/forgejo-runner"
	// runnerBinary is the runner's path in the official image.
	runnerBinary = "/bin/forgejo-runner"
	dumbInit     = "/usr/bin/dumb-init"

	runnerUID int64 = 1000
	// podIndexLabel is set on every StatefulSet pod (Kubernetes 1.28 and later).
	podIndexLabel = "apps.kubernetes.io/pod-index"
	// gracePeriodMargin is added to the runner's shutdown timeout for the pod's grace period.
	gracePeriodMargin int64 = 30
)

// WorkloadInput is what the StatefulSet is rendered from.
type WorkloadInput struct {
	Runner *forgejov1.ForgejoRunner
	// Replicas is the number of pods: the runners that are registered.
	Replicas int32
	// ConfigHash is the digest of the configuration all runners share: a change rolls the
	// pods. Nothing per runner is on the pod template, so adding or removing a runner leaves
	// the other pods alone (a runner whose secret changes is outside the registered prefix,
	// so its pod is scaled away until it is registered again).
	ConfigHash string
}

// Image is the image the runner container runs.
func Image(r *forgejov1.ForgejoRunner) string {
	if Executor(r) == forgejov1.RunnerExecutorHost {
		return HostImage(r)
	}
	return RunnerImage(r)
}

// RunnerImage is spec.image with its default.
func RunnerImage(r *forgejov1.ForgejoRunner) string {
	if r.Spec.Image != "" {
		return r.Spec.Image
	}
	return forgejov1.DefaultRunnerImage
}

// HostImage is spec.host.image with its default.
func HostImage(r *forgejov1.ForgejoRunner) string {
	if r.Spec.Host.Image != "" {
		return r.Spec.Host.Image
	}
	return forgejov1.DefaultRunnerToolchainImage
}

// DindImage is spec.docker.image with its default.
func DindImage(r *forgejov1.ForgejoRunner) string {
	if r.Spec.Docker.Image != "" {
		return r.Spec.Docker.Image
	}
	return forgejov1.DefaultDindImage
}

// MutateStatefulSet renders the StatefulSet of the runner pods into sts (create or update).
// Fields the API server would default are set explicitly, so that an unchanged spec compares
// equal.
func MutateStatefulSet(sts *appsv1.StatefulSet, in WorkloadInput) {
	r := in.Runner
	exists := sts.ResourceVersion != ""
	sts.Labels = mergeLabels(sts.Labels, Labels(r, forgejo.ComponentRunner))
	sts.Spec.Replicas = ptr.To(in.Replicas)
	if !exists {
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: SelectorLabels(r)}
		sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	}
	sts.Spec.ServiceName = ""
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type:          appsv1.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
	}
	sts.Spec.RevisionHistoryLimit = ptr.To[int32](5)
	sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}

	annotations := maps.Clone(r.Spec.PodTemplate.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[forgejo.AnnotationConfigHash] = in.ConfigHash
	podLabels := mergeLabels(r.Spec.PodTemplate.Labels, Labels(r, forgejo.ComponentRunner))
	sts.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: annotations},
		Spec:       PodSpec(r),
	}
}

// PodSpec builds the runner pod (docs/actions-runners.md, "Pods").
func PodSpec(r *forgejov1.ForgejoRunner) corev1.PodSpec {
	pt := r.Spec.PodTemplate
	host := Executor(r) == forgejov1.RunnerExecutorHost
	configFile := ConfigDir + "/config-$(POD_INDEX).yaml"

	volumes := []corev1.Volume{
		{Name: volConfig, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: ConfigSecretName(r), DefaultMode: ptr.To[int32](0o440),
		}}},
		{Name: volTokens, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: TokensSecretName(r), DefaultMode: ptr.To[int32](0o440),
		}}},
		{Name: volData, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: volConfig, MountPath: ConfigDir, ReadOnly: true},
		{Name: volTokens, MountPath: TokensDir, ReadOnly: true},
		{Name: volData, MountPath: DataDir},
		{Name: volTmp, MountPath: "/tmp"},
	}
	if CacheEnabled(r) {
		volumes = append(volumes, corev1.Volume{Name: volCache, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: r.Spec.Cache.SizeLimit},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: volCache, MountPath: CacheDir})
	}
	env := []corev1.EnvVar{
		{Name: "POD_INDEX", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			APIVersion: "v1", FieldPath: "metadata.labels['" + podIndexLabel + "']",
		}}},
		{Name: "HOME", Value: DataDir},
		{Name: "TMPDIR", Value: "/tmp"},
	}

	runner := corev1.Container{
		Name:                     ContainerRunner,
		Image:                    Image(r),
		ImagePullPolicy:          pullPolicy(r.Spec.ImagePullPolicy, Image(r)),
		WorkingDir:               DataDir,
		Resources:                r.Spec.Resources,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	var initContainers []corev1.Container
	podSecurity := &corev1.PodSecurityContext{
		FSGroup:        ptr.To(runnerUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	shareProcessNamespace := (*bool)(nil)

	if host {
		// The toolchain image gets the runner binary from the runner image. The pause
		// container reaps the job processes the runner leaves behind (it is not PID 1).
		volumes = append(volumes, corev1.Volume{Name: volRunnerBin, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		initContainers = append(initContainers, corev1.Container{
			Name:                     ContainerInstall,
			Image:                    RunnerImage(r),
			ImagePullPolicy:          pullPolicy(r.Spec.ImagePullPolicy, RunnerImage(r)),
			Command:                  []string{"cp", runnerBinary, runnerBinDir + "/forgejo-runner"},
			VolumeMounts:             []corev1.VolumeMount{{Name: volRunnerBin, MountPath: runnerBinDir}},
			SecurityContext:          restrictedContext(true),
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		})
		runner.Command = []string{runnerBinDir + "/forgejo-runner", "daemon", "--config", configFile}
		runner.VolumeMounts = append(mounts, corev1.VolumeMount{Name: volRunnerBin, MountPath: runnerBinDir, ReadOnly: true})
		// Job steps run in this container: its root file system stays writable for them.
		runner.SecurityContext = restrictedContext(false)
		podSecurity.RunAsNonRoot = ptr.To(true)
		podSecurity.RunAsUser = ptr.To(runnerUID)
		podSecurity.RunAsGroup = ptr.To(runnerUID)
		shareProcessNamespace = ptr.To(true)
	} else {
		volumes = append(volumes,
			corev1.Volume{Name: volDockerSocket, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: volDockerStorage, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: r.Spec.Docker.StorageSizeLimit}}},
		)
		// dockerd listens on a UNIX socket only (no TCP port on the pod's network), owned by
		// group 1000, the runner's group. "dind" prepares cgroups for a nested daemon.
		args := append([]string{dindCommand, "dockerd", "--host=unix://" + DockerSocket, "--group=" + "1000"}, r.Spec.Docker.Args...)
		if !setsMTU(r.Spec.Docker.Args) {
			args = append([]string{"sh", "-c", podMTUScript, "sh"}, args...)
		}
		initContainers = append(initContainers, corev1.Container{
			Name:            ContainerDocker,
			Image:           DindImage(r),
			ImagePullPolicy: pullPolicy(r.Spec.ImagePullPolicy, DindImage(r)),
			// A native sidecar: started before the runner, stopped after it.
			RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
			Command:       args,
			Resources:     r.Spec.Docker.Resources,
			StartupProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
					Command: []string{dockerCLI, "--host=unix://" + DockerSocket, "info"},
				}},
				PeriodSeconds: 2, TimeoutSeconds: 5, SuccessThreshold: 1, FailureThreshold: 150,
			},
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: volDockerSocket, MountPath: DockerSocketDir},
				{Name: volDockerStorage, MountPath: "/var/lib/docker"},
			},
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		})
		runner.Command = []string{dumbInit, "--", runnerBinary, "daemon", "--config", configFile}
		runner.VolumeMounts = append(mounts, corev1.VolumeMount{Name: volDockerSocket, MountPath: DockerSocketDir})
		env = append(env, corev1.EnvVar{Name: "DOCKER_HOST", Value: "unix://" + DockerSocket})
		sc := restrictedContext(true)
		sc.RunAsUser = ptr.To(runnerUID)
		sc.RunAsGroup = ptr.To(runnerUID)
		sc.RunAsNonRoot = ptr.To(true)
		runner.SecurityContext = sc
	}
	runner.Env = dedupeEnv(env, r.Spec.Env)

	return corev1.PodSpec{
		InitContainers:                initContainers,
		Containers:                    []corev1.Container{runner},
		Volumes:                       volumes,
		SecurityContext:               podSecurity,
		ShareProcessNamespace:         shareProcessNamespace,
		ImagePullSecrets:              r.Spec.ImagePullSecrets,
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
		TerminationGracePeriodSeconds: ptr.To(GracePeriodSeconds(r)),
	}
}

// GracePeriodSeconds is the runner pods' termination grace period: the runner's shutdown
// timeout, within which it lets running jobs finish, plus a margin.
func GracePeriodSeconds(r *forgejov1.ForgejoRunner) int64 {
	return int64(ShutdownTimeout(r).Seconds()) + gracePeriodMargin
}

// restrictedContext passes the "restricted" Pod Security Standard.
func restrictedContext(readOnlyRoot bool) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(readOnlyRoot),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capDropAll}},
	}
}

func mergeLabels(extra, own map[string]string) map[string]string {
	out := make(map[string]string, len(extra)+len(own))
	maps.Copy(out, extra)
	maps.Copy(out, own)
	return out
}

// dedupeEnv concatenates env lists; a later variable replaces an earlier one of the same name.
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

// pullPolicy mirrors the API server default so that specs compare equal.
func pullPolicy(p corev1.PullPolicy, image string) corev1.PullPolicy {
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
