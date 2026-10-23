package backup_test

// The ForgejoBackup and ForgejoRestore reconcilers in a namespace that is being deleted: the
// API server refuses every create there (forgejo.IsNamespaceTerminating).

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// refuseCreate makes the fake API server answer creates of objects of kind's type with err.
func refuseCreate(kind client.Object, err error) interceptor.Funcs {
	return interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if reflect.TypeOf(obj) == reflect.TypeOf(kind) {
			return err
		}
		return c.Create(ctx, obj, opts...)
	}}
}

func terminating(resource string) error {
	return forgejo.NewNamespaceTerminatingError(resource, "x", ns)
}

func noEvents(t *testing.T, rec *events.FakeRecorder) {
	t.Helper()
	if n := len(rec.Events); n > 0 {
		t.Errorf("%d event(s) emitted into a terminating namespace, e.g. %q", n, <-rec.Events)
	}
}

// A backup whose cleanup Job cannot be created because the namespace is being deleted is
// released at once: the backup volume goes with the namespace. Before, the finalizer held
// the namespace for cleanupTimeout (10 minutes).
func TestBackupFinalizerReleasedInTerminatingNamespace(t *testing.T) {
	s := scheme(t)
	newBackup := func() *forgejov1.ForgejoBackup {
		b := completedBackup("manual")
		b.Status.Path = "/manual"
		b.Finalizers = []string{forgejo.FinalizerName}
		b.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		return b
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: ns}}
	key := types.NamespacedName{Namespace: ns, Name: "manual"}

	rec := events.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&forgejov1.ForgejoBackup{}).
		WithObjects(instance(""), newBackup(), pvc.DeepCopy()).
		WithInterceptorFuncs(refuseCreate(&batchv1.Job{}, terminating("jobs"))).Build()
	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s, Recorder: rec}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("Reconcile = %+v, %v; want done without error", res, err)
	}
	if err := c.Get(context.Background(), key, &forgejov1.ForgejoBackup{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer was not released (err %v)", err)
	}
	noEvents(t, rec)

	// Any other refusal still keeps the finalizer until the cleanup ran or timed out.
	quota := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "x", errors.New("exceeded quota"))
	c = fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&forgejov1.ForgejoBackup{}).
		WithObjects(instance(""), newBackup(), pvc.DeepCopy()).
		WithInterceptorFuncs(refuseCreate(&batchv1.Job{}, quota)).Build()
	r = &controller.ForgejoBackupReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); !apierrors.IsForbidden(err) {
		t.Errorf("quota refusal: err %v, want it returned", err)
	}
	if err := c.Get(context.Background(), key, &forgejov1.ForgejoBackup{}); err != nil {
		t.Errorf("finalizer released on a quota refusal: %v", err)
	}
}

// A new backup in a terminating namespace neither waits for its storage (and then fails
// with a Warning event) nor returns an error that would be retried with backoff.
func TestBackupStartInTerminatingNamespace(t *testing.T) {
	s := scheme(t)
	for name, refused := range map[string]client.Object{
		"backup PVC": &corev1.PersistentVolumeClaim{},
		"backup Job": &batchv1.Job{},
	} {
		t.Run(name, func(t *testing.T) {
			b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: ns, UID: "backup-b1"}}
			b.Spec.ForgejoRef.Name = "fb"
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "fb-db-app", Namespace: ns},
				Data:       map[string][]byte{"username": []byte("forgejo"), testPasswordField: []byte("x")},
			}
			rec := events.NewFakeRecorder(10)
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&forgejov1.ForgejoBackup{}).
				WithObjects(instance(""), b, secret).
				WithInterceptorFuncs(refuseCreate(refused, terminating("x"))).Build()
			r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s, Recorder: rec}
			key := types.NamespacedName{Namespace: ns, Name: b.Name}
			for i := range 2 { // finalizer and labels, then the start
				res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
				if err != nil || res != (ctrl.Result{}) {
					t.Fatalf("pass %d: Reconcile = %+v, %v; want no error and no requeue", i, res, err)
				}
			}
			got := &forgejov1.ForgejoBackup{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != "" {
				t.Errorf("phase %q (%s), want it untouched", got.Status.Phase, got.Status.Message)
			}
			noEvents(t, rec)
		})
	}
}

// A restore whose Job cannot be created in a terminating namespace stays as it is, without
// an error, a retry or an event.
func TestRestoreStartInTerminatingNamespace(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	rs := newRestore("r1", forgejov1.RestorePhaseScalingDown)
	rs.Spec.BackupName = b.Name
	rec := events.NewFakeRecorder(10)
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).
		WithObjects(instance(rs.Name), b, rs).
		WithInterceptorFuncs(refuseCreate(&batchv1.Job{}, terminating("jobs"))).Build()
	r := &controller.ForgejoRestoreReconciler{Client: c, Scheme: s, Recorder: rec}
	for i := range 2 { // finalizer, then the scale-down check and the Job
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: rs.Name}})
		if err != nil || res != (ctrl.Result{}) {
			t.Fatalf("pass %d: Reconcile = %+v, %v; want no error and no requeue", i, res, err)
		}
	}
	got := reconcileRestore(t, c, s, rs.Name, 0)
	if got.Status.Phase != forgejov1.RestorePhaseScalingDown || got.Status.JobName != "" {
		t.Errorf("phase %s job %q (%s), want ScalingDown without a Job", got.Status.Phase, got.Status.JobName, got.Status.Message)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: backup.RestoreJobName(rs)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("restore Job exists (err %v)", err)
	}
	noEvents(t, rec)
}
