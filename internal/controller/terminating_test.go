package controller

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// A create refused because the namespace is being deleted ends the pass, like a conflict,
// and is not a step failure.
func TestPipelineNamespaceTerminatingEndsPass(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepStorage {
			return forgejo.StepResult{}, fmt.Errorf("creating PVC: %w", forgejo.NewNamespaceTerminatingError("persistentvolumeclaims", "fj-data", "ns"))
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	if !p.terminating || p.err() != nil || len(p.failed) != 0 || (*ran)[len(*ran)-1] != stepStorage {
		t.Errorf("terminating %v err %v failed %v ran %v", p.terminating, p.err(), p.failed, *ran)
	}
}

// The real pipeline in a terminating namespace: no error (no backoff), no requeue, no
// Warning event and no Degraded condition.
func TestReconcileNamespaceTerminatingIsQuiet(t *testing.T) {
	const ns, name = "pipeline-test-terminating", "fj"
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithStatusSubresource(f).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return forgejo.NewNamespaceTerminatingError("x", obj.GetName(), ns)
			},
		}).Build()
	rec := events.NewFakeRecorder(10)
	r := &ForgejoReconciler{Client: c, Scheme: s, Recorder: rec}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}

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
	forgejo.DeleteInstanceMetrics(ns, name)
}
