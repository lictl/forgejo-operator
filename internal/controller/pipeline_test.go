package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
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
		out[i] = namedStep{name: s.name, needs: s.needs, waitWhileBroken: s.waitWhileBroken, fn: func(_ context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
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

func TestPipelineAllSucceed(t *testing.T) {
	st, ran := stubSteps(nil)
	p := runPipeline(context.Background(), newRC(), st)
	if len(*ran) != len(steps) || p.err() != nil || p.conflict {
		t.Fatalf("ran %v, err %v, conflict %v", *ran, p.err(), p.conflict)
	}
}

// A failing network step must not stop scheduled backups, health or autopilot.
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

// A Valkey that is not ready blocks config and workload, not the restore handshake,
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
	want := []string{stepMaintenance, stepRestoreGuard, stepRestoreIdentity, stepIdentity, stepSecrets, stepBackupOwnership, stepOwnership, stepDatabaseUpgrade,
		stepDatabase, stepHighAvailability, stepSSHHostKeys, stepObjectStorage, stepNetwork, stepGateway, stepTLS, stepMonitoring, stepBackupSchedule, stepHealth,
		stepValkeyStatus, stepStorageAutopilot}
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

// A conflict ends the pass and retries a second later; so does a create refused because the
// namespace is being deleted, without a retry. Neither is a step failure.
func TestPipelineEndsPass(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		step                  string
		err                   error
		conflict, terminating bool
		requeue               time.Duration
	}{
		{name: "conflict", step: stepConfig, conflict: true, requeue: time.Second,
			err: apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New("stale"))},
		{name: "namespace terminating", step: stepStorage, terminating: true,
			err: fmt.Errorf("creating PVC: %w", forgejo.NewNamespaceTerminatingError("persistentvolumeclaims", "fj-data", "ns"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
				if name == tc.step {
					return forgejo.StepResult{}, tc.err
				}
				return forgejo.StepResult{}, nil
			})
			rc := newRC()
			p := runPipeline(context.Background(), rc, st)
			if p.conflict != tc.conflict || p.terminating != tc.terminating || p.err() != nil || len(p.failed) != 0 ||
				(*ran)[len(*ran)-1] != tc.step || rc.RequeueAfter != tc.requeue {
				t.Errorf("conflict %v terminating %v err %v ran %v requeue %v", p.conflict, p.terminating, p.err(), *ran, rc.RequeueAfter)
			}
		})
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

// The retain policy must reach the data volume, the cluster and the identity Secrets whatever
// else happens: a Broken instance (the identity step stops), a database that cannot start and
// a failing step before it.
func TestPipelineOwnershipRunsWhenTheChainStops(t *testing.T) {
	for name, tc := range map[string]struct {
		step   string
		result forgejo.StepResult
		err    error
	}{
		"identity stops (Broken)": {step: stepIdentity, result: forgejo.StepResult{Stop: true, RequeueAfter: time.Minute}},
		"database stops":          {step: stepDatabase, result: forgejo.StepResult{Stop: true}},
		"secrets fail":            {step: stepSecrets, err: errors.New("admin.existingSecret is missing")},
		// A conflict in any step ends the pass: the ownership step is early enough not to depend on it.
		"a later step conflicts": {step: stepWorkload, err: apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "fj", errors.New("changed"))},
	} {
		t.Run(name, func(t *testing.T) {
			st, _ := stubSteps(func(step string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
				if step == tc.step {
					return tc.result, tc.err
				}
				return forgejo.StepResult{}, nil
			})
			if p := runPipeline(context.Background(), newRC(), st); !p.completed[stepOwnership] {
				t.Errorf("%s did not complete: skipped %v failed %v", stepOwnership, p.skipped, p.failed)
			}
		})
	}
}

func ownerRefTo(f *forgejov1.Forgejo) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo", Name: f.Name, UID: f.UID,
		Controller: &yes, BlockOwnerDeletion: &yes}}
}

// Switching a Forgejo from retainPolicy Delete to Retain protects its data volume even when the
// instance cannot run the steps that own it: paused, or Broken because the identity Secret is
// gone (the case in which deleting the Forgejo is the documented way out). Reproduces the loss
// of the repositories by garbage collection that the owning steps alone left open.
func TestRetainPolicyReachesTheDataVolumeOfPausedAndBrokenInstances(t *testing.T) {
	for name, tc := range map[string]struct {
		paused bool
		image  string
	}{
		"paused": {paused: true},
		// status.image is set: the instance has run, and without Secret fj-secrets the identity
		// step reports Broken and stops the chain.
		"broken": {image: "codeberg.org/forgejo/forgejo:15"},
	} {
		t.Run(name, func(t *testing.T) {
			const ns = "retain-test"
			f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fj", UID: "fj-uid"},
				Spec: forgejov1.ForgejoSpec{Paused: tc.paused}}
			f.Status.Image = tc.image
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: forgejo.DataPVCName(f),
				OwnerReferences: ownerRefTo(f)}}
			s := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, pvc).WithStatusSubresource(f).Build()
			r := &ForgejoReconciler{Client: c, Scheme: s}
			key := types.NamespacedName{Namespace: ns, Name: f.Name}
			// Errors of unrelated steps do not matter; the data volume does.
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})

			got := &corev1.PersistentVolumeClaim{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), got); err != nil {
				t.Fatal(err)
			}
			if forgejo.HasInstanceRef(got.OwnerReferences, f) {
				t.Fatalf("the data volume still belongs to the Forgejo: %+v", got.OwnerReferences)
			}
			fj := &forgejov1.Forgejo{}
			if err := c.Get(context.Background(), key, fj); err != nil {
				t.Fatal(err)
			}
			if !tc.paused && !meta.IsStatusConditionTrue(fj.Status.Conditions, forgejov1.ConditionBroken) {
				t.Fatalf("the instance is not Broken, so the test does not cover that case: %+v", fj.Status.Conditions)
			}
			if !meta.IsStatusConditionTrue(fj.Status.Conditions, forgejov1.ConditionRetainPolicyApplied) {
				t.Errorf("RetainPolicyApplied is not True: %+v", fj.Status.Conditions)
			}
		})
	}
}

// Even when the very first step fails, the steps without prerequisites run, and a step it
// blocks through the chain names that failure.
func TestPipelineStepsWithoutNeedsRunAfterMaintenanceError(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepMaintenance {
			return forgejo.StepResult{}, errors.New("listing restores: forbidden")
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	want := []string{stepMaintenance, stepBackupOwnership, stepOwnership, stepObjectStorage, stepNetwork, stepGateway, stepTLS, stepMonitoring,
		stepValkeyStatus, stepStorageAutopilot}
	if !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	// backup-schedule needs restore-guard, which needs maintenance.
	if got := p.blockedBy(stepBackupSchedule); !strings.Contains(got, `step "maintenance" failed: listing restores: forbidden`) {
		t.Errorf("blockedBy = %q", got)
	}
}

// Every step needs only steps before it. The step table wires the steps the orchestrator
// names to the feature packages, and gives the ownership and storage steps no prerequisites.
func TestStepTable(t *testing.T) {
	byName := map[string]namedStep{}
	order := map[string]int{}
	for i, s := range steps {
		for _, n := range s.needs {
			if _, ok := byName[n]; !ok {
				t.Errorf("step %q needs %q, which does not run before it", s.name, n)
			}
		}
		if _, dup := byName[s.name]; dup {
			t.Errorf("duplicate step %q", s.name)
		}
		byName[s.name] = s
		order[s.name] = i
	}
	for _, name := range []string{stepBackupOwnership, stepOwnership, stepStorageAutopilot, stepObjectStorage, stepNetwork, stepGateway, stepTLS,
		stepMonitoring, stepMaintenance} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("step %q missing", name)
		}
		if len(s.needs) != 0 {
			t.Errorf("step %q needs %v; it must run on every pass", name, s.needs)
		}
	}
	// Pods reference the object storage Secrets: no upgrade, app.ini or rollout without them.
	if !slices.Contains(byName[stepUpgrade].needs, stepObjectStorage) {
		t.Errorf("upgrade must need %s (needs %v)", stepObjectStorage, byName[stepUpgrade].needs)
	}
	// spec.restoreFrom: the identity Secrets come from the backup before anything generates
	// them, and the backup's ForgejoRestore needs the database and the data volume. The identity
	// check comes between them, before anything generates or creates identity or data.
	if !slices.Equal(byName[stepIdentity].needs, []string{stepRestoreIdentity}) {
		t.Errorf("identity must need %s (needs %v)", stepRestoreIdentity, byName[stepIdentity].needs)
	}
	if !slices.Equal(byName[stepSecrets].needs, []string{stepIdentity}) {
		t.Errorf("secrets must need %s (needs %v)", stepIdentity, byName[stepSecrets].needs)
	}
	for _, name := range []string{stepDatabaseUpgrade, stepBackupSchedule} {
		if !slices.Contains(byName[name].needs, stepIdentity) {
			t.Errorf("%s must need %s (needs %v)", name, stepIdentity, byName[name].needs)
		}
	}
	if !slices.Equal(byName[stepRestoreFrom].needs, []string{stepStorage}) {
		t.Errorf("%s needs %v, want storage", stepRestoreFrom, byName[stepRestoreFrom].needs)
	}
	// Last: it sees this pass's maintenance and upgrade flags, and its restart follows the
	// workload update.
	if order[stepStorageAutopilot] != len(steps)-1 {
		t.Errorf("storage-autopilot is at %d, want last", order[stepStorageAutopilot])
	}
	for name, want := range map[string]string{
		stepBackupOwnership:  "internal/backup.ReconcileOwnership",
		stepOwnership:        "internal/core.ReconcileOwnership",
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

// Neither a pass in a namespace that is being deleted (as in kube-controller-manager) nor a
// pass whose instance is deleted while it runs is a reconcile error: no error (no backoff),
// no requeue, no Warning event and no Degraded condition.
func TestReconcileEndsQuietly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
		funcs  interceptor.Funcs
	}{
		{name: "namespace terminating", funcs: interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return forgejo.NewNamespaceTerminatingError("x", obj.GetName(), obj.GetNamespace())
			},
		}},
		// Paused, so that the status patch is the only write.
		{name: "instance deleted during the pass", paused: true, funcs: interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Group: forgejov1.GroupVersion.Group, Resource: "forgejos"}, obj.GetName())
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const ns, name = "pipeline-test-quiet", "fj"
			f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: forgejov1.ForgejoSpec{Paused: tc.paused}}
			s := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithStatusSubresource(f).WithInterceptorFuncs(tc.funcs).Build()
			rec := events.NewFakeRecorder(10)
			r := &ForgejoReconciler{Client: c, Scheme: s, Recorder: rec}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
			defer forgetInstance(ns, name)

			res, err := r.Reconcile(context.Background(), req)
			if err != nil || res != (ctrl.Result{}) {
				t.Errorf("Reconcile = %+v, %v; want no error and no requeue", res, err)
			}
			if n := len(rec.Events); n > 0 {
				t.Errorf("%d event(s) emitted, e.g. %q", n, <-rec.Events)
			}
			got := &forgejov1.Forgejo{}
			if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			if c := meta.FindStatusCondition(got.Status.Conditions, forgejov1.ConditionDegraded); c != nil {
				t.Errorf("Degraded set: %+v", c)
			}
		})
	}
}

// While the Cluster of a PostgreSQL upgrade in flight is being deleted, the database-upgrade
// step stops: the database step (and the chain) is skipped, the independent steps run, and the
// upgrade in flight is visible to the backup schedule. The same holds when the step fails. (A
// Cluster that is gone ends the upgrade with failHeld, which also stops the step.)
func TestPipelineDatabaseUpgradeStopsTheDatabaseStep(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var upgradingAtSchedule, dbUpgradingAtSchedule bool
		st, ran := stubSteps(func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
			switch name {
			case stepDatabaseUpgrade:
				if fail {
					return forgejo.StepResult{}, errors.New("reading CNPG cluster: timeout")
				}
				rc.Upgrading, rc.DatabaseUpgrading = true, true
				return forgejo.StepResult{Stop: true}, nil
			case stepBackupSchedule:
				upgradingAtSchedule, dbUpgradingAtSchedule = rc.Upgrading, rc.DatabaseUpgrading
			}
			return forgejo.StepResult{}, nil
		})
		rc := newRC()
		rc.Instance.Status.Database = &forgejov1.DatabaseStatus{Upgrade: &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeReverting}}
		runPipeline(context.Background(), rc, st)
		for _, skipped := range []string{stepDatabase, stepHighAvailability, stepUpgrade, stepWorkload} {
			if slices.Contains(*ran, skipped) {
				t.Errorf("fail=%v: %s ran although database-upgrade did not complete", fail, skipped)
			}
		}
		for _, want := range []string{stepBackupSchedule, stepHealth, stepStorageAutopilot} {
			if !slices.Contains(*ran, want) {
				t.Errorf("fail=%v: %s did not run", fail, want)
			}
		}
		if !upgradingAtSchedule || !dbUpgradingAtSchedule {
			t.Errorf("fail=%v: the backup schedule saw upgrading=%v, database upgrading=%v", fail, upgradingAtSchedule, dbUpgradingAtSchedule)
		}
	}
}

// A Broken instance (here a failed PostgreSQL upgrade that holds Forgejo, which does not stop the
// identity step) takes no scheduled backups, prunes none and grows no volume.
func TestPipelineBrokenStopsBackupsAndVolumeGrowth(t *testing.T) {
	st, ran := stubSteps(func(string, *forgejo.ReconcileContext) (forgejo.StepResult, error) { return forgejo.StepResult{}, nil })
	rc := newRC()
	rc.SetBroken(forgejo.BrokenDatabaseUpgradeFailed, "held")
	p := runPipeline(context.Background(), rc, st)
	for _, name := range []string{stepBackupSchedule, stepStorageAutopilot} {
		if slices.Contains(*ran, name) || !slices.Contains(p.stopped, name) {
			t.Errorf("%s ran on a Broken instance", name)
		}
	}
	if !slices.Contains(*ran, stepHealth) || !slices.Contains(*ran, stepDatabase) {
		t.Errorf("health or database did not run: %v", *ran)
	}
}
