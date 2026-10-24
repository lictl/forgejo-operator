package observability

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const testSeverityWarning = "warning"

func monitoringRC(t *testing.T, f *forgejov1.Forgejo, funcs interceptor.Funcs) (*forgejo.ReconcileContext, *events.FakeRecorder) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{forgejo.ServiceMonitorGVK, forgejo.PrometheusRuleGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithInterceptorFuncs(funcs).Build()
	rec := events.NewFakeRecorder(20)
	return &forgejo.ReconcileContext{Client: c, Scheme: s, Recorder: rec, RESTMapper: mapper, Instance: f}, rec
}

func testInstance(name string) *forgejov1.Forgejo {
	return &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: types.UID("uid-" + name)}}
}

func exists(t *testing.T, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

func hasRule(u *unstructured.Unstructured, alert string) bool {
	groups, _, _ := unstructured.NestedSlice(u.Object, "spec", "groups")
	for _, g := range groups {
		rules, _, _ := unstructured.NestedSlice(g.(map[string]any), "rules")
		for _, r := range rules {
			if r.(map[string]any)["alert"] == alert {
				return true
			}
		}
	}
	return false
}

func degraded(rc *forgejo.ReconcileContext) *metav1.Condition {
	return meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
}

func TestMonitoringFailuresDoNotStopThePipeline(t *testing.T) {
	ctx := context.Background()
	f := testInstance("bad-interval")
	f.Spec.Monitoring.ServiceMonitor = &forgejov1.PrometheusResourceSpec{Interval: "1min"}
	rc, rec := monitoringRC(t, f, interceptor.Funcs{})

	res, err := ReconcileMonitoring(ctx, rc)
	if err != nil {
		t.Fatalf("step returned %v; optional monitoring must not stop the pipeline", err)
	}
	if rc.RequeueAfter != monitoringRetry || res.Stop {
		t.Fatalf("requeue %s stop %v", rc.RequeueAfter, res.Stop)
	}
	c := degraded(rc)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonMonitoringFailed || !strings.Contains(c.Message, `"1min"`) {
		t.Fatalf("Degraded %+v", c)
	}
	if exists(t, rc, forgejo.ServiceMonitorGVK, "bad-interval") != nil {
		t.Fatal("invalid ServiceMonitor was created")
	}
	pr := exists(t, rc, forgejo.PrometheusRuleGVK, "bad-interval")
	if pr == nil {
		t.Fatal("PrometheusRule not created next to the failing ServiceMonitor")
	}
	if hasRule(pr, "ForgejoDown") || !hasRule(pr, "ForgejoVolumeAlmostFull") {
		t.Fatal("ForgejoDown must only be rendered while a ServiceMonitor exists")
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], ReasonMonitoringFailed) {
		t.Fatalf("events %v", ev)
	}

	// The same failure again: no new event.
	if _, err := ReconcileMonitoring(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if ev := drain(rec); len(ev) != 0 {
		t.Fatalf("repeated events %v", ev)
	}

	// Fixed: everything is applied and Degraded is cleared.
	f.Spec.Monitoring.ServiceMonitor.Interval = "2m"
	if _, err := ReconcileMonitoring(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if c := degraded(rc); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded not cleared: %+v", c)
	}
	if exists(t, rc, forgejo.ServiceMonitorGVK, "bad-interval") == nil {
		t.Fatal("ServiceMonitor missing")
	}
	if !hasRule(exists(t, rc, forgejo.PrometheusRuleGVK, "bad-interval"), "ForgejoDown") {
		t.Fatal("ForgejoDown missing once the ServiceMonitor exists")
	}
}

func TestMonitoringAPIErrors(t *testing.T) {
	ctx := context.Background()
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}, "x",
		field.ErrorList{field.Invalid(field.NewPath("spec", "endpoints").Index(0).Child("interval"), "1min", "bad")})
	isKind := func(obj client.Object, kind string) bool {
		u, ok := obj.(*unstructured.Unstructured)
		return ok && u.GetKind() == kind
	}

	t.Run("rejected ServiceMonitor", func(t *testing.T) {
		rc, _ := monitoringRC(t, testInstance("rejected"), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if isKind(obj, "ServiceMonitor") {
					return invalid
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		if _, err := ReconcileMonitoring(ctx, rc); err != nil {
			t.Fatalf("step returned %v", err)
		}
		if c := degraded(rc); c == nil || c.Reason != ReasonMonitoringFailed || !strings.Contains(c.Message, "ServiceMonitor rejected") {
			t.Fatalf("Degraded %+v", c)
		}
		if exists(t, rc, forgejo.PrometheusRuleGVK, "rejected") == nil {
			t.Fatal("PrometheusRule not created")
		}
	})

	t.Run("conflict is returned for a quick retry", func(t *testing.T) {
		rc, _ := monitoringRC(t, testInstance("conflict"), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if isKind(obj, "PrometheusRule") {
					return apierrors.NewConflict(schema.GroupResource{Resource: "prometheusrules"}, "conflict", nil)
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		if _, err := ReconcileMonitoring(ctx, rc); !apierrors.IsConflict(err) {
			t.Fatalf("got %v, want a conflict", err)
		}
	})

	t.Run("terminating namespace ends the pass", func(t *testing.T) {
		rc, rec := monitoringRC(t, testInstance("terminating"), interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return forgejo.NewNamespaceTerminatingError("x", obj.GetName(), "ns")
			},
		})
		if _, err := ReconcileMonitoring(ctx, rc); !forgejo.IsNamespaceTerminating(err) {
			t.Fatalf("got %v, want the namespace-terminating error", err)
		}
		if c := degraded(rc); c != nil {
			t.Fatalf("Degraded %+v", c)
		}
		if ev := drain(rec); len(ev) != 0 {
			t.Fatalf("events %v", ev)
		}
	})

	t.Run("owned by someone else", func(t *testing.T) {
		f := testInstance("taken")
		rc, _ := monitoringRC(t, f, interceptor.Funcs{})
		other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "taken-dashboard", UID: "other",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "x", UID: "x", Controller: new(true)}}}}
		if err := rc.Client.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		f.Spec.Monitoring.GrafanaDashboard = &forgejov1.PrometheusResourceSpec{Enabled: new(true)}
		if _, err := ReconcileMonitoring(ctx, rc); err != nil {
			t.Fatalf("step returned %v", err)
		}
		if c := degraded(rc); c == nil || c.Reason != ReasonMonitoringFailed || !strings.Contains(c.Message, "taken-dashboard") {
			t.Fatalf("Degraded %+v", c)
		}
		if exists(t, rc, forgejo.ServiceMonitorGVK, "taken") == nil || exists(t, rc, forgejo.PrometheusRuleGVK, "taken") == nil {
			t.Fatal("ServiceMonitor/PrometheusRule not applied")
		}
	})
}

func TestMonitoringInvalidLabels(t *testing.T) {
	f := testInstance("labels")
	f.Spec.Monitoring.PrometheusRule = &forgejov1.PrometheusResourceSpec{Labels: map[string]string{"release": "kube prometheus"}}
	rc, _ := monitoringRC(t, f, interceptor.Funcs{})
	if _, err := ReconcileMonitoring(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if c := degraded(rc); c == nil || !strings.Contains(c.Message, `label "release" value "kube prometheus"`) {
		t.Fatalf("Degraded %+v", c)
	}
	if exists(t, rc, forgejo.PrometheusRuleGVK, "labels") != nil {
		t.Fatal("PrometheusRule with invalid labels was applied")
	}
	if exists(t, rc, forgejo.ServiceMonitorGVK, "labels") == nil {
		t.Fatal("ServiceMonitor not applied")
	}
}

func TestMonitoringKeepsOtherDegradedReasons(t *testing.T) {
	f := testInstance("other")
	f.Spec.Monitoring.ServiceMonitor = &forgejov1.PrometheusResourceSpec{Interval: "soon"}
	rc, _ := monitoringRC(t, f, interceptor.Funcs{})
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, "ConfigRolledBack", "rolled back")
	if _, err := ReconcileMonitoring(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if c := degraded(rc); c.Reason != "ConfigRolledBack" {
		t.Fatalf("Degraded overwritten: %+v", c)
	}

	// A stale orchestrator ReconcileError is replaced.
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, "ReconcileError", `step "monitoring": ...`)
	if _, err := ReconcileMonitoring(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if c := degraded(rc); c.Reason != ReasonMonitoringFailed {
		t.Fatalf("Degraded %+v", c)
	}
}

func ruleSpecOf(t *testing.T, f *forgejov1.Forgejo, scraped bool) *ruleSpec {
	t.Helper()
	spec, err := prometheusRuleSpec(f, scraped)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// findRule returns the first rule of the alert in any group.
func findRule(spec *ruleSpec, alert string) *rule {
	for g := range spec.Groups {
		for i := range spec.Groups[g].Rules {
			if r := &spec.Groups[g].Rules[i]; r.Alert == alert {
				return r
			}
		}
	}
	return nil
}

func TestNotReadyAlertFollowsDesiredReplicas(t *testing.T) {
	hpa := func(f *forgejov1.Forgejo) {
		f.Spec.HighAvailability.Enabled = true
		f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: 0, MaxReplicas: 3}
	}
	for _, tt := range []struct {
		name     string
		replicas *int32
		hpa      bool
		want     bool
	}{
		{name: "default replicas", want: true},
		{name: "two replicas", replicas: new(int32(2)), want: true},
		{name: "scaled to zero", replicas: new(int32(0)), want: false},
		{name: "zero replicas with HPA", replicas: new(int32(0)), hpa: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := testInstance("replicas")
			f.Spec.Replicas = tt.replicas
			if tt.hpa {
				hpa(f)
			}
			spec := ruleSpecOf(t, f, true)
			if got := findRule(spec, "ForgejoNotReady") != nil; got != tt.want {
				t.Fatalf("ForgejoNotReady rendered=%v, want %v", got, tt.want)
			}
			if got := findRule(spec, "ForgejoDown") != nil; got != tt.want {
				t.Fatalf("ForgejoDown rendered=%v, want %v", got, tt.want)
			}
			// The guard and the unreachable alert do not depend on the replica count.
			if findRule(spec, "ForgejoOperatorMetricsMissing") == nil || findRule(spec, "ForgejoHealthProbeUnreachable") == nil {
				t.Fatal("ForgejoOperatorMetricsMissing/ForgejoHealthProbeUnreachable missing")
			}
		})
	}
}

func TestOperatorMetricsGuard(t *testing.T) {
	f := testInstance("guard")
	f.Spec.Backup.Enabled = true
	f.Spec.Backup.Schedule = testDailySchedule
	spec := ruleSpecOf(t, f, false)

	guard := findRule(spec, "ForgejoOperatorMetricsMissing")
	if guard == nil || guard.Expr != `absent(forgejo_operator_instance_ready{namespace="ns", name="guard"})` || guard.For != "15m" {
		t.Fatalf("guard %+v", guard)
	}
	if !strings.Contains(guard.Annotations["description"], "honorLabels") {
		t.Fatalf("guard description %q", guard.Annotations["description"])
	}
	// A missing backup is only reported while the instance's operator metrics are scraped.
	missing := findRule(spec, "ForgejoBackupMissing")
	want := `absent(forgejo_operator_backup_last_success_timestamp_seconds{namespace="ns", name="guard"}) and on() ` +
		`count(forgejo_operator_instance_ready{namespace="ns", name="guard"}) > 0`
	if missing == nil || missing.Expr != want || missing.For != "2d" {
		t.Fatalf("ForgejoBackupMissing %+v", missing)
	}

	unreachable := findRule(spec, "ForgejoHealthProbeUnreachable")
	if unreachable == nil || unreachable.For != "15m" || unreachable.Expr !=
		`increase(forgejo_operator_health_check_unreachable_total{namespace="ns", name="guard"}[5m]) > 0` {
		t.Fatalf("ForgejoHealthProbeUnreachable %+v", unreachable)
	}
	if d := unreachable.Annotations["description"]; !strings.Contains(d, "guard-http port 3000") {
		t.Fatalf("ForgejoHealthProbeUnreachable description %q", d)
	}
	// A slow probe interval widens the window so that it always contains a probe.
	f.Spec.AutoPilot.SelfHealing.HealthCheckInterval = &metav1.Duration{Duration: 10 * time.Minute}
	if r := findRule(ruleSpecOf(t, f, false), "ForgejoHealthProbeUnreachable"); !strings.Contains(r.Expr, "[20m]") {
		t.Fatalf("expr %q", r.Expr)
	}
}

func TestVersionUnsupportedAlert(t *testing.T) {
	r := findRule(ruleSpecOf(t, testInstance("ver"), false), "ForgejoVersionUnsupported")
	want := `max by (namespace, name, major, support) (forgejo_operator_version_supported{namespace="ns", name="ver"}) == 0`
	if r == nil || r.Expr != want || r.For != "1h" || r.Labels["severity"] != testSeverityWarning {
		t.Fatalf("ForgejoVersionUnsupported %+v", r)
	}
	if d := r.Annotations["description"]; !strings.Contains(d, "VersionSupported") || !strings.Contains(d, "docs/versions.md") {
		t.Fatalf("description %q", d)
	}
	r = findRule(ruleSpecOf(t, testInstance("ver"), false), "ForgejoVersionAdvisory")
	want = `max by (namespace, name, major) (forgejo_operator_version_advisory{namespace="ns", name="ver"}) == 1`
	if r == nil || r.Expr != want || r.For != "1h" || r.Labels["severity"] != testSeverityWarning {
		t.Fatalf("ForgejoVersionAdvisory %+v", r)
	}
}

// Upload alerts exist only with spec.backup.remote; staleness ones only with scheduled backups.
func TestBackupUploadAlerts(t *testing.T) {
	f := testInstance("up")
	for _, name := range []string{"ForgejoBackupUploadFailed", "ForgejoBackupUploadTooOld", "ForgejoBackupUploadMissing"} {
		if findRule(ruleSpecOf(t, f, false), name) != nil {
			t.Errorf("%s without spec.backup.remote", name)
		}
	}
	f.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{}
	r := findRule(ruleSpecOf(t, f, false), "ForgejoBackupUploadFailed")
	if r == nil || !strings.Contains(r.Expr, `forgejo_operator_backup_uploads_total{namespace="ns", name="up", result="failed"}`) {
		t.Fatalf("upload failed rule %+v", r)
	}
	if findRule(ruleSpecOf(t, f, false), "ForgejoBackupUploadTooOld") != nil {
		t.Error("staleness alert without scheduled backups")
	}
	f.Spec.Backup.Enabled = true
	f.Spec.Backup.Schedule = testDailySchedule
	if r := findRule(ruleSpecOf(t, f, false), "ForgejoBackupUploadTooOld"); r == nil ||
		!strings.Contains(r.Expr, "forgejo_operator_backup_last_upload_success_timestamp_seconds") {
		t.Errorf("too old rule %+v", r)
	}
	if r := findRule(ruleSpecOf(t, f, false), "ForgejoBackupUploadMissing"); r == nil || !strings.Contains(r.Expr, "absent(") {
		t.Errorf("missing rule %+v", r)
	}
}

func TestRunnersOfflineAlert(t *testing.T) {
	r := findRule(ruleSpecOf(t, testInstance("run"), false), "ForgejoRunnersOffline")
	want := `max by (namespace, name, runner) (forgejo_operator_runner_jobs_waiting{namespace="ns", name="run"}) > 0 ` +
		`and on (namespace, name, runner) max by (namespace, name, runner) (forgejo_operator_runners_online{namespace="ns", name="run"}) == 0 ` +
		`and on (namespace, name, runner) max by (namespace, name, runner) (forgejo_operator_runners_desired{namespace="ns", name="run"}) > 0`
	if r == nil || r.Expr != want || r.For != "10m" || r.Labels["severity"] != testSeverityWarning {
		t.Fatalf("ForgejoRunnersOffline %+v", r)
	}
	if d := r.Annotations["description"]; !strings.Contains(d, "kubectl -n ns describe forgejorunner {{ $labels.runner }}") ||
		!strings.Contains(d, `{{ $value | printf "%.0f" }}`) {
		t.Fatalf("description %q", d)
	}
}

// With highAvailability: alerts for a Valkey without a usable primary, a failover, a
// degraded group and a cron runner that is down; none without it.
func TestHighAvailabilityAlerts(t *testing.T) {
	names := []string{"ForgejoValkeyNoMaster", "ForgejoValkeyFailedOver", "ForgejoValkeyDegraded", "ForgejoCronRunnerDown"}
	f := testInstance("ha")
	for _, n := range names {
		if findRule(ruleSpecOf(t, f, false), n) != nil {
			t.Fatalf("%s without highAvailability", n)
		}
	}
	f.Spec.HighAvailability.Enabled = true
	spec := ruleSpecOf(t, f, false)
	for _, n := range names {
		if findRule(spec, n) == nil {
			t.Fatalf("no %s", n)
		}
	}
	if r := findRule(spec, "ForgejoValkeyNoMaster"); r.Labels["severity"] != "critical" ||
		!strings.Contains(r.Expr, `forgejo_operator_valkey_master_available{namespace="ns", name="ha"}`) {
		t.Fatalf("ForgejoValkeyNoMaster: %+v", r)
	}
	if r := findRule(spec, "ForgejoCronRunnerDown"); !strings.Contains(r.Expr, "forgejo_operator_cron_runner_ready") || r.For == "" {
		t.Fatalf("ForgejoCronRunnerDown: %+v", r)
	}
}
