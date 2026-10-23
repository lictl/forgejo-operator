package autopilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	valkeyName = "valkey"
	volData    = "data"
)

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list

const (
	valkeyPort        = 6379
	valkeyPasswordKey = "valkey-password"
	valkeyUID         = 999 // "valkey" in the official image
	valkeyGID         = 1000
	valkeyDataPath    = "/data"

	annotationValkeyPassword = "forgejo.lucas.icu/valkey-password-hash"
)

// ReconcileHA ensures Valkey when highAvailability is enabled and sets rc.ValkeyURL.
//
// Valkey keeps cache, sessions and queues. Queues hold pending work (webhook deliveries,
// mirror syncs, PR checks), so it runs with an append-only file on an emptyDir: a crashed or
// OOM-killed Valkey container restarts with its data, while a rescheduled pod starts empty
// (cache and sessions are rebuilt, queued tasks are re-created by Forgejo's cron/retry
// logic). A PVC would survive rescheduling but ties Valkey to one node and adds a
// provisioning dependency on every cluster, for data that is mostly transient.
//
// rc.ValkeyURL is set as soon as the password is known, whether or not Valkey is ready: it
// only depends on the instance and the password, so every step renders the same app.ini.
// While Valkey is not ready the step stops, so that no pod is rolled onto a dead backend,
// except in maintenance: a restore keeps Forgejo scaled to zero, and the workload step that
// scales it down must not wait for Valkey.
func ReconcileHA(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !f.Spec.HighAvailability.Enabled {
		// Valkey itself is removed by Reconcile once no pod uses it any more.
		clearDegraded(rc, ReasonHARequiresSharedStorage)
		return forgejo.StepResult{}, nil
	}

	password, err := ensureValkeyPassword(ctx, rc, rc.Client)
	if (apierrors.IsConflict(err) || apierrors.IsNotFound(err)) && rc.APIReader != nil {
		// The secrets step just created or changed the Secret and the cache lags behind;
		// read it live.
		password, err = ensureValkeyPassword(ctx, rc, rc.APIReader)
	}
	if err != nil {
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return forgejo.StepResult{}, err
		}
		// Still racing with another writer; retry with a fresh copy.
		if !rc.Maintenance {
			return forgejo.StepResult{Stop: true, RequeueAfter: 2 * time.Second}, nil
		}
		// Forgejo is scaled to zero, so an app.ini without Valkey does no harm until the
		// password is read on the next pass; scaling down must not wait for it.
		logger(ctx).Info("cannot read the Valkey password yet; continuing in maintenance", "error", err.Error())
		return forgejo.StepResult{RequeueAfter: 2 * time.Second}, nil
	}
	rc.ValkeyURL = valkeyURL(f, password)
	if err := ensureValkeyService(ctx, rc); err != nil {
		return forgejo.StepResult{}, err
	}
	dep, err := ensureValkeyDeployment(ctx, rc, password)
	if err != nil {
		return forgejo.StepResult{}, err
	}

	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := checkSharedStorage(ctx, rc, st); err != nil {
		return forgejo.StepResult{}, err
	}

	// Pods are only rolled onto the redis config once Valkey answers, so an outage pauses
	// the steps that depend on this one instead of rolling pods onto a dead backend.
	if !deploymentReady(dep) {
		if st.allow("waiting-valkey", 5*time.Minute) {
			rc.Event(corev1.EventTypeNormal, "WaitingForValkey", "HighAvailability", "waiting for Valkey %s to become ready", dep.Name)
		}
		return forgejo.StepResult{Stop: !rc.Maintenance, RequeueAfter: 5 * time.Second}, nil
	}
	return forgejo.StepResult{}, nil
}

// valkeyURL is the redis URL Forgejo uses for Valkey. It depends only on the instance and
// the password, so it is the same whether or not Valkey is ready.
func valkeyURL(f *forgejov1.Forgejo, password string) string {
	return fmt.Sprintf("redis://:%s@%s:%d", password, forgejo.ServiceHost(forgejo.ValkeyName(f), f.Namespace), valkeyPort)
}

func deploymentReady(dep *appsv1.Deployment) bool {
	want := ptr.Deref(dep.Spec.Replicas, 1)
	s := dep.Status
	return s.ObservedGeneration >= dep.Generation && s.UpdatedReplicas == want && s.AvailableReplicas >= want && s.Replicas == want
}

// ensureValkeyPassword adds "valkey-password" to "<name>-secrets" without touching the other
// keys, reading the Secret through r. The optimistic lock prevents a stale read from
// generating a second password.
func ensureValkeyPassword(ctx context.Context, rc *forgejo.ReconcileContext, r client.Reader) (string, error) {
	f := rc.Instance
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec); err != nil {
		return "", err
	}
	if pw := string(sec.Data[valkeyPasswordKey]); pw != "" {
		return pw, nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(buf)
	patch := client.MergeFromWithOptions(sec.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[valkeyPasswordKey] = []byte(pw)
	if err := rc.Client.Patch(ctx, sec, patch); err != nil {
		return "", err
	}
	rc.Event(corev1.EventTypeNormal, "ValkeyPasswordGenerated", "HighAvailability", "generated the Valkey password in %s", sec.Name)
	return pw, nil
}

func valkeySelector(f *forgejov1.Forgejo) map[string]string {
	return map[string]string{
		forgejo.LabelName:      forgejo.AppName,
		forgejo.LabelInstance:  f.Name,
		forgejo.LabelComponent: forgejo.ComponentValkey,
	}
}

func ensureValkeyService(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, svc, true, func() error {
		svc.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = valkeySelector(f)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:        valkeyName,
			Port:        valkeyPort,
			TargetPort:  intstr.FromString("valkey"),
			Protocol:    corev1.ProtocolTCP,
			AppProtocol: ptr.To("redis"),
		}}
		return nil
	})
	return err
}

func valkeyResources(f *forgejov1.Forgejo) corev1.ResourceRequirements {
	r := f.Spec.HighAvailability.Valkey.Resources
	if len(r.Requests) == 0 && len(r.Limits) == 0 {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
	}
	return *r.DeepCopy()
}

func ensureValkeyDeployment(ctx context.Context, rc *forgejo.ReconcileContext, password string) (*appsv1.Deployment, error) {
	f := rc.Instance
	image := f.Spec.HighAvailability.Valkey.Image
	if image == "" {
		image = forgejov1.DefaultValkeyImage
	}
	res := valkeyResources(f)
	args := []string{
		"--port", strconv.Itoa(valkeyPort),
		"--requirepass", "$(VALKEY_PASSWORD)",
		"--dir", valkeyDataPath,
		"--appendonly", "yes",
		"--appendfsync", "everysec",
		"--save", "",
		// Only keys with a TTL (cache, sessions) may be evicted; queue keys have none.
		"--maxmemory-policy", "volatile-lru",
	}
	if lim := res.Limits.Memory(); !lim.IsZero() {
		args = append(args, "--maxmemory", strconv.FormatInt(lim.Value()*8/10, 10))
	}
	sum := sha256.Sum256([]byte(password))
	secretRef := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: forgejo.SecretsName(f)},
		Key:                  valkeyPasswordKey,
	}}
	probeCmd := []string{"sh", "-c", "valkey-cli -h 127.0.0.1 -p " + strconv.Itoa(valkeyPort) + " ping | grep -q PONG"}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, dep, true, func() error {
		dep.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		dep.Spec.Replicas = ptr.To[int32](1)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: valkeySelector(f)}
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		dep.Spec.RevisionHistoryLimit = ptr.To[int32](3)
		dep.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)

		t := &dep.Spec.Template
		t.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		t.Annotations = map[string]string{annotationValkeyPassword: hex.EncodeToString(sum[:8])}
		ps := &t.Spec
		ps.RestartPolicy = corev1.RestartPolicyAlways
		ps.DNSPolicy = corev1.DNSClusterFirst
		ps.SchedulerName = corev1.DefaultSchedulerName
		ps.TerminationGracePeriodSeconds = ptr.To[int64](30)
		ps.EnableServiceLinks = ptr.To(false)
		ps.AutomountServiceAccountToken = ptr.To(false)
		ps.ImagePullSecrets = f.Spec.ImagePullSecrets
		ps.NodeSelector = f.Spec.PodTemplate.NodeSelector
		ps.Tolerations = f.Spec.PodTemplate.Tolerations
		ps.PriorityClassName = f.Spec.PodTemplate.PriorityClassName
		ps.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:      ptr.To[int64](valkeyUID),
			RunAsGroup:     ptr.To[int64](valkeyGID),
			FSGroup:        ptr.To[int64](valkeyGID),
			RunAsNonRoot:   ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		ps.Volumes = []corev1.Volume{{Name: volData, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		ps.Containers = []corev1.Container{{
			Name:            valkeyName,
			Image:           image,
			ImagePullPolicy: pullPolicy(f, image),
			Command:         []string{"valkey-server"},
			Args:            args,
			Env: []corev1.EnvVar{
				{Name: "VALKEY_PASSWORD", ValueFrom: secretRef},
				{Name: "REDISCLI_AUTH", ValueFrom: secretRef},
			},
			Ports:     []corev1.ContainerPort{{Name: valkeyName, ContainerPort: valkeyPort, Protocol: corev1.ProtocolTCP}},
			Resources: res,
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: probeCmd}},
				PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3, SuccessThreshold: 1,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("valkey")}},
				InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 6, SuccessThreshold: 1,
			},
			VolumeMounts: []corev1.VolumeMount{{Name: volData, MountPath: valkeyDataPath}},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		}}
		return nil
	})
	return dep, err
}

func pullPolicy(f *forgejov1.Forgejo, image string) corev1.PullPolicy {
	if f.Spec.ImagePullPolicy != "" {
		return f.Spec.ImagePullPolicy
	}
	if forgejo.ImageTag(image) == "latest" || forgejo.ImageTag(image) == "" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// cleanupValkey removes Valkey once HA is disabled and nothing can bring a redis app.ini
// back: the desired (non-redis) config is deployed, not a rolled-back one, every Forgejo pod
// runs it, and the last known good config a future rollback would deploy no longer uses
// Valkey. Until recordLastKnownGood has stored the non-HA config, Valkey stays.
func cleanupValkey(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, dep *appsv1.Deployment, pods []corev1.Pod) error {
	f := rc.Instance
	if f.Spec.HighAvailability.Enabled {
		return nil
	}
	key := client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ValkeyName(f)}
	valkey := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, key, valkey); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		valkey = nil
	}
	svc := &corev1.Service{}
	if err := rc.Client.Get(ctx, key, svc); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		svc = nil
	}
	if (valkey == nil || !metav1.IsControlledBy(valkey, f)) && (svc == nil || !metav1.IsControlledBy(svc, f)) {
		return nil
	}

	keep, why, err := valkeyKeepReason(ctx, rc, dep, pods)
	if err != nil {
		return err
	}
	if keep != "" {
		if st.allow("valkey-kept/"+keep, time.Hour) {
			rc.Event(corev1.EventTypeNormal, "ValkeyRetained", "HighAvailability",
				"highAvailability is disabled but Valkey %s is kept: %s", key.Name, why)
		}
		return nil
	}

	deleted := false
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		ok, err := deleteOwned(ctx, rc, key, obj)
		if err != nil {
			return err
		}
		deleted = deleted || ok
	}
	if deleted {
		rc.Event(corev1.EventTypeNormal, "ValkeyDeleted", "HighAvailability", "removed Valkey %s: highAvailability is disabled", key.Name)
	}
	return nil
}

// valkeyKeepReason says why Valkey must stay although HA is disabled: a short key for the
// reason (empty when nothing needs Valkey any more) and the explanation for the event.
func valkeyKeepReason(ctx context.Context, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, pods []corev1.Pod) (string, string, error) {
	f := rc.Instance
	keep, why := "", ""
	switch {
	case rc.ConfigHash == "" || rc.ConfigHash != rc.DesiredConfigHash:
		keep, why = "rollback", "the pods run a rolled-back app.ini that may still use it"
	case f.Status.AutoPilot != nil && f.Status.AutoPilot.RejectedConfigHash != "":
		keep, why = "rejected", "a config rollback is pending"
	case dep != nil && !fullyRolledOut(dep, rc.ConfigHash) && ptr.Deref(dep.Spec.Replicas, 1) > 0:
		keep, why = "rollout", "the non-HA app.ini is still rolling out"
	}
	if keep == "" {
		for i := range pods {
			if pods[i].Annotations[forgejo.AnnotationConfigHash] != rc.DesiredConfigHash {
				keep, why = "rollout", fmt.Sprintf("pod %s still runs an earlier app.ini", pods[i].Name)
				break
			}
		}
	}
	if keep == "" && configRollbackEnabled(f) {
		lkg, err := currentLKG(ctx, rc)
		if err != nil {
			return "", "", err
		}
		if lkg != nil && lkgUsesValkey(f, lkg) {
			keep, why = "lkg", "the last known good app.ini, which a config rollback would deploy, still uses it; it is replaced once the non-HA app.ini has run healthy"
		}
	}
	return keep, why, nil
}

// deleteOwned deletes the named object when it exists and is controlled by the instance.
func deleteOwned(ctx context.Context, rc *forgejo.ReconcileContext, key client.ObjectKey, obj client.Object) (bool, error) {
	if err := rc.Client.Get(ctx, key, obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, rc.Instance) {
		return false, nil
	}
	if err := rc.Client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return true, nil
}

// checkSharedStorage warns when several Forgejo pods would share a ReadWriteOnce data volume
// on a multi-node cluster: pods scheduled to another node cannot mount it.
func checkSharedStorage(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState) error {
	f := rc.Instance
	if maxPods(f) <= 1 {
		clearDegraded(rc, ReasonHARequiresSharedStorage)
		return nil
	}
	modes := f.Spec.Storage.AccessModes
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: dataClaimName(f)}, pvc); err == nil {
		modes = pvc.Spec.AccessModes
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if slices.Contains(modes, corev1.ReadWriteMany) {
		clearDegraded(rc, ReasonHARequiresSharedStorage)
		return nil
	}
	nodes, err := schedulableNodes(ctx)
	if err != nil {
		logger(ctx).Info("cannot count nodes; skipping the shared storage check", "error", err.Error())
		return nil
	}
	if nodes <= 1 {
		clearDegraded(rc, ReasonHARequiresSharedStorage)
		return nil
	}
	msg := fmt.Sprintf("up to %d Forgejo replicas share the ReadWriteOnce data volume %s on a %d-node cluster; "+
		"pods on other nodes cannot mount it. Use a ReadWriteMany storage class (spec.storage.accessModes)", maxPods(f), dataClaimName(f), nodes)
	setDegraded(rc, ReasonHARequiresSharedStorage, msg)
	warnEvery(rc, st, "ha-storage", ReasonHARequiresSharedStorage, "%s", msg)
	return nil
}

func dataClaimName(f *forgejov1.Forgejo) string {
	if f.Spec.Storage.ExistingClaim != "" {
		return f.Spec.Storage.ExistingClaim
	}
	return forgejo.DataPVCName(f)
}

// maxPods is the largest replica count the spec allows (HPA max or spec.replicas).
func maxPods(f *forgejov1.Forgejo) int32 {
	if h := f.Spec.Autoscaling.Horizontal; forgejo.HorizontalAutoscalingEnabled(f) {
		return max(h.MaxReplicas, h.MinReplicas, 1)
	}
	return max(forgejo.DesiredReplicas(f), 1)
}
