package core

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testCronEnabled = "ENABLED"
	testHAHash      = "cfg-hash"
)

const testValkeyURL = "redis+sentinel://:pw@fj-valkey-0.fj-valkey.ns.svc.cluster.local:26379,fj-valkey-1.fj-valkey.ns.svc.cluster.local:26379" +
	"?mastername=forgejo&sentinelpassword=pw"

// renderHAConfig runs the config step for f with Valkey and returns "<name>-config".
func renderHAConfig(t *testing.T, f *forgejov1.Forgejo, valkeyURL string) *corev1.Secret {
	t.Helper()
	ctx := context.Background()
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data: map[string][]byte{forgejo.SecretKeyInternalToken: []byte("tok")}}
	rc := testContext(t, f, secrets)
	rc.Recorder = events.NewFakeRecorder(10)
	rc.TargetImage = forgejo.Image(f)
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.ValkeyURL = valkeyURL
	rc.DB = &forgejo.DBConnection{Host: "db", Port: 5432, Name: testForgejoName, User: testForgejoName, Password: "pw", SSLMode: testSSLModeDisable}
	if _, err := ReconcileConfig(ctx, rc); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, sec); err != nil {
		t.Fatal(err)
	}
	return sec
}

// With highAvailability, the server pods' app-web.ini has every cron task off, including
// ones spec.config enables explicitly, while app.ini (the cron runner's) keeps them.
func TestHAConfigServerVariantWithoutCron(t *testing.T) {
	f := testInstance()
	f.Spec.Image = img17
	f.Spec.HighAvailability.Enabled = true
	f.Spec.Config = map[string]map[string]string{
		"cron":                 {testCronEnabled: valueTrue},
		"Cron.Archive_Cleanup": {testCronEnabled: valueTrue, "run_at_start": valueTrue, "SCHEDULE": "@every 1m"},
	}
	sec := renderHAConfig(t, f, testValkeyURL)
	full, web := string(sec.Data[forgejo.ConfigSecretKey]), string(sec.Data[forgejo.ConfigSecretKeyWeb])
	if web == "" {
		t.Fatalf("no %s in %v", forgejo.ConfigSecretKeyWeb, sec.Data)
	}
	for _, want := range []string{"ENABLED = false\nRUN_AT_START = false\n"} {
		if got := section(web, "cron"); got != want {
			t.Errorf("app-web.ini [cron] = %q, want %q", got, want)
		}
	}
	if got := section(web, "cron.archive_cleanup"); got != "ENABLED = false\nRUN_AT_START = false\nSCHEDULE = @every 1m\n" {
		t.Errorf("app-web.ini [cron.archive_cleanup] = %q", got)
	}
	if got := section(web, "cron.update_checker"); !strings.Contains(got, "ENABLED = false") {
		t.Errorf("app-web.ini [cron.update_checker] = %q", got)
	}
	if got := section(full, "cron.archive_cleanup"); !strings.Contains(got, "ENABLED = true") || !strings.Contains(got, "run_at_start = true") {
		t.Errorf("app.ini [cron.archive_cleanup] = %q, want the user's settings", got)
	}
	if got := section(full, "cron"); got != "ENABLED = true\n" {
		t.Errorf("app.ini [cron] = %q", got)
	}
	// Apart from the cron sections, both variants are the same.
	strip := func(ini string) string {
		var keep []string
		for block := range strings.SplitSeq(ini, "\n[") {
			if !strings.HasPrefix(block, "cron") {
				keep = append(keep, block)
			}
		}
		return strings.Join(keep, "\n[")
	}
	if strip(full) != strip(web) {
		t.Errorf("variants differ outside [cron*]:\n%s\n---\n%s", strip(full), strip(web))
	}
	for _, ini := range []string{full, web} {
		if got := section(ini, "session"); !strings.Contains(got, "PROVIDER = db") || strings.Contains(got, "redis") {
			t.Errorf("[session] = %q, want sessions in the database", got)
		}
		if got := section(ini, "cache"); !strings.Contains(got, "HOST = redis+sentinel://:pw@fj-valkey-0.fj-valkey.ns.svc.cluster.local:26379,"+
			"fj-valkey-1.fj-valkey.ns.svc.cluster.local:26379/0?mastername=forgejo&sentinelpassword=pw") {
			t.Errorf("[cache] = %q", got)
		}
		if got := section(ini, "queue"); !strings.Contains(got, "/2?mastername=forgejo") || !strings.Contains(got, "TYPE = redis") {
			t.Errorf("[queue] = %q", got)
		}
		if got := section(ini, "server"); !strings.Contains(got, "GRACEFUL_HAMMER_TIME = 50s") {
			t.Errorf("[server] = %q, want GRACEFUL_HAMMER_TIME", got)
		}
	}
	if sec.Annotations[forgejo.AnnotationConfigHash] == "" {
		t.Fatal("no config hash")
	}
}

// Without highAvailability the single pod runs the cron tasks: one app.ini, unchanged.
func TestConfigWithoutHAHasNoServerVariant(t *testing.T) {
	f := testInstance()
	sec := renderHAConfig(t, f, "")
	if _, ok := sec.Data[forgejo.ConfigSecretKeyWeb]; ok || len(sec.Data) != 1 {
		t.Fatalf("keys = %v, want only app.ini", sec.Data)
	}
	if got := section(string(sec.Data[forgejo.ConfigSecretKey]), "cron"); got != "" {
		t.Fatalf("[cron] = %q, want none", got)
	}
}

// The hash covers app-web.ini: a cron-only change rolls the server pods too.
func TestConfigHashCoversServerVariant(t *testing.T) {
	f := testInstance()
	f.Spec.HighAvailability.Enabled = true
	a := renderHAConfig(t, f, testValkeyURL)
	f.Spec.Config = map[string]map[string]string{"cron.git_gc_repos": {testCronEnabled: valueTrue}}
	b := renderHAConfig(t, f, testValkeyURL)
	if a.Annotations[forgejo.AnnotationConfigHash] == b.Annotations[forgejo.AnnotationConfigHash] {
		t.Fatal("hash unchanged by a cron setting")
	}
	if !strings.Contains(string(b.Data[forgejo.ConfigSecretKeyWeb]), "[cron.git_gc_repos]\nENABLED = false") {
		t.Fatalf("app-web.ini:\n%s", b.Data[forgejo.ConfigSecretKeyWeb])
	}
}

func TestValkeyDB(t *testing.T) {
	for in, want := range map[string]string{
		"redis+sentinel://:p@a:1,b:2?mastername=m":  "redis+sentinel://:p@a:1,b:2/2?mastername=m",
		"redis+sentinel://:p@a:1,b:2/?mastername=m": "redis+sentinel://:p@a:1,b:2/2?mastername=m",
		"redis://:p@host:6379":                      "redis://:p@host:6379/2",
	} {
		if got := valkeyDB(in, 2); got != want {
			t.Errorf("valkeyDB(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCronReplicas(t *testing.T) {
	running := func() *forgejov1.Forgejo { f := withHA(testInstance()); f.Status.Image = img17; return f }
	for _, tt := range []struct {
		name string
		f    *forgejov1.Forgejo
		rc   forgejo.ReconcileContext
		web  int32
		hold bool
		want int32
	}{
		{name: "running", f: running(), web: 3, want: 1},
		{name: "single server pod", f: running(), web: 1, want: 1},
		{name: "server scaled to zero", f: running(), web: 0, want: 0},
		{name: "maintenance", f: running(), rc: forgejo.ReconcileContext{Maintenance: true}, want: 0},
		{name: "upgrading", f: running(), rc: forgejo.ReconcileContext{Upgrading: true}, web: 1, want: 0},
		{name: "cold backup", f: running(), rc: forgejo.ReconcileContext{ReplicasOverride: ptr.To[int32](0)}, want: 0},
		{name: "held image", f: heldUpgrade(withHA(testInstance())), rc: forgejo.ReconcileContext{ReplicasOverride: ptr.To[int32](1)}, web: 1, want: 0},
		{name: "restore migration hold", f: running(), web: 1, hold: true, want: 0},
		{name: "first install", f: withHA(testInstance()), web: 1, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rc := tt.rc
			rc.Instance = tt.f
			if got := cronReplicas(&rc, tt.web, tt.hold); got != tt.want {
				t.Fatalf("cronReplicas = %d, want %d", got, tt.want)
			}
		})
	}
}

func haWorkloadContext(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) *forgejo.ReconcileContext {
	t.Helper()
	rc := testContext(t, f, objs...)
	rc.Recorder = events.NewFakeRecorder(20)
	rc.TargetImage = img17
	rc.Layout = forgejo.LayoutForImage(img17)
	rc.ConfigHash, rc.ConfigSecretName = testHAHash, testConfigSecret
	return rc
}

func configVol(t *testing.T, d *appsv1.Deployment) *corev1.SecretVolumeSource {
	t.Helper()
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == configVolume {
			return v.Secret
		}
	}
	t.Fatalf("%s: no config volume", d.Name)
	return nil
}

func getCronRunner(t *testing.T, rc *forgejo.ReconcileContext) *appsv1.StatefulSet {
	t.Helper()
	s := &appsv1.StatefulSet{}
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: rc.Instance.Namespace, Name: forgejo.CronRunnerName(rc.Instance)}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return s
}

func configVolOf(t *testing.T, name string, spec corev1.PodSpec) *corev1.SecretVolumeSource {
	t.Helper()
	for _, v := range spec.Volumes {
		if v.Name == configVolume {
			return v.Secret
		}
	}
	t.Fatalf("%s: no config volume", name)
	return nil
}

func getDeployment(t *testing.T, rc *forgejo.ReconcileContext, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: rc.Instance.Namespace, Name: name}, d); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return d
}

// With highAvailability the workload step runs StatefulSet "<name>-cron": one pod with the
// full app.ini, not selected by the Services; the server pods mount app-web.ini.
func TestReconcileWorkloadCronRunner(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	f.Spec.Replicas = ptr.To[int32](3)
	f.Status.Image = img17
	rc := haWorkloadContext(t, f)
	rc.DataVolumeShared = true
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	web, cron := getDeployment(t, rc, forgejo.DeploymentName(f)), getCronRunner(t, rc)
	if web == nil || cron == nil {
		t.Fatalf("web %v, cron %v", web != nil, cron != nil)
	}
	if *web.Spec.Replicas != 3 || *cron.Spec.Replicas != 1 || cron.Spec.PodManagementPolicy != appsv1.ParallelPodManagement ||
		cron.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType || !metav1.IsControlledBy(cron, f) {
		t.Fatalf("replicas web %d cron %d, cron policy %s strategy %s", *web.Spec.Replicas, *cron.Spec.Replicas,
			cron.Spec.PodManagementPolicy, cron.Spec.UpdateStrategy.Type)
	}
	if got := cron.Spec.Selector.MatchLabels[forgejo.LabelComponent]; got != forgejo.ComponentCron ||
		cron.Spec.Template.Labels[forgejo.LabelComponent] != forgejo.ComponentCron {
		t.Fatalf("cron selector/labels: %v %v", cron.Spec.Selector.MatchLabels, cron.Spec.Template.Labels)
	}
	if cron.Spec.Template.Annotations[forgejo.AnnotationConfigHash] != testHAHash {
		t.Fatalf("cron template annotations %v", cron.Spec.Template.Annotations)
	}
	if items := configVol(t, web).Items; len(items) != 1 || items[0].Key != forgejo.ConfigSecretKeyWeb || items[0].Path != "app.ini" {
		t.Fatalf("server config items = %v", items)
	}
	if items := configVolOf(t, cron.Name, cron.Spec.Template.Spec).Items; len(items) != 0 {
		t.Fatalf("cron config items = %v, want the whole Secret", items)
	}
	wc, cc := web.Spec.Template.Spec.Containers[0], cron.Spec.Template.Spec.Containers[0]
	if wc.Lifecycle == nil || wc.Lifecycle.PreStop == nil || strings.Join(wc.Lifecycle.PreStop.Exec.Command, " ") != "/bin/sleep 10" {
		t.Fatalf("server preStop = %+v", wc.Lifecycle)
	}
	if cc.Lifecycle != nil {
		t.Fatalf("cron preStop = %+v", cc.Lifecycle)
	}
	if strings.Join(cc.Command, " ") != strings.Join(wc.Command, " ") || cc.Image != wc.Image {
		t.Fatalf("cron runs %v %s, server %v %s", cc.Command, cc.Image, wc.Command, wc.Image)
	}
	if g := *web.Spec.Template.Spec.TerminationGracePeriodSeconds; g != terminationPeriod {
		t.Fatalf("grace period %d", g)
	}
	spread := web.Spec.Template.Spec.TopologySpreadConstraints
	if len(spread) != 1 || spread[0].TopologyKey != corev1.LabelHostname || spread[0].WhenUnsatisfiable != corev1.ScheduleAnyway ||
		spread[0].LabelSelector.MatchLabels[forgejo.LabelComponent] != forgejo.ComponentServer {
		t.Fatalf("server spread = %+v", spread)
	}
}

// On a ReadWriteMany volume the cron runner is spread away from the server pods, and an
// unchanged spec changes nothing.
func TestReconcileWorkloadCronRunnerSpreadOnReadWriteMany(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	f.Spec.Replicas = ptr.To[int32](3)
	f.Status.Image = img17
	rc := haWorkloadContext(t, f)
	rc.DataVolumeShared = true
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	cron := getCronRunner(t, rc)
	if s := cron.Spec.Template.Spec.TopologySpreadConstraints; len(s) != 1 || len(s[0].LabelSelector.MatchExpressions) != 1 {
		t.Fatalf("cron spread = %+v", s)
	}
	if a := cron.Spec.Template.Spec.Affinity; a != nil {
		t.Fatalf("cron affinity on a ReadWriteMany volume = %+v", a)
	}
	rv := cron.ResourceVersion
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if got := getCronRunner(t, rc).ResourceVersion; got != rv {
		t.Fatalf("cron runner updated without a change: %s -> %s", rv, got)
	}
}

// On a volume that is not ReadWriteMany, only pods on the node it is attached to can mount
// it: the cron runner requires a node with a server pod, next to the user's own affinity.
func TestCronRunnerSharesAServerNodeOnReadWriteOnce(t *testing.T) {
	f := withHA(testInstance())
	f.Spec.PodTemplate.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"git"}}}}}}}}
	rc := &forgejo.ReconcileContext{Instance: f}
	a := affinity(rc, forgejo.ComponentCron)
	if a == nil || a.NodeAffinity == nil || a.PodAffinity == nil || len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("cron affinity = %+v", a)
	}
	term := a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != corev1.LabelHostname || term.LabelSelector.MatchLabels[forgejo.LabelComponent] != forgejo.ComponentServer ||
		term.LabelSelector.MatchLabels[forgejo.LabelInstance] != f.Name {
		t.Fatalf("cron pod affinity term = %+v", term)
	}
	if f.Spec.PodTemplate.Affinity.PodAffinity != nil {
		t.Fatal("the spec's affinity was modified")
	}
	if s := topologySpread(rc, forgejo.ComponentCron); len(s) != 0 {
		t.Fatalf("cron spread on ReadWriteOnce = %+v, want none (it would push the runner away)", s)
	}
	if a := affinity(rc, forgejo.ComponentServer); a != f.Spec.PodTemplate.Affinity {
		t.Fatalf("server affinity = %+v", a)
	}
	rc.DataVolumeShared = true
	if a := affinity(rc, forgejo.ComponentCron); a != f.Spec.PodTemplate.Affinity {
		t.Fatalf("cron affinity on ReadWriteMany = %+v", a)
	}
}

// An upgrade holds the cron runner at zero; without highAvailability it is removed and the
// server pod mounts the full app.ini.
func TestReconcileWorkloadCronRunnerFollowsHoldsAndHA(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	f.Spec.Replicas = ptr.To[int32](3)
	f.Status.Image = img17
	rc := haWorkloadContext(t, f)
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	// An upgrade holds the cron runner at zero with the server pods' migration.
	rc.Upgrading = true
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if cron := getCronRunner(t, rc); *cron.Spec.Replicas != 0 {
		t.Fatalf("cron replicas during an upgrade = %d", *cron.Spec.Replicas)
	}
	rc.Upgrading = false

	// Without highAvailability: no cron runner, the server pod mounts the full app.ini.
	f.Spec.HighAvailability.Enabled = false
	f.Spec.Replicas = ptr.To[int32](1)
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if getCronRunner(t, rc) != nil {
		t.Fatal("cron runner kept without highAvailability")
	}
	web := getDeployment(t, rc, forgejo.DeploymentName(f))
	if items := configVol(t, web).Items; len(items) != 0 || web.Spec.Template.Spec.Containers[0].Lifecycle != nil ||
		len(web.Spec.Template.Spec.TopologySpreadConstraints) != 0 {
		t.Fatalf("non-HA pod: items %v, lifecycle %+v, spread %v", items, web.Spec.Template.Spec.Containers[0].Lifecycle,
			web.Spec.Template.Spec.TopologySpreadConstraints)
	}
}

// spec.podTemplate.topologySpreadConstraints replace the defaults; zones are added when the
// nodes have them.
func TestTopologySpread(t *testing.T) {
	f := withHA(testInstance())
	rc := &forgejo.ReconcileContext{Instance: f, NodeZones: true}
	got := topologySpread(rc, forgejo.ComponentServer)
	if len(got) != 2 || got[1].TopologyKey != corev1.LabelTopologyZone {
		t.Fatalf("zoned spread = %+v", got)
	}
	own := []corev1.TopologySpreadConstraint{{MaxSkew: 2, TopologyKey: "rack", WhenUnsatisfiable: corev1.DoNotSchedule}}
	f.Spec.PodTemplate.TopologySpreadConstraints = own
	for _, c := range []string{forgejo.ComponentServer, forgejo.ComponentCron} {
		if got := topologySpread(rc, c); len(got) != 1 || got[0].TopologyKey != "rack" {
			t.Fatalf("%s: override ignored: %+v", c, got)
		}
	}
}

// A new image is not rolled out while the cron runner's pod (of the old image) still runs:
// the first pod of the new image migrates the database alone.
func TestReconcileWorkloadWaitsForCronRunnerBeforeNewImage(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	f.Status.Image = img16
	rc := haWorkloadContext(t, f)
	rc.TargetImage, rc.Layout = img16, forgejo.LayoutForImage(img16)
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fj-cron-0", Namespace: f.Namespace, Labels: forgejo.CronSelectorLabels(f)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := rc.Client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	rc.TargetImage, rc.Layout, rc.Upgrading = img17, forgejo.LayoutForImage(img17), true
	res, err := ReconcileWorkload(ctx, rc)
	if err != nil || !res.Stop || res.RequeueAfter == 0 {
		t.Fatalf("res = %+v, err = %v; want a wait", res, err)
	}
	if web := getDeployment(t, rc, forgejo.DeploymentName(f)); deployedImage(web) != img16 {
		t.Fatalf("server image %s rolled out while the cron runner runs", deployedImage(web))
	}
	if cron := getCronRunner(t, rc); *cron.Spec.Replicas != 0 {
		t.Fatalf("cron runner not scaled down: %d", *cron.Spec.Replicas)
	}

	if err := rc.Client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if res, err := ReconcileWorkload(ctx, rc); err != nil || res.Stop {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if web := getDeployment(t, rc, forgejo.DeploymentName(f)); deployedImage(web) != img17 || *web.Spec.Replicas != 1 {
		t.Fatalf("server: image %s, replicas %d", deployedImage(web), *web.Spec.Replicas)
	}
}

// controlledCron is StatefulSet "<name>-cron" of f, controlled by owner.
func controlledCron(t *testing.T, f, owner *forgejov1.Forgejo, replicas int32) *appsv1.StatefulSet {
	t.Helper()
	s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)}}
	if err := controllerutil.SetControllerReference(owner, s, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReportCronRunner(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	cron := controlledCron(t, f, f, 1)
	pod := func(name string, ready corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace, Labels: forgejo.CronSelectorLabels(f)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}}}
	}
	rc := testContext(t, f, cron, pod("fj-cron-a", corev1.ConditionFalse), pod("fj-cron-b", corev1.ConditionTrue))
	if err := reportCronRunner(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if ha := f.Status.HighAvailability; ha == nil || ha.CronRunner != "fj-cron-b" || !ha.CronRunnerReady {
		t.Fatalf("status = %+v", ha)
	}
	f.Spec.HighAvailability.Enabled = false
	if err := reportCronRunner(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if ha := f.Status.HighAvailability; ha.CronRunner != "" || ha.CronRunnerReady {
		t.Fatalf("status without HA = %+v", ha)
	}
}

// A cron runner pod that outlives its deletion deadline (its node is unreachable) is
// reported, with the command that lets a replacement start; the StatefulSet starts none
// before it is gone.
func TestReportCronRunnerWarnsAboutStuckPod(t *testing.T) {
	ctx := context.Background()
	f := withHA(testInstance())
	f.UID = types.UID("uid-stuck")
	stuck := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fj-cron-0", Namespace: f.Namespace, UID: "pod-stuck",
		Labels: forgejo.CronSelectorLabels(f), Finalizers: []string{"test/keep"},
		DeletionTimestamp: ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))},
		Spec:   corev1.PodSpec{NodeName: "node-b"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	rc := testContext(t, f, controlledCron(t, f, f, 1), stuck)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	if err := reportCronRunner(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if ha := f.Status.HighAvailability; ha.CronRunner != "" || ha.CronRunnerReady {
		t.Fatalf("status = %+v, a terminating pod is not the runner", ha)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "CronRunnerStuck") || !strings.Contains(ev, "node-b") ||
			!strings.Contains(ev, "delete pod fj-cron-0 --force --grace-period=0") {
			t.Fatalf("event = %q", ev)
		}
	default:
		t.Fatal("no CronRunnerStuck event")
	}
}

// "<name>-cron" can be another object than the instance's cron runner (a user's, or one an
// operator bug left behind): a new image is rolled out without scaling, counting or waiting
// for it, and it is not reported.
func TestCronRunnerOfAnotherOwnerIsIgnored(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	other := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj-cron", Namespace: f.Namespace, UID: "uid-other"}}
	foreign := controlledCron(t, f, other, 1)
	rc := haWorkloadContext(t, f, foreign)
	rc.TargetImage, rc.Layout = img16, forgejo.LayoutForImage(img16)
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	rc.TargetImage, rc.Layout = img17, forgejo.LayoutForImage(img17)
	if res, err := ReconcileWorkload(ctx, rc); err != nil || res.Stop {
		t.Fatalf("res = %+v, err = %v; want the rollout to go ahead", res, err)
	}
	if web := getDeployment(t, rc, forgejo.DeploymentName(f)); deployedImage(web) != img17 {
		t.Fatalf("server image %s", deployedImage(web))
	}
	got := getCronRunner(t, rc)
	if got == nil || *got.Spec.Replicas != 1 || !metav1.IsControlledBy(got, other) {
		t.Fatalf("foreign StatefulSet changed or deleted: %+v", got)
	}

	f.Spec.HighAvailability.Enabled = true
	if err := reportCronRunner(ctx, rc); err != nil {
		t.Fatal(err)
	}
	// No runner wanted: the gauge is not exported (a fresh child reads 0 and is removed).
	m := &dto.Metric{}
	if err := forgejo.CronRunnerReady.WithLabelValues(f.Namespace, f.Name).Write(m); err != nil || m.GetGauge().GetValue() != 0 {
		t.Fatalf("cron runner gauge = %v, %v", m.GetGauge().GetValue(), err)
	}
	forgejo.CronRunnerReady.DeleteLabelValues(f.Namespace, f.Name)
}
