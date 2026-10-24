package autopilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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
	valkeyName   = "valkey"
	sentinelName = "sentinel"
	volData      = "data"
	volConf      = "conf"
)

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;delete

const (
	valkeyPort        = 6379
	sentinelPort      = 26379
	valkeyPasswordKey = "valkey-password"
	valkeyUID         = 999 // "valkey" in the official image
	valkeyGID         = 1000
	valkeyDataPath    = "/data"
	valkeyConfPath    = "/etc/valkey"

	// valkeyMembers is the size of the replication group: a primary, two replicas and a
	// Sentinel next to each, so that one pod (or node) can fail and the other two still
	// form the Sentinel majority that elects a new primary.
	valkeyMembers = 3
	// sentinelQuorum Sentinels must agree that the primary is down.
	sentinelQuorum = 2
	// sentinelDownAfter (ms) is how long the primary may be unreachable before a Sentinel
	// considers it down. Valkey's default (30s) would leave Forgejo without its cache and
	// queues long enough to fail the readiness probes of every server pod.
	sentinelDownAfter = 5000
	// sentinelFailoverTimeout (ms) bounds a failover.
	sentinelFailoverTimeout = 60000
	// valkeyMasterName is the Sentinel name of the replication group.
	valkeyMasterName = "forgejo"
	// valkeyTerminationPeriod leaves the primary's preStop hook time for a failover.
	valkeyTerminationPeriod = 30

	annotationValkeyPassword = "forgejo.lucas.icu/valkey-password-hash"

	// annotationValkeyReset is set on StatefulSet "<name>-valkey" while the operator empties
	// Valkey (see planValkeyReset): what it is emptied for (valkeyResetPassword or
	// valkeyResetRestore + the restore's UID).
	annotationValkeyReset = "forgejo.lucas.icu/valkey-reset"
	// annotationValkeyResetDone records the restore Valkey was last emptied for, so that a
	// restore that is still ScalingUp does not empty it again.
	annotationValkeyResetDone = "forgejo.lucas.icu/valkey-reset-done"
	valkeyResetPassword       = "password"
	valkeyResetRestore        = "restore/"
	// valkeyResetPoll is how often the HA step looks again while Valkey is emptied.
	valkeyResetPoll = 2 * time.Second
)

// ReconcileHA ensures Valkey when highAvailability is enabled and sets rc.ValkeyURL.
//
// Valkey keeps the cache and the queues (pending webhook deliveries, mirror syncs, PR
// checks, ...). It runs as StatefulSet "<name>-valkey": three pods, each with a Valkey server
// and a Sentinel, from the official image. One server is the primary, the others replicate
// from it; when the primary fails, the Sentinels promote a replica and Forgejo, which
// connects through the Sentinels (redis+sentinel://), follows. Data is an append-only file on
// an emptyDir: a restarted container keeps it, a new pod resynchronises from the primary.
// See docs/high-availability.md.
//
// rc.ValkeyURL is set as soon as the password is known, whether or not Valkey is ready: it
// only depends on the instance and the password, so every step renders the same app.ini.
// While no Valkey pod is ready the step stops, so that no pod is rolled onto a dead backend,
// except in maintenance: a restore keeps Forgejo scaled to zero, and the workload step that
// scales it down must not wait for Valkey. The step stops in the same way while it empties
// Valkey after a restore or a password change (planValkeyReset).
func ReconcileHA(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !f.Spec.HighAvailability.Enabled {
		// Valkey itself is removed by Reconcile once no pod uses it any more.
		clearDegraded(rc, ReasonHARequiresSharedStorage)
		return forgejo.StepResult{}, nil
	}
	// The zones are part of every HA pod template: a guess that a later read corrects would
	// roll every pod (Valkey failovers included) twice.
	switch zoned, err := nodeZonesFn(ctx); {
	case err == nil:
		rc.NodeZones = zoned
	case apierrors.IsForbidden(err):
		// Without permission to read nodes (a namespaced installation) the answer never
		// changes: pods are spread across nodes only.
		logger(ctx).V(1).Info("Could not read the nodes' zones; spreading pods across nodes only", "error", err.Error())
	default:
		return forgejo.StepResult{}, fmt.Errorf("reading the nodes' zones: %w", err)
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
	reset, err := planValkeyReset(ctx, rc, passwordDigest(password))
	if err != nil {
		return forgejo.StepResult{}, err
	}
	sts, err := ensureValkeyStatefulSet(ctx, rc, password, reset)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if reset.start {
		rc.Event(corev1.EventTypeNormal, "ValkeyReset", "HighAvailability",
			"emptying Valkey %s: %s. Its pods stop together and start again without data; Forgejo waits for it", sts.Name, reset.why)
	}
	if err := ensureValkeyPDB(ctx, rc); err != nil {
		return forgejo.StepResult{}, err
	}

	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := checkSharedStorage(ctx, rc, st); err != nil {
		return forgejo.StepResult{}, err
	}

	if reset.key != "" && !reset.empty {
		// Like a Valkey outage: nothing is rolled onto Valkey meanwhile.
		return forgejo.StepResult{Stop: !rc.Maintenance, RequeueAfter: valkeyResetPoll}, nil
	}
	if reset.empty {
		logger(ctx).Info("Started Valkey again without data", "statefulSet", sts.Name, "reset", reset.key)
	}

	// Pods are only rolled onto the redis config once Valkey answers, so an outage pauses
	// the steps that depend on this one instead of rolling pods onto a dead backend. A Ready
	// Valkey pod is a primary, or a replica whose link to the primary is up, and its Sentinel
	// answers: Forgejo can find the primary.
	if sts.Status.ReadyReplicas == 0 {
		if st.allow("waiting-valkey", 5*time.Minute) {
			rc.Event(corev1.EventTypeNormal, "WaitingForValkey", "HighAvailability", "waiting for Valkey %s to become ready", sts.Name)
		}
		return forgejo.StepResult{Stop: !rc.Maintenance, RequeueAfter: 5 * time.Second}, nil
	}
	return forgejo.StepResult{}, nil
}

// valkeyHosts are the DNS names of the Valkey pods (headless Service "<name>-valkey").
func valkeyHosts(f *forgejov1.Forgejo) []string {
	domain := forgejo.ServiceHost(forgejo.ValkeyName(f), f.Namespace)
	out := make([]string, valkeyMembers)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d.%s", forgejo.ValkeyName(f), i, domain)
	}
	return out
}

// valkeyURL is the URL Forgejo reaches Valkey with, without a database: the Sentinels of
// every pod, the group's name and the password (Valkey and its Sentinels share it). It
// depends only on the instance and the password, so it is the same whether or not Valkey is
// ready.
func valkeyURL(f *forgejov1.Forgejo, password string) string {
	hosts := valkeyHosts(f)
	for i, h := range hosts {
		hosts[i] = net.JoinHostPort(h, strconv.Itoa(sentinelPort))
	}
	q := url.Values{"mastername": {valkeyMasterName}, "sentinelpassword": {password}}
	return fmt.Sprintf("redis+sentinel://:%s@%s?%s", url.PathEscape(password), strings.Join(hosts, ","), q.Encode())
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

// passwordDigest is what the Valkey pod template records of the password it was rendered
// with (annotationValkeyPassword).
func passwordDigest(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:8])
}

// valkeyResetPlan says whether, and why, Valkey is emptied in this pass.
type valkeyResetPlan struct {
	// key is what Valkey is emptied for (annotationValkeyReset); "" when it is not.
	key string
	// why explains it in the ValkeyReset event.
	why string
	// start: this pass begins the reset.
	start bool
	// empty: every pod is gone, so this pass starts Valkey again.
	empty bool
}

// planValkeyReset decides whether Valkey is emptied: its StatefulSet is scaled to zero, and
// once every pod is gone, terminating ones included, back to three pods that start as a new,
// empty replication group (pod 0 the primary). Replacing pods one at a time, as a rolling
// update does, would keep the data, since each new pod resynchronises from the others. The
// HA step stops meanwhile, so no Forgejo pod is rolled onto Valkey. Valkey is emptied
//
//   - for a restore that has restored its backup (ForgejoRestore in ScalingUp, also the
//     rollback of a failed upgrade), before any Forgejo pod starts: the queues hold work
//     created after the backup point (webhook deliveries, PR checks, push updates, mirror
//     syncs by row ID), which would act on unrelated rows once the restored sequences reissue
//     those IDs, or, after a rollback, items the newer major wrote; and the cache would serve
//     state from after that point. Once per restore (annotationValkeyResetDone);
//   - when the Valkey password in "<name>-secrets" changed: a pod that starts with the new
//     one can neither ask the old pods' Sentinels for the primary nor replicate from it, so a
//     rolling update would stall at its first pod, with two pods left on the old password
//     that the new app.ini cannot use.
//
// A reset that has begun is completed even when its reason went away (the restore finished),
// so that new pods never resynchronise from an old one.
func planValkeyReset(ctx context.Context, rc *forgejo.ReconcileContext, digest string) (valkeyResetPlan, error) {
	f := rc.Instance
	sts := &appsv1.StatefulSet{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ValkeyName(f)}, sts); err != nil {
		// A Valkey that is still to be created starts empty.
		return valkeyResetPlan{}, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(sts, f) {
		return valkeyResetPlan{}, nil
	}
	var p valkeyResetPlan
	rs := rc.ScalingUpRestore
	switch {
	case sts.Annotations[annotationValkeyReset] != "":
		p.key = sts.Annotations[annotationValkeyReset]
	case sts.Spec.Template.Annotations[annotationValkeyPassword] != digest:
		p.key, p.start = valkeyResetPassword, true
		p.why = fmt.Sprintf("the Valkey password (key %s of Secret %s) changed", valkeyPasswordKey, forgejo.SecretsName(f))
	case rs != nil && sts.Annotations[annotationValkeyResetDone] != valkeyResetRestore+string(rs.UID):
		p.key, p.start = valkeyResetRestore+string(rs.UID), true
		p.why = fmt.Sprintf("ForgejoRestore %s restored a backup, and the queues and the cache in Valkey are from after it", rs.Name)
	}
	if p.key == "" || p.start {
		return p, nil
	}
	if sts.Status.ObservedGeneration < sts.Generation || sts.Status.Replicas > 0 {
		return p, nil
	}
	// Read live: a pod the cache no longer lists may still be terminating.
	var r client.Reader = rc.Client
	if rc.APIReader != nil {
		r = rc.APIReader
	}
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(valkeySelector(f))); err != nil {
		return p, err
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if metav1.IsControlledBy(pod, sts) && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return p, nil
		}
	}
	p.empty = true
	return p, nil
}

func valkeySelector(f *forgejov1.Forgejo) map[string]string {
	return map[string]string{
		forgejo.LabelName:      forgejo.AppName,
		forgejo.LabelInstance:  f.Name,
		forgejo.LabelComponent: forgejo.ComponentValkey,
	}
}

// ensureValkeyService is the headless Service that gives the Valkey pods their DNS names.
// They are published before the pods are Ready: a starting replica, and the Sentinels,
// resolve the primary and each other by name.
func ensureValkeyService(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, svc, true, func() error {
		svc.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = valkeySelector(f)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: valkeyName, Port: valkeyPort, TargetPort: intstr.FromString(valkeyName), Protocol: corev1.ProtocolTCP, AppProtocol: ptr.To("redis")},
			{Name: sentinelName, Port: sentinelPort, TargetPort: intstr.FromString(sentinelName), Protocol: corev1.ProtocolTCP, AppProtocol: ptr.To("redis")},
		}
		return nil
	})
	return err
}

// ensureValkeyPDB lets voluntary disruptions (node drains) take one Valkey pod at a time.
func ensureValkeyPDB(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, pdb, true, func() error {
		pdb.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: valkeySelector(f)}
		pdb.Spec.UnhealthyPodEvictionPolicy = ptr.To(policyv1.AlwaysAllow)
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

// sentinelResources are the fixed resources of the Sentinel and init containers.
func sentinelResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
}

// valkeyScripts are the shell scripts of the Valkey pods. Every one reads the password from
// the environment (VALKEY_PASSWORD, and REDISCLI_AUTH/VALKEYCLI_AUTH for valkey-cli), never
// from its arguments.
type valkeyScripts struct {
	// config (init container) finds the current primary through the other pods' Sentinels and
	// writes valkey.conf and sentinel.conf.
	config string
	// ready is the valkey container's readiness probe: a primary, or a replica whose link to
	// the primary is up.
	ready string
	// preStop hands the primary role to a replica before the primary's pod stops.
	preStop string
}

func newValkeyScripts(f *forgejov1.Forgejo, maxmemory int64) valkeyScripts {
	hosts := valkeyHosts(f)
	common := fmt.Sprintf(`self="$POD_NAME.%s"
peers="%s"
ask() {
  h=$1; shift
  timeout 3 valkey-cli --no-auth-warning -h "$h" -p %d "$@" 2>/dev/null
}
current_master() {
  for h in $peers; do
    [ "$h" = "$self" ] && continue
    m=$(ask "$h" sentinel get-master-addr-by-name %s | head -n 1 | tr -d '\r')
    if [ -n "$m" ]; then echo "$m"; return 0; fi
  done
  return 1
}
`, forgejo.ServiceHost(forgejo.ValkeyName(f), f.Namespace), strings.Join(hosts, " "), sentinelPort, valkeyMasterName)

	maxmem := ""
	if maxmemory > 0 {
		maxmem = fmt.Sprintf("maxmemory %d\n", maxmemory)
	}
	config := "set -eu\n" + common + fmt.Sprintf(`first="%s"
ordinal="${POD_NAME##*-}"
master=""
waited=0
while :; do
  found=$(current_master) || found=""
  if [ -z "$found" ]; then
    # No Sentinel names a primary: a new replication group, or every Sentinel is down.
    if [ "$ordinal" = 0 ]; then master="$self"; else master="$first"; fi
    break
  fi
  if [ "$found" != "$self" ]; then master="$found"; break; fi
  # The Sentinels still name this pod, which starts without data. Have them promote a
  # replica first, so that the replicas do not synchronise from an empty primary.
  res=""
  for h in $peers; do
    [ "$h" = "$self" ] && continue
    res=$(ask "$h" sentinel failover %[2]s) && [ -n "$res" ] && break
  done
  case "$res" in
    *NOGOOD*) echo "no replica can be promoted"; master="$self"; break ;;
  esac
  if [ "$waited" -ge 60 ]; then echo "the Sentinels did not fail over within 60s" >&2; master="$self"; break; fi
  waited=$((waited + 2))
  sleep 2
done
if [ "$master" = "$self" ]; then echo "starting as the primary"; else echo "starting as a replica of $master"; fi
umask 077
{
  echo "port %[3]d"
  echo "bind * -::*"
  echo "dir %[4]s"
  echo "appendonly yes"
  echo "appendfsync everysec"
  echo 'save ""'
  echo "maxmemory-policy volatile-lru"
  printf '%%s' "%[5]s"
  echo "replica-announce-ip $self"
  echo "replica-announce-port %[3]d"
  echo "requirepass $VALKEY_PASSWORD"
  echo "masterauth $VALKEY_PASSWORD"
  if [ "$master" != "$self" ]; then echo "replicaof $master %[3]d"; fi
} > %[6]s/valkey.conf
{
  echo "port %[7]d"
  echo "bind * -::*"
  echo "dir %[6]s"
  echo "sentinel resolve-hostnames yes"
  echo "sentinel announce-hostnames yes"
  echo "sentinel announce-ip $self"
  echo "sentinel announce-port %[7]d"
  echo "requirepass $VALKEY_PASSWORD"
  echo "sentinel sentinel-pass $VALKEY_PASSWORD"
  echo "sentinel monitor %[2]s $master %[3]d %[8]d"
  echo "sentinel auth-pass %[2]s $VALKEY_PASSWORD"
  echo "sentinel down-after-milliseconds %[2]s %[9]d"
  echo "sentinel failover-timeout %[2]s %[10]d"
  echo "sentinel parallel-syncs %[2]s 1"
} > %[6]s/sentinel.conf
`, hosts[0], valkeyMasterName, valkeyPort, valkeyDataPath, maxmem, valkeyConfPath, sentinelPort,
		sentinelQuorum, sentinelDownAfter, sentinelFailoverTimeout)

	ready := fmt.Sprintf(`r=$(timeout 3 valkey-cli --no-auth-warning -p %d info replication) || exit 1
case "$r" in
  *role:master*|*master_link_status:up*) exit 0 ;;
esac
exit 1
`, valkeyPort)

	preStop := common + fmt.Sprintf(`r=$(timeout 3 valkey-cli --no-auth-warning -p %d info replication) || exit 0
case "$r" in *role:master*) ;; *) exit 0 ;; esac
for h in $peers; do
  [ "$h" = "$self" ] && continue
  res=$(ask "$h" sentinel failover %s) && [ "$res" = OK ] && break
done
i=0
while [ "$i" -lt 20 ]; do
  m=$(current_master) || m=""
  if [ -n "$m" ] && [ "$m" != "$self" ]; then exit 0; fi
  i=$((i + 1))
  sleep 1
done
`, valkeyPort, valkeyMasterName)
	return valkeyScripts{config: config, ready: ready, preStop: preStop}
}

// ensureValkeyStatefulSet ensures StatefulSet "<name>-valkey". Fields the API server would
// default are set explicitly, so an unchanged spec compares equal and causes no update.
func ensureValkeyStatefulSet(ctx context.Context, rc *forgejo.ReconcileContext, password string, reset valkeyResetPlan) (*appsv1.StatefulSet, error) {
	f := rc.Instance
	image := f.Spec.HighAvailability.Valkey.Image
	if image == "" {
		image = forgejov1.DefaultValkeyImage
	}
	res := valkeyResources(f)
	var maxmemory int64
	if lim := res.Limits.Memory(); !lim.IsZero() {
		maxmemory = lim.Value() * 8 / 10
	}
	scripts := newValkeyScripts(f, maxmemory)
	replicas := int32(valkeyMembers)
	if reset.key != "" && !reset.empty {
		replicas = 0
	}
	secretRef := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: forgejo.SecretsName(f)},
		Key:                  valkeyPasswordKey,
	}}
	env := []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "VALKEY_PASSWORD", ValueFrom: secretRef},
		{Name: "REDISCLI_AUTH", ValueFrom: secretRef},
		{Name: "VALKEYCLI_AUTH", ValueFrom: secretRef},
	}
	pull := pullPolicy(f, image)
	secCtx := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	mounts := []corev1.VolumeMount{
		{Name: volData, MountPath: valkeyDataPath},
		{Name: volConf, MountPath: valkeyConfPath},
	}
	shell := func(script string) []string { return []string{"sh", "-c", script} }
	container := func(name string, port int32) corev1.Container {
		return corev1.Container{
			Name:            name,
			Image:           image,
			ImagePullPolicy: pull,
			Env:             env,
			Ports:           []corev1.ContainerPort{{Name: name, ContainerPort: port, Protocol: corev1.ProtocolTCP}},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(name)}},
				InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 6, SuccessThreshold: 1,
			},
			VolumeMounts:             mounts,
			SecurityContext:          secCtx,
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		}
	}
	server := container(valkeyName, valkeyPort)
	server.Command = []string{"valkey-server", valkeyConfPath + "/valkey.conf"}
	server.Resources = res
	server.ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: shell(scripts.ready)}},
		PeriodSeconds: 5, TimeoutSeconds: 4, FailureThreshold: 3, SuccessThreshold: 1,
	}
	server.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: shell(scripts.preStop)}}}
	sentinel := container(sentinelName, sentinelPort)
	sentinel.Command = []string{"valkey-sentinel", valkeyConfPath + "/sentinel.conf"}
	sentinel.Resources = sentinelResources()
	sentinel.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: shell(
			fmt.Sprintf("timeout 3 valkey-cli --no-auth-warning -p %d ping | grep -q PONG", sentinelPort))}},
		PeriodSeconds: 5, TimeoutSeconds: 4, FailureThreshold: 3, SuccessThreshold: 1,
	}
	initConfig := corev1.Container{
		Name:                     "config",
		Image:                    image,
		ImagePullPolicy:          pull,
		Command:                  shell(scripts.config),
		Env:                      env,
		Resources:                sentinelResources(),
		VolumeMounts:             mounts,
		SecurityContext:          secCtx,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}

	spread := f.Spec.HighAvailability.Valkey.TopologySpreadConstraints
	if len(spread) == 0 {
		spread = forgejo.DefaultTopologySpread(&metav1.LabelSelector{MatchLabels: valkeySelector(f)}, rc.NodeZones)
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, sts, true, func() error {
		exists := sts.ResourceVersion != ""
		sts.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		switch {
		case reset.empty:
			delete(sts.Annotations, annotationValkeyReset)
			if strings.HasPrefix(reset.key, valkeyResetRestore) {
				metav1.SetMetaDataAnnotation(&sts.ObjectMeta, annotationValkeyResetDone, reset.key)
			}
		case reset.key != "":
			metav1.SetMetaDataAnnotation(&sts.ObjectMeta, annotationValkeyReset, reset.key)
		}
		sts.Spec.Replicas = ptr.To(replicas)
		if !exists {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: valkeySelector(f)}
			sts.Spec.ServiceName = forgejo.ValkeyName(f)
			// Every pod starts at once: a pod waits in its init container for the others'
			// Sentinels, not for their readiness.
			sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		}
		sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
			Type:          appsv1.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
		}
		sts.Spec.RevisionHistoryLimit = ptr.To[int32](3)
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}

		t := &sts.Spec.Template
		t.Labels = forgejo.Labels(f, forgejo.ComponentValkey)
		t.Annotations = map[string]string{annotationValkeyPassword: passwordDigest(password)}
		ps := &t.Spec
		ps.RestartPolicy = corev1.RestartPolicyAlways
		ps.DNSPolicy = corev1.DNSClusterFirst
		ps.SchedulerName = corev1.DefaultSchedulerName
		ps.TerminationGracePeriodSeconds = ptr.To[int64](valkeyTerminationPeriod)
		ps.EnableServiceLinks = ptr.To(false)
		ps.AutomountServiceAccountToken = ptr.To(false)
		ps.ImagePullSecrets = f.Spec.ImagePullSecrets
		ps.NodeSelector = f.Spec.PodTemplate.NodeSelector
		ps.Tolerations = f.Spec.PodTemplate.Tolerations
		ps.PriorityClassName = f.Spec.PodTemplate.PriorityClassName
		ps.TopologySpreadConstraints = spread
		ps.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:      ptr.To[int64](valkeyUID),
			RunAsGroup:     ptr.To[int64](valkeyGID),
			FSGroup:        ptr.To[int64](valkeyGID),
			RunAsNonRoot:   ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		ps.Volumes = []corev1.Volume{
			{Name: volData, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			// The configuration holds the password: memory only.
			{Name: volConf, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		}
		ps.InitContainers = []corev1.Container{initConfig}
		ps.Containers = []corev1.Container{server, sentinel}
		return nil
	})
	return sts, err
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
	valkey := &appsv1.StatefulSet{}
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
	for _, obj := range []client.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &policyv1.PodDisruptionBudget{}} {
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

// checkSharedStorage warns when the Forgejo pods would share a ReadWriteOnce data volume on
// a multi-node cluster: a pod scheduled to another node than the one the volume is attached
// to cannot mount it. With highAvailability that is always several pods, even at one replica:
// the cron runner and, during rolling updates, a surge pod.
func checkSharedStorage(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState) error {
	f := rc.Instance
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
	msg := fmt.Sprintf("up to %d Forgejo pods (server pods: up to %d, plus a surge pod during rolling updates and the cron runner) "+
		"share the ReadWriteOnce data volume %s on a %d-node cluster; pods on another node than the one it is attached to "+
		"cannot mount it (Multi-Attach). Use a ReadWriteMany storage class (spec.storage.accessModes)",
		maxForgejoPods(f), maxPods(f), dataClaimName(f), nodes)
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

// maxForgejoPods is the largest number of Forgejo pods that can run at once, all of them
// mounting the data volume: maxPods server pods, with highAvailability a surge pod during
// rolling updates, and the cron runner.
func maxForgejoPods(f *forgejov1.Forgejo) int32 {
	n := maxPods(f)
	if f.Spec.HighAvailability.Enabled {
		n++ // rolling updates surge one extra pod
	}
	if forgejo.CronRunnerEnabled(f) {
		n++ // the cron runner
	}
	return n
}

// maxPods is the largest replica count the spec allows (HPA max or spec.replicas).
func maxPods(f *forgejov1.Forgejo) int32 {
	if h := f.Spec.Autoscaling.Horizontal; forgejo.HorizontalAutoscalingEnabled(f) {
		return max(h.MaxReplicas, h.MinReplicas, 1)
	}
	return max(forgejo.DesiredReplicas(f), 1)
}
