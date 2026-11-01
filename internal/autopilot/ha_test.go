package autopilot

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testValkeyPassword = "s3cr3tvalkeypassword"
	testValkeyPod0     = "fj-valkey-0"
	testValkeyPod1     = "fj-valkey-1"
	testValkeyPod2     = "fj-valkey-2"
)

func secretsWithValkeyPassword(f *forgejov1.Forgejo, pw string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data:       map[string][]byte{valkeyPasswordKey: []byte(pw)},
	}
}

// The Forgejo URL names every Sentinel, the group and both passwords; no database.
func TestValkeyURL(t *testing.T) {
	f := haInstance()
	u, err := url.Parse(valkeyURL(f, testValkeyPassword))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "redis+sentinel" || u.Path != "" {
		t.Fatalf("scheme %q path %q", u.Scheme, u.Path)
	}
	if pw, _ := u.User.Password(); pw != testValkeyPassword {
		t.Fatalf("password %q", pw)
	}
	want := "fj-valkey-0.fj-valkey.ns.svc.cluster.local:26379,fj-valkey-1.fj-valkey.ns.svc.cluster.local:26379," +
		"fj-valkey-2.fj-valkey.ns.svc.cluster.local:26379"
	if u.Host != want {
		t.Fatalf("hosts %q, want %q", u.Host, want)
	}
	if q := u.Query(); q.Get("mastername") != valkeyMasterName || q.Get("sentinelpassword") != testValkeyPassword {
		t.Fatalf("query %v", q)
	}
}

func reconcileValkey(t *testing.T, f *forgejov1.Forgejo, zoned bool) (testEnv, *appsv1.StatefulSet) {
	t.Helper()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	stubNodeZones(t, zoned)
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	return env, valkeySTS(t, env, f)
}

// Three pods with a server and a Sentinel each, the password only in env references and in
// the generated configuration files (never in arguments), a headless Service that publishes
// not-ready pods, a PDB and spreading.
func TestValkeyStatefulSet(t *testing.T) {
	f := haInstance()
	f.Spec.HighAvailability.Valkey.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")}}
	env, sts := reconcileValkey(t, f, true)
	if *sts.Spec.Replicas != valkeyMembers || sts.Spec.ServiceName != forgejo.ValkeyName(f) ||
		sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Fatalf("spec: replicas %d service %q policy %q", *sts.Spec.Replicas, sts.Spec.ServiceName, sts.Spec.PodManagementPolicy)
	}
	ps := sts.Spec.Template.Spec
	if len(ps.Containers) != 2 || ps.Containers[0].Name != valkeyName || ps.Containers[1].Name != sentinelName || len(ps.InitContainers) != 1 {
		t.Fatalf("containers %v init %v", ps.Containers, ps.InitContainers)
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		all := strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")
		if strings.Contains(all, testValkeyPassword) {
			t.Fatalf("%s: password in command/args", c.Name)
		}
		for _, e := range c.Env {
			if e.Value == testValkeyPassword {
				t.Fatalf("%s: password as a literal env value", c.Name)
			}
		}
		if c.SecurityContext == nil || !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("%s: security context %+v", c.Name, c.SecurityContext)
		}
	}
	server := ps.Containers[0]
	if server.Lifecycle == nil || !strings.Contains(strings.Join(server.Lifecycle.PreStop.Exec.Command, " "), "sentinel failover "+valkeyMasterName) {
		t.Fatalf("preStop %+v", server.Lifecycle)
	}
	config := strings.Join(ps.InitContainers[0].Command, " ")
	for _, want := range []string{
		"sentinel monitor forgejo $master 6379 2", "sentinel down-after-milliseconds forgejo 5000",
		"sentinel failover-timeout forgejo 60000", "sentinel resolve-hostnames yes", "sentinel announce-hostnames yes",
		"replica-announce-ip $self", "requirepass $VALKEY_PASSWORD", "masterauth $VALKEY_PASSWORD",
		"maxmemory 83886080", "maxmemory-policy volatile-lru", "appendonly yes", "sentinel failover forgejo",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config script lacks %q", want)
		}
	}
	if len(ps.TopologySpreadConstraints) != 2 || ps.TopologySpreadConstraints[1].TopologyKey != corev1.LabelTopologyZone {
		t.Fatalf("spread %+v", ps.TopologySpreadConstraints)
	}
	for _, v := range ps.Volumes {
		if v.Name == volConf && v.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Fatal("configuration with the password not in memory")
		}
	}

	// Reconciling again does not change the StatefulSet (no update loop on API server defaults
	// that the operator sets itself), also once the API server has defaulted maxUnavailable
	// (Kubernetes 1.37).
	for _, defaulted := range []bool{false, true} {
		if defaulted {
			sts.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(1))
			if err := env.rc.Client.Update(context.Background(), sts); err != nil {
				t.Fatal(err)
			}
		}
		rv := sts.ResourceVersion
		if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
			t.Fatal(err)
		}
		if sts = valkeySTS(t, env, f); sts.ResourceVersion != rv {
			t.Fatalf("StatefulSet updated without a change (maxUnavailable defaulted: %v)", defaulted)
		}
	}
}

// The headless Service publishes pods before they are Ready; the PDB lets one pod go at a
// time; spec.highAvailability.valkey.topologySpreadConstraints replace the default spread.
func TestValkeyServicePDBAndSpreadOverride(t *testing.T) {
	f := haInstance()
	env, sts := reconcileValkey(t, f, false)
	svc := &corev1.Service{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), svc) || svc.Spec.ClusterIP != corev1.ClusterIPNone ||
		!svc.Spec.PublishNotReadyAddresses || len(svc.Spec.Ports) != 2 {
		t.Fatalf("service %+v", svc.Spec)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), pdb) || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("pdb %+v", pdb.Spec)
	}

	// spec.highAvailability.valkey.topologySpreadConstraints replace the default.
	f.Spec.HighAvailability.Valkey.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "rack", WhenUnsatisfiable: corev1.DoNotSchedule}}
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts)
	if s := sts.Spec.Template.Spec.TopologySpreadConstraints; len(s) != 1 || s[0].TopologyKey != "rack" {
		t.Fatalf("override ignored: %+v", s)
	}
}

// rc.ValkeyURL is set as soon as the password is known and is the same whether or not Valkey
// is ready. While Valkey is not ready, or without the Secret anywhere, the step stops and
// requeues, except in maintenance.
func TestReconcileHAWaitsForValkey(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name                       string
		secret, ready, maintenance bool
		stop                       bool
	}{
		{name: "Valkey not ready", secret: true, stop: true},
		{name: "Valkey not ready, in maintenance", secret: true, maintenance: true},
		{name: "Valkey ready", secret: true, ready: true},
		{name: "no Secret", stop: true},
		{name: "no Secret, in maintenance", maintenance: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			var objs []client.Object
			if tt.secret {
				objs = append(objs, secretsWithValkeyPassword(f, "pw"))
			}
			env := newTestEnv(t, f, objs...)
			env.rc.Maintenance = tt.maintenance
			if tt.ready {
				if _, err := ReconcileHA(ctx, env.rc); err != nil {
					t.Fatal(err)
				}
				setValkeyStatus(t, env, f, 3, 1)
				env.rc.ValkeyURL = ""
			}
			res, err := ReconcileHA(ctx, env.rc)
			if err != nil {
				t.Fatal(err)
			}
			// Only a ready Valkey ends the requeues.
			if res.Stop != tt.stop || (res.RequeueAfter == 0) != tt.ready {
				t.Fatalf("res = %+v", res)
			}
			if want := valkeyURL(f, "pw"); tt.secret && env.rc.ValkeyURL != want {
				t.Fatalf("ValkeyURL = %q, want %q", env.rc.ValkeyURL, want)
			}
		})
	}
}

// The secrets step just created "<name>-secrets" and the cache has not seen it yet.
func TestReconcileHAReadsPasswordLiveWhenCacheLags(t *testing.T) {
	f := haInstance()
	env := newTestEnv(t, f)
	env.rc.APIReader = fake.NewClientBuilder().WithScheme(env.rc.Scheme).WithObjects(secretsWithValkeyPassword(f, "live")).Build()
	res, err := ReconcileHA(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if want := valkeyURL(f, "live"); env.rc.ValkeyURL != want {
		t.Fatalf("ValkeyURL = %q, want %q (res %+v)", env.rc.ValkeyURL, want, res)
	}
}

// --- Valkey is emptied after a restore and when its password changes ---

// valkeyResetEnv is an HA instance whose Valkey StatefulSet the HA step has created, with
// three running pods.
func valkeyResetEnv(t *testing.T) (testEnv, *forgejov1.Forgejo) {
	t.Helper()
	f := haInstance()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	for i := range 3 {
		p := valkeyPod(f, i, true)
		p.OwnerReferences[0].UID = sts.UID
		if err := env.rc.Client.Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	setValkeyStatus(t, env, f, 3, 3)
	env.events()
	return env, f
}

func valkeySTS(t *testing.T, env testEnv, f *forgejov1.Forgejo) *appsv1.StatefulSet {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts) {
		t.Fatal("no Valkey StatefulSet")
	}
	return sts
}

func setValkeyStatus(t *testing.T, env testEnv, f *forgejov1.Forgejo, replicas, ready int32) {
	t.Helper()
	sts := valkeySTS(t, env, f)
	sts.Status.ObservedGeneration, sts.Status.Replicas, sts.Status.ReadyReplicas = sts.Generation, replicas, ready
	if err := env.rc.Client.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
}

func deleteValkeyPods(t *testing.T, env testEnv, f *forgejov1.Forgejo) {
	t.Helper()
	if err := env.rc.Client.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace(f.Namespace),
		client.MatchingLabels(valkeySelector(f))); err != nil {
		t.Fatal(err)
	}
}

func scalingUpRestore(name, uid string) *forgejov1.ForgejoRestore {
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)}}
	rs.Status.Phase = forgejov1.RestorePhaseScalingUp
	return rs
}

func TestValkeyEmptiedAfterRestore(t *testing.T) {
	ctx := context.Background()
	env, f := valkeyResetEnv(t)
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r1", "uid-r1")

	// The restore reached ScalingUp: Valkey goes to zero before any Forgejo pod starts, and the
	// steps that roll Forgejo wait.
	res, err := ReconcileHA(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	if !res.Stop || *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetRestore+"uid-r1" {
		t.Fatalf("stop %v, replicas %d, annotations %v", res.Stop, *sts.Spec.Replicas, sts.Annotations)
	}
	if evs := env.events(); !hasEvent(evs, "Normal ValkeyReset") || !strings.Contains(strings.Join(evs, "\n"), "ForgejoRestore r1") {
		t.Fatalf("events %v", evs)
	}

	// Pods are left (terminating): it stays at zero, also once the restore has completed.
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = false, nil
	setValkeyStatus(t, env, f, 0, 0)
	if res, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if sts = valkeySTS(t, env, f); !res.Stop || *sts.Spec.Replicas != 0 {
		t.Fatalf("pods left: stop %v replicas %d", res.Stop, *sts.Spec.Replicas)
	}
	if evs := env.events(); hasEvent(evs, "Normal ValkeyReset") {
		t.Fatalf("reset announced twice: %v", evs)
	}

	// Every pod is gone: three new pods start without data; the step waits for one to be Ready.
	deleteValkeyPods(t, env, f)
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r1", "uid-r1")
	if res, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	sts = valkeySTS(t, env, f)
	if *sts.Spec.Replicas != valkeyMembers || sts.Annotations[annotationValkeyReset] != "" ||
		sts.Annotations[annotationValkeyResetDone] != valkeyResetRestore+"uid-r1" || !res.Stop {
		t.Fatalf("after the pods are gone: replicas %d, annotations %v, stop %v", *sts.Spec.Replicas, sts.Annotations, res.Stop)
	}

	// Once per restore: the same restore, still ScalingUp, does not empty it again.
	setValkeyStatus(t, env, f, 3, 3)
	if res, err = ReconcileHA(ctx, env.rc); err != nil || res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if sts = valkeySTS(t, env, f); *sts.Spec.Replicas != valkeyMembers {
		t.Fatalf("emptied again for the same restore: replicas %d", *sts.Spec.Replicas)
	}

	// A later restore does.
	env.rc.ScalingUpRestore = scalingUpRestore("r2", "uid-r2")
	if res, err = ReconcileHA(ctx, env.rc); err != nil || !res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if sts = valkeySTS(t, env, f); *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetRestore+"uid-r2" {
		t.Fatalf("second restore: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
}

// In maintenance (spec.restoreFrom holds the instance while the restore is ScalingUp) the
// workload stays at zero anyway, so the step does not stop the steps after it.
func TestValkeyEmptiedInMaintenanceDoesNotStop(t *testing.T) {
	env, f := valkeyResetEnv(t)
	env.rc.Maintenance = true
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r", "uid-r")
	res, err := ReconcileHA(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if sts := valkeySTS(t, env, f); res.Stop || *sts.Spec.Replicas != 0 {
		t.Fatalf("stop %v replicas %d", res.Stop, *sts.Spec.Replicas)
	}
}

// A changed valkey-password replaces every Valkey pod at once with the new password (a
// rolling update would stall at its first pod); app.ini follows once Valkey is back.
func TestValkeyEmptiedWhenPasswordChanges(t *testing.T) {
	ctx := context.Background()
	env, f := valkeyResetEnv(t)
	sec := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.SecretsName(f), sec) {
		t.Fatal("no secrets")
	}
	sec.Data[valkeyPasswordKey] = []byte("n3wpassword")
	if err := env.rc.Client.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}

	res, err := ReconcileHA(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	if !res.Stop || *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetPassword ||
		sts.Spec.Template.Annotations[annotationValkeyPassword] != passwordDigest("n3wpassword") {
		t.Fatalf("stop %v, replicas %d, annotations %v, template %v", res.Stop, *sts.Spec.Replicas, sts.Annotations, sts.Spec.Template.Annotations)
	}
	if !strings.Contains(env.rc.ValkeyURL, "n3wpassword") {
		t.Fatal("rc.ValkeyURL does not carry the new password")
	}
	evs := env.events()
	if !hasEvent(evs, "Normal ValkeyReset") || strings.Contains(strings.Join(evs, "\n"), "n3wpassword") ||
		strings.Contains(strings.Join(evs, "\n"), testValkeyPassword) {
		t.Fatalf("events %v", evs)
	}

	setValkeyStatus(t, env, f, 0, 0)
	deleteValkeyPods(t, env, f)
	if _, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	sts = valkeySTS(t, env, f)
	if *sts.Spec.Replicas != valkeyMembers || sts.Annotations[annotationValkeyReset] != "" || sts.Annotations[annotationValkeyResetDone] != "" {
		t.Fatalf("after the pods are gone: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
	setValkeyStatus(t, env, f, 3, 3)
	if res, err = ReconcileHA(ctx, env.rc); err != nil || res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
}

// --- Valkey cleanup ---

func owned(t *testing.T, f *forgejov1.Forgejo, obj client.Object) client.Object {
	t.Helper()
	if err := controllerutil.SetControllerReference(f, obj, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return obj
}

func valkeyObjects(t *testing.T, f *forgejov1.Forgejo) []client.Object {
	return []client.Object{
		valkeyStatefulSet(f),
		owned(t, f, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}),
		owned(t, f, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}),
	}
}

func TestValkeyKeptWhileRedisConfigMayRun(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		rolledBack bool // pods run the LKG
		lkgRedis   bool
		wantKept   bool
	}{
		{"rollback to a redis LKG is active", true, true, true},
		{"desired rolled out but the LKG still uses Valkey", false, true, true},
		{"desired rolled out and recorded as LKG", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash}
			deployed := testHash
			if tc.rolledBack {
				deployed = testOldHash
				f.Status.AutoPilot.RejectedConfigHash = testHash
			}
			dep := testDeployment(f, 1)
			dep.Spec.Template.Annotations[forgejo.AnnotationConfigHash] = deployed
			objs := append(valkeyObjects(t, f), dep, lkgSecret(f, tc.lkgRedis), readyPod(f, "fj-a", deployed, time.Hour))
			env := newTestEnv(t, f, objs...)
			env.rc.ConfigHash = deployed
			pods, _ := serverPods(ctx, env.rc)
			if err := cleanupValkey(ctx, env.rc, stateFor(f), dep, pods); err != nil {
				t.Fatal(err)
			}
			for _, obj := range []client.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &policyv1.PodDisruptionBudget{}} {
				if kept := found(t, env.rc.Client, f, forgejo.ValkeyName(f), obj); kept != tc.wantKept {
					t.Fatalf("Valkey %T kept = %v, want %v (events %q)", obj, kept, tc.wantKept, env.events())
				}
			}
		})
	}
}

// --- cron runner in the autopilot ---

// cronRunnerFor is the instance's cron runner StatefulSet, rolled out (or not) with testHash.
func cronRunnerFor(f *forgejov1.Forgejo, rolledOut bool) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace, Generation: 1,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo", Name: f.Name,
				UID: f.UID, Controller: ptr.To(true)}}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash}}}},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
			AvailableReplicas: 1, CurrentRevision: "fj-cron-1", UpdateRevision: "fj-cron-1"},
	}
	if !rolledOut {
		s.Status.AvailableReplicas, s.Status.ReadyReplicas, s.Status.UpdateRevision = 0, 0, "fj-cron-2"
	}
	return s
}

func cronPod(f *forgejov1.Forgejo, name string, since time.Duration) *corev1.Pod {
	p := readyPod(f, name, testHash, since)
	p.Labels = forgejo.CronSelectorLabels(f)
	return p
}

// pendingCronPod is a cron runner pod that the scheduler could not place for age.
func pendingCronPod(f *forgejov1.Forgejo, age time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f) + "-0", Namespace: f.Namespace, UID: "uid-cron",
			Labels: forgejo.CronSelectorLabels(f), CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}},
	}
	return p
}

// "<name>-cron" that the instance does not control (another instance's or a user's) is not
// its cron runner: it neither blocks nor counts towards the last known good config.
func TestCronRunnerOfAnotherOwnerIgnoredByAutopilot(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	foreign := cronRunnerFor(f, false)
	foreign.OwnerReferences[0].UID, foreign.OwnerReferences[0].Name = "uid-other", "fj-other"
	env := newTestEnv(t, f, foreign)
	if cron, err := cronRunner(ctx, env.rc); err != nil || cron != nil {
		t.Fatalf("cronRunner = %v, %v; want none", cron, err)
	}
	f.Spec.HighAvailability.Enabled = false
	own := cronRunnerFor(f, true)
	env = newTestEnv(t, f, own)
	if cron, err := cronRunner(ctx, env.rc); err != nil || cron != nil {
		t.Fatalf("cronRunner without HA = %v, %v; want none", cron, err)
	}
}

// The cron runner is one more pod sharing PostgreSQL's connections.
func TestDBMaxOpenConnsCountsCronRunner(t *testing.T) {
	f := haInstance()
	f.Spec.Replicas = ptr.To[int32](3)
	// (100 - 10) / (3 replicas + 1 surge + 1 cron runner) = 18
	if got := dbMaxOpenConns(f); got != 18 {
		t.Fatalf("MAX_OPEN_CONNS = %d, want 18", got)
	}
}

// --- node zones and shared storage ---

// withNodes serves nodeCapacity from a fake clientset with nodes, starting with an empty
// cache; the returned func makes every later List fail with err.
func withNodes(t *testing.T, nodes ...*corev1.Node) func(err error) {
	t.Helper()
	objs := make([]k8sruntime.Object, len(nodes))
	for i, n := range nodes {
		objs[i] = n
	}
	cs := kubefake.NewSimpleClientset(objs...)
	kubeMu.Lock()
	oldClient, oldConfig := kubeClient, kubeConfig
	kubeClient, kubeConfig = cs, &rest.Config{}
	kubeMu.Unlock()
	nodeInfo.Lock()
	oldAt, oldCount, oldMem, oldZoned, oldKnown := nodeInfo.at, nodeInfo.count, nodeInfo.maxMem, nodeInfo.zoned, nodeInfo.zonesKnown
	nodeInfo.at, nodeInfo.zonesKnown = time.Time{}, false
	nodeInfo.Unlock()
	t.Cleanup(func() {
		kubeMu.Lock()
		kubeClient, kubeConfig = oldClient, oldConfig
		kubeMu.Unlock()
		nodeInfo.Lock()
		nodeInfo.at, nodeInfo.count, nodeInfo.maxMem, nodeInfo.zoned, nodeInfo.zonesKnown = oldAt, oldCount, oldMem, oldZoned, oldKnown
		nodeInfo.Unlock()
	})
	return func(err error) {
		cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, k8sruntime.Object, error) { return true, nil, err })
		nodeInfo.Lock()
		nodeInfo.at = time.Now().Add(-time.Hour) // expire the cache
		nodeInfo.Unlock()
	}
}

func testNode(name, zone string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if zone != "" {
		n.Labels[corev1.LabelTopologyZone] = zone
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

// The zones are in every HA pod template, so they must not flip on a failed read or on
// nodes that are NotReady for a moment: either would roll every HA pod twice.
func TestNodeZonesStable(t *testing.T) {
	ctx := context.Background()
	fail := withNodes(t, testNode("a", "zone-a", false), testNode("b", "", true))
	if zoned, err := nodeZones(ctx); err != nil || !zoned {
		t.Fatalf("zoned = %v, %v; a NotReady node with a zone counts", zoned, err)
	}
	fail(errors.New("connection refused"))
	if zoned, err := nodeZones(ctx); err != nil || !zoned {
		t.Fatalf("after a failed read: zoned = %v, %v; want the last answer", zoned, err)
	}
	if _, err := schedulableNodes(ctx); err == nil {
		t.Fatal("schedulableNodes hid the failed read")
	}

	// Before any successful read, the error is returned and the HA step fails instead of
	// guessing; without permission to read nodes it spreads across nodes only.
	fail = withNodes(t)
	fail(errors.New("connection refused"))
	if _, err := nodeZones(ctx); err == nil {
		t.Fatal("no error before the first successful read")
	}
	f := haInstance()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	nodeZonesFn = func(context.Context) (bool, error) { return false, errors.New("connection refused") }
	if _, err := ReconcileHA(ctx, env.rc); err == nil || !strings.Contains(err.Error(), "zones") {
		t.Fatalf("ReconcileHA err = %v", err)
	}
	if found(t, env.rc.Client, f, forgejo.ValkeyName(f), &appsv1.StatefulSet{}) {
		t.Fatal("Valkey rendered without knowing the zones")
	}
	nodeZonesFn = func(context.Context) (bool, error) {
		return false, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("namespaced"))
	}
	if _, err := ReconcileHA(ctx, env.rc); err != nil || env.rc.NodeZones {
		t.Fatalf("forbidden: err = %v, zones %v", err, env.rc.NodeZones)
	}
}

// With highAvailability the data volume always has several pods, even at one replica: the
// cron runner and a surge pod. On a ReadWriteOnce volume and several nodes that is Degraded.
func TestSharedStorageCountsCronRunnerAndSurge(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name     string
		mode     corev1.PersistentVolumeAccessMode
		nodes    int
		degraded bool
	}{
		{"RWO, 3 nodes", corev1.ReadWriteOnce, 3, true},
		{"RWO, 1 node", corev1.ReadWriteOnce, 1, false},
		{"RWX, 3 nodes", corev1.ReadWriteMany, 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			f.Spec.Replicas = ptr.To[int32](1)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{tt.mode}}}
			env := newTestEnv(t, f, pvc)
			stubNodeCapacity(t, tt.nodes, 0)
			if err := checkSharedStorage(ctx, env.rc, stateFor(f)); err != nil {
				t.Fatal(err)
			}
			c := condition(f, forgejov1.ConditionDegraded)
			got := c != nil && c.Status == metav1.ConditionTrue && c.Reason == ReasonHARequiresSharedStorage
			if got != tt.degraded {
				t.Fatalf("Degraded = %+v, want HARequiresSharedStorage %v", c, tt.degraded)
			}
			if got && !strings.Contains(c.Message, "up to 3 Forgejo pods (server pods: up to 1, plus") {
				t.Fatalf("message %q", c.Message)
			}
		})
	}
}
