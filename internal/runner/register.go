package runner

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Slot is one runner to register: its index, name and secret, and the version Forgejo lists
// for it.
type Slot struct {
	Index int32
	Name  string
	Token string
	// Version is the runner version Forgejo lists for a runner registered before ("" for a
	// new or lost runner, which declares its version when its pod connects). Registering
	// writes Forgejo's version column (models/actions.RegisterRunner on Forgejo 15 and 17), so
	// a runner registered again without its pod restarting, as on a scope change, keeps the
	// version only when it is passed on.
	Version string
}

// versionPattern is a runner version as Forgejo Runner declares it (e.g. 13.2.0, v12.7.0-rc1).
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

// SafeVersion returns v when it is a plain version string and "" otherwise. The version is
// what a runner declared to Forgejo and is passed through the registration script, so nothing
// that could split a word or reach the shell gets through.
func SafeVersion(v string) string {
	if versionPattern.MatchString(v) {
		return v
	}
	return ""
}

// UUID of the slot's runner in Forgejo.
func (s Slot) UUID() string { return UUIDOf(s.Token) }

// RegistrationAnnotation lists the slots of a registration Job as "<index>=<uuid>,...".
func RegistrationAnnotation(slots []Slot) string {
	parts := make([]string, 0, len(slots))
	for _, s := range slots {
		parts = append(parts, fmt.Sprintf("%d=%s", s.Index, s.UUID()))
	}
	return strings.Join(parts, ",")
}

// ParseRegistrationAnnotation reverses RegistrationAnnotation.
func ParseRegistrationAnnotation(v string) map[int32]string {
	out := map[int32]string{}
	for p := range strings.SplitSeq(v, ",") {
		i, u, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(i, 10, 32)
		if err != nil || n < 0 {
			continue
		}
		out[int32(n)] = u
	}
	return out
}

// RegisterJobInput is what the registration Job is rendered from.
type RegisterJobInput struct {
	Runner  *forgejov1.ForgejoRunner
	Forgejo *forgejov1.Forgejo
	// Image is the instance's running Forgejo image (status.image): its CLI writes to the
	// instance's database.
	Image string
	// AppIniSecret is the Secret with the app.ini the instance's pods run with.
	AppIniSecret string
	Slots        []Slot
}

// registerScript registers every runner of $RUNNERS ("<index>:<version>:<name> ...") with
// "forgejo-cli actions register", which is idempotent: it creates the runner whose UUID the
// secret's first 16 characters give, or updates its name, scope, secret and version. Only the
// UUID is printed. Labels are left alone (--keep-labels): the runners declare them when they
// connect. The version is the one Forgejo listed for the runner (see Slot.Version), empty for
// a new runner; neither a version (SafeVersion) nor a name (a DNS label) contains a ':'.
// The secrets are read from files; error output is passed through a filter that masks
// anything shaped like a secret, because Forgejo prints the runner record (with its secret)
// when updating it fails.
const registerScript = `set -eu
F=%s
for runner in $RUNNERS; do
  i=${runner%%%%:*}
  rest=${runner#*:}
  version=${rest%%%%:*}
  name=${rest#*:}
  if uuid=$("$F" --config "$GITEA_APP_INI" forgejo-cli actions register --secret-file "%s/token-$i" --scope "$RUNNER_SCOPE" --name "$name" --version "$version" --keep-labels 2>/tmp/register.err); then
    echo "Registered runner $name ($uuid)"
  else
    sed -E 's/[0-9a-fA-F]{40}/<redacted>/g' /tmp/register.err >&2
    { printf 'Could not register runner %%s: ' "$name"; sed -E 's/[0-9a-fA-F]{40}/<redacted>/g' /tmp/register.err | tail -c 800 | tr '\n' ' '; } > /dev/termination-log
    exit 1
  fi
done
`

// registerScriptFor renders registerScript for the Forgejo binary cli and the directory of the
// runner secrets.
func registerScriptFor(cli, tokensDir string) string {
	return fmt.Sprintf(registerScript, cli, tokensDir)
}

// RegisterJob renders the registration Job. It runs the instance's own image as uid 1000 with
// a read-only root file system and the instance's app.ini (which holds the database
// credentials), but without the data volume: the CLI only writes to the database. It is placed
// like the instance's pods (see instancePlacement).
//
// The Job has no TTL: the controller deletes it once its result is recorded (or a minute
// after it failed), and the finalizer deletes it with the ForgejoRunner. A Job that vanished
// before it was collected would leave runners registered that nothing records.
func RegisterJob(in RegisterJobInput) *batchv1.Job {
	r, f := in.Runner, in.Forgejo
	l := forgejo.LayoutForImage(in.Image)
	runners := make([]string, 0, len(in.Slots))
	items := make([]corev1.KeyToPath, 0, len(in.Slots))
	for _, s := range in.Slots {
		runners = append(runners, fmt.Sprintf("%d:%s:%s", s.Index, SafeVersion(s.Version), s.Name))
		items = append(items, corev1.KeyToPath{Key: TokenKey(s.Index), Path: TokenKey(s.Index)})
	}
	const tokensDir = "/etc/forgejo-runner-tokens"
	labels := Labels(r, forgejo.ComponentRunnerRegistration)
	nonRoot := &corev1.SecurityContext{
		RunAsUser:                ptr.To(l.UID),
		RunAsGroup:               ptr.To(l.GID),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capDropAll}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RegisterJobName(r),
			Namespace: r.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				AnnotationRegistrationScope:   Scope(r),
				AnnotationRegistrationRunners: RegistrationAnnotation(in.Slots),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To[int32](2),
			ActiveDeadlineSeconds: ptr.To[int64](600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					ImagePullSecrets:             f.Spec.ImagePullSecrets,
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:        ptr.To(l.GID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "register",
						Image:           in.Image,
						ImagePullPolicy: pullPolicy(f.Spec.ImagePullPolicy, in.Image),
						Command:         []string{"/bin/sh", "-c", registerScriptFor(l.CLI, tokensDir)},
						Env: []corev1.EnvVar{
							{Name: "GITEA_WORK_DIR", Value: l.WorkPath},
							{Name: "GITEA_CUSTOM", Value: l.CustomPath},
							{Name: "GITEA_APP_INI", Value: l.ConfigPath},
							{Name: "HOME", Value: l.HomePath},
							{Name: "USER", Value: l.RunUser},
							{Name: "TMPDIR", Value: l.TmpPath},
							{Name: "RUNNERS", Value: strings.Join(runners, " ")},
							{Name: "RUNNER_SCOPE", Value: Scope(r)},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: volConfig, MountPath: l.ConfigDir, ReadOnly: true},
							{Name: volTokens, MountPath: tokensDir, ReadOnly: true},
							// Forgejo creates directories under WORK_PATH when it loads its
							// settings; they are thrown away with the pod.
							{Name: "data", MountPath: l.DataMountPath},
							{Name: "tmp", MountPath: l.TmpPath},
						},
						SecurityContext:          nonRoot,
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: []corev1.Volume{
						{Name: volConfig, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: in.AppIniSecret, DefaultMode: ptr.To[int32](0o440),
						}}},
						{Name: volTokens, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: TokensSecretName(r), Items: items, DefaultMode: ptr.To[int32](0o440),
						}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	instancePlacement(&job.Spec.Template.Spec, f)
	return job
}

// instancePlacement puts a registration pod where the instance's pods may run: the node
// selector, tolerations, node affinity and priority class of the instance's pod template, and
// its service account, whose imagePullSecrets may be how the instance's image is pulled (its
// token stays unmounted). Pod affinity and anti-affinity are left out: their terms select the
// instance's pods, and would misplace or block a pod that carries the operator's labels.
func instancePlacement(spec *corev1.PodSpec, f *forgejov1.Forgejo) {
	pt := f.Spec.PodTemplate
	spec.NodeSelector = maps.Clone(pt.NodeSelector)
	for _, t := range pt.Tolerations {
		spec.Tolerations = append(spec.Tolerations, *t.DeepCopy())
	}
	if pt.Affinity != nil && pt.Affinity.NodeAffinity != nil {
		spec.Affinity = &corev1.Affinity{NodeAffinity: pt.Affinity.NodeAffinity.DeepCopy()}
	}
	spec.PriorityClassName = pt.PriorityClassName
	spec.ServiceAccountName = pt.ServiceAccountName
	spec.AutomountServiceAccountToken = ptr.To(false)
}
