package controller

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// stubSteps returns the real step table with every step replaced by a recorder. outcome
// decides a step's result by name; the returned slice collects the steps that ran.
func stubSteps(outcome func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error)) ([]namedStep, *[]string) {
	ran := &[]string{}
	out := make([]namedStep, len(steps))
	for i, s := range steps {
		out[i] = namedStep{name: s.name, needs: s.needs, fn: func(_ context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
			*ran = append(*ran, s.name)
			if outcome == nil {
				return forgejo.StepResult{}, nil
			}
			return outcome(s.name, rc)
		}}
	}
	return out, ran
}

func newRC() *forgejo.ReconcileContext {
	return &forgejo.ReconcileContext{Instance: &forgejov1.Forgejo{}}
}

func TestStepTableNeedsEarlierSteps(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range steps {
		for _, n := range s.needs {
			if !seen[n] {
				t.Errorf("step %q needs %q, which does not run before it", s.name, n)
			}
		}
		if seen[s.name] {
			t.Errorf("duplicate step %q", s.name)
		}
		seen[s.name] = true
	}
}

func TestPipelineAllSucceed(t *testing.T) {
	st, ran := stubSteps(nil)
	p := runPipeline(context.Background(), newRC(), st)
	if len(*ran) != len(steps) || p.err() != nil || p.conflict {
		t.Fatalf("ran %v, err %v, conflict %v", *ran, p.err(), p.conflict)
	}
}

// F45/F26: a failing network step must not stop scheduled backups, health or autopilot.
func TestPipelineIndependentStepsRunAfterError(t *testing.T) {
	boom := errors.New("admission webhook denied the Ingress")
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepNetwork {
			return forgejo.StepResult{}, boom
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	for _, want := range []string{"scaling", stepMonitoring, stepBackupSchedule, stepHealth, stepAutopilot} {
		if !slices.Contains(*ran, want) {
			t.Errorf("%s did not run after the network step failed; ran %v", want, *ran)
		}
	}
	if !errors.Is(p.err(), boom) || !strings.Contains(p.err().Error(), `step "network"`) {
		t.Errorf("err = %v", p.err())
	}
}

// F57: a Valkey that is not ready blocks config and workload, not the restore handshake,
// backups or health.
func TestPipelineValkeyStop(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepHighAvailability {
			return forgejo.StepResult{Stop: true, RequeueAfter: 5 * time.Second}, nil
		}
		return forgejo.StepResult{RequeueAfter: time.Minute}, nil
	})
	rc := newRC()
	p := runPipeline(context.Background(), rc, st)
	want := []string{stepMaintenance, stepRestoreGuard, stepRestoreIdentity, stepSecrets, stepBackupOwnership, stepDatabase, stepHighAvailability,
		stepSSHHostKeys, stepObjectStorage, stepNetwork, stepGateway, stepTLS, stepMonitoring, stepBackupSchedule, stepHealth, stepValkeyStatus,
		stepStorageAutopilot}
	if !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	if p.err() != nil {
		t.Errorf("err = %v", p.err())
	}
	if rc.RequeueAfter != 5*time.Second {
		t.Errorf("requeue = %v, want the Valkey wait", rc.RequeueAfter)
	}
	if got := p.blockedBy("autopilot"); !strings.Contains(got, `"high-availability" is waiting`) {
		t.Errorf("blockedBy(autopilot) = %q", got)
	}
}

// A database that is not ready skips the upgrade step; an upgrade in flight must still be
// visible to the backup schedule.
func TestPipelineInfersUpgradingWhenUpgradeSkipped(t *testing.T) {
	var upgradingAtSchedule bool
	st, ran := stubSteps(func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		switch name {
		case stepDatabase:
			return forgejo.StepResult{Stop: true}, nil
		case stepBackupSchedule:
			upgradingAtSchedule = rc.Upgrading
		}
		return forgejo.StepResult{}, nil
	})
	rc := newRC()
	rc.Instance.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseRollingOut}
	runPipeline(context.Background(), rc, st)
	if slices.Contains(*ran, stepUpgrade) || !slices.Contains(*ran, stepBackupSchedule) {
		t.Fatalf("ran %v", *ran)
	}
	if !upgradingAtSchedule {
		t.Error("rc.Upgrading was false for the backup schedule while status.upgrade is RollingOut")
	}

	rc = newRC()
	rc.Instance.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseSucceeded}
	runPipeline(context.Background(), rc, st)
	if upgradingAtSchedule {
		t.Error("rc.Upgrading was true after a finished upgrade")
	}
}

func TestPipelineConflictEndsPass(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepConfig {
			return forgejo.StepResult{}, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New("stale"))
		}
		return forgejo.StepResult{}, nil
	})
	rc := newRC()
	p := runPipeline(context.Background(), rc, st)
	if !p.conflict || p.err() != nil || (*ran)[len(*ran)-1] != stepConfig || rc.RequeueAfter != time.Second {
		t.Errorf("conflict %v err %v ran %v requeue %v", p.conflict, p.err(), *ran, rc.RequeueAfter)
	}
}

// A database that cannot start (e.g. PostgreSQL on a full CNPG volume) stops the chain, and
// the step that grows that volume must still run. So must the backup ownership step, which
// needs neither the database nor the workload.
func TestPipelineStorageAndOwnershipRunWhenDatabaseStops(t *testing.T) {
	var maintenanceAtStorage bool
	st, ran := stubSteps(func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		switch name {
		case stepMaintenance:
			rc.Maintenance = true
		case stepDatabase:
			return forgejo.StepResult{Stop: true, RequeueAfter: 10 * time.Second}, nil
		case stepStorageAutopilot:
			maintenanceAtStorage = rc.Maintenance
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	for _, want := range []string{stepBackupOwnership, stepStorageAutopilot} {
		if !p.completed[want] {
			t.Errorf("%s did not complete while the database step waits; ran %v", want, *ran)
		}
	}
	for _, skipped := range []string{stepHighAvailability, stepWorkload, stepAutopilot} {
		if slices.Contains(*ran, skipped) {
			t.Errorf("%s ran although the database step stopped", skipped)
		}
	}
	if (*ran)[len(*ran)-1] != stepStorageAutopilot {
		t.Errorf("storage-autopilot is not the last step: ran %v", *ran)
	}
	if !maintenanceAtStorage {
		t.Error("storage-autopilot did not see rc.Maintenance set earlier in the pass")
	}
}

// Even when the very first step fails, the steps without prerequisites run.
func TestPipelineStepsWithoutNeedsRunAfterMaintenanceError(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepMaintenance {
			return forgejo.StepResult{}, errors.New("listing restores: forbidden")
		}
		return forgejo.StepResult{}, nil
	})
	runPipeline(context.Background(), newRC(), st)
	want := []string{stepMaintenance, stepBackupOwnership, stepObjectStorage, stepNetwork, stepGateway, stepTLS, stepMonitoring, stepValkeyStatus,
		stepStorageAutopilot}
	if !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}

// The step table wires the steps the orchestrator names to the feature packages, and
// gives the ownership and storage steps no prerequisites.
func TestStepTableWiring(t *testing.T) {
	byName := map[string]namedStep{}
	order := map[string]int{}
	for i, s := range steps {
		byName[s.name] = s
		order[s.name] = i
	}
	for _, name := range []string{stepBackupOwnership, stepStorageAutopilot, stepObjectStorage, stepNetwork, stepGateway, stepTLS, stepMonitoring, stepMaintenance} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("step %q missing", name)
		}
		if len(s.needs) != 0 {
			t.Errorf("step %q needs %v; it must run on every pass", name, s.needs)
		}
	}
	if order[stepBackupOwnership] != order["secrets"]+1 {
		t.Errorf("backup-ownership is at %d, want right after secrets (%d)", order[stepBackupOwnership], order["secrets"])
	}
	// Pods reference the object storage Secrets: no upgrade, app.ini or rollout without them.
	if !slices.Contains(byName[stepUpgrade].needs, stepObjectStorage) || order[stepObjectStorage] > order[stepUpgrade] {
		t.Errorf("upgrade must need %s (needs %v)", stepObjectStorage, byName[stepUpgrade].needs)
	}
	// spec.restoreFrom: the identity Secrets come from the backup before anything generates
	// them, and the backup's ForgejoRestore needs the database and the data volume.
	if !slices.Equal(byName["secrets"].needs, []string{stepRestoreIdentity}) || order[stepRestoreIdentity] > order["secrets"] {
		t.Errorf("secrets must need %s (needs %v)", stepRestoreIdentity, byName["secrets"].needs)
	}
	if !slices.Equal(byName[stepRestoreFrom].needs, []string{stepStorage}) {
		t.Errorf("%s needs %v, want storage", stepRestoreFrom, byName[stepRestoreFrom].needs)
	}
	if order[stepStorageAutopilot] != len(steps)-1 {
		t.Errorf("storage-autopilot is at %d, want last", order[stepStorageAutopilot])
	}
	if order[stepStorageAutopilot] < order["autopilot"] || order[stepStorageAutopilot] < order["workload"] {
		t.Error("storage-autopilot must run after the workload and autopilot steps")
	}
	for name, want := range map[string]string{
		stepBackupOwnership:  "internal/backup.ReconcileOwnership",
		stepStorageAutopilot: "internal/autopilot.ReconcileStorage",
		stepStorage:          "internal/core.ReconcileStorage",
		stepObjectStorage:    "internal/core.ReconcileObjectStorage",
		stepRestoreIdentity:  "internal/backup.ReconcileRestoreIdentity",
		stepRestoreFrom:      "internal/backup.ReconcileBootstrap",
	} {
		if got := runtime.FuncForPC(reflect.ValueOf(byName[name].fn).Pointer()).Name(); !strings.HasSuffix(got, want) {
			t.Errorf("step %q runs %s, want %s", name, got, want)
		}
	}
}

// gaugeValue reads a gauge (creating the series when it does not exist).
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func testScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A paused instance still exports instance_ready, instance_info{phase="Paused"} and the
// last backup success (no step runs, so nothing else would after an operator restart), and
// an instance that is gone loses its series and in-memory bookkeeping.
func TestPausedReconcileExportsAndDeletionForgetsMetrics(t *testing.T) {
	const ns, name = "pipeline-test-paused", "fj"
	done := metav1.NewTime(time.Unix(1_700_000_000, 0))
	f := &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       forgejov1.ForgejoSpec{Paused: true},
	}
	f.Status.Conditions = []metav1.Condition{{Type: forgejov1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Available", LastTransitionTime: metav1.Now()}}
	f.Status.Backup = &forgejov1.BackupStatusSummary{LastSuccessfulBackupTime: &done}
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithStatusSubresource(f).Build()
	r := &ForgejoReconciler{Client: c, Scheme: s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := &forgejov1.Forgejo{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != forgejov1.PhasePaused {
		t.Errorf("phase = %q, want Paused", got.Status.Phase)
	}
	if v := gaugeValue(t, forgejo.InstanceReady.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("instance_ready = %v, want 1 (Ready=True)", v)
	}
	if v := gaugeValue(t, forgejo.InstanceInfo.WithLabelValues(ns, name, forgejo.Image(f), "", string(forgejov1.PhasePaused))); v != 1 {
		t.Errorf("instance_info{phase=Paused} = %v, want 1", v)
	}
	if v := gaugeValue(t, forgejo.BackupLastSuccess.WithLabelValues(ns, name)); v != float64(done.Unix()) {
		t.Errorf("backup_last_success = %v, want %d", v, done.Unix())
	}

	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if forgejo.InstanceReady.DeleteLabelValues(ns, name) {
		t.Error("instance_ready is still exported for a deleted instance")
	}
	if forgejo.BackupLastSuccess.DeleteLabelValues(ns, name) {
		t.Error("backup_last_success is still exported for a deleted instance")
	}
	// The last-success gauge only moves forward per instance; once the instance is
	// forgotten, a re-created one starts from its own (older) backups.
	earlier := done.Add(-time.Hour)
	backup.RecordLastSuccess(ns, name, earlier)
	if v := gaugeValue(t, forgejo.BackupLastSuccess.WithLabelValues(ns, name)); v != float64(earlier.Unix()) {
		t.Errorf("backup_last_success after re-creation = %v, want %d (ForgetLastSuccess not called)", v, earlier.Unix())
	}
	forgejo.DeleteInstanceMetrics(ns, name)
	backup.ForgetLastSuccess(ns, name)
}

// An instance deleted while its reconcile runs is not reported as a reconcile error.
func TestReconcileInstanceDeletedDuringPass(t *testing.T) {
	const ns, name = "pipeline-test-deleted", "fj"
	f := &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       forgejov1.ForgejoSpec{Paused: true},
	}
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithStatusSubresource(f).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Group: forgejov1.GroupVersion.Group, Resource: "forgejos"}, obj.GetName())
			},
		}).Build()
	r := &ForgejoReconciler{Client: c, Scheme: s}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil || res.RequeueAfter != 0 {
		t.Errorf("Reconcile = %+v, %v; want no error and no requeue", res, err)
	}
	forgejo.DeleteInstanceMetrics(ns, name)
}

// A failure inside the chain skips its dependants but keeps the failure as the reason.
func TestPipelineBlockedByNamesRootCause(t *testing.T) {
	st, _ := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepMaintenance {
			return forgejo.StepResult{}, errors.New("listing restores: forbidden")
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	if p.completed[stepBackupSchedule] {
		t.Fatal("backup schedule ran without maintenance")
	}
	if got := p.blockedBy(stepBackupSchedule); !strings.Contains(got, `step "maintenance" failed: listing restores: forbidden`) {
		t.Errorf("blockedBy = %q", got)
	}
}
