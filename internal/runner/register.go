package runner

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Slot is one runner to register: its index, name and secret.
type Slot struct {
	Index int32
	Name  string
	Token string
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

// registerScript registers every runner of $RUNNERS ("<index>:<name> ...") with
// "forgejo-cli actions register", which is idempotent: it creates the runner whose UUID the
// secret's first 16 characters give, or updates its name, scope and secret. Only the UUID is
// printed. Labels are left alone (--keep-labels): the runners declare them when they connect.
// The secrets are read from files; error output is passed through a filter that masks
// anything shaped like a secret, because Forgejo prints the runner record (with its secret)
// when updating it fails.
const registerScript = `set -eu
F=%s
for runner in $RUNNERS; do
  i=${runner%%%%:*}
  name=${runner#*:}
  if uuid=$("$F" --config "$GITEA_APP_INI" forgejo-cli actions register --secret-file "%s/token-$i" --scope "$RUNNER_SCOPE" --name "$name" --keep-labels 2>/tmp/register.err); then
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
// credentials), but without the data volume: the CLI only writes to the database.
func RegisterJob(in RegisterJobInput) *batchv1.Job {
	r, f := in.Runner, in.Forgejo
	l := forgejo.LayoutForImage(in.Image)
	runners := make([]string, 0, len(in.Slots))
	items := make([]corev1.KeyToPath, 0, len(in.Slots))
	for _, s := range in.Slots {
		runners = append(runners, fmt.Sprintf("%d:%s", s.Index, s.Name))
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
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	return &batchv1.Job{
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
			BackoffLimit:            ptr.To[int32](2),
			ActiveDeadlineSeconds:   ptr.To[int64](600),
			TTLSecondsAfterFinished: ptr.To[int32](3600),
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
							{Name: "config", MountPath: l.ConfigDir, ReadOnly: true},
							{Name: "tokens", MountPath: tokensDir, ReadOnly: true},
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
						{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: in.AppIniSecret, DefaultMode: ptr.To[int32](0o440),
						}}},
						{Name: "tokens", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: TokensSecretName(r), Items: items, DefaultMode: ptr.To[int32](0o440),
						}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}
