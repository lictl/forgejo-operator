package observability

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// A ServiceMonitor or PrometheusRule of a monitoring that was switched off is deleted on what
// the cache showed, and the delete names that object's UID: one created again under the name
// meanwhile survives. The fake client does not enforce the precondition, the real API server does.
func TestDeleteIfOwnedNamesTheUID(t *testing.T) {
	f := testInstance("fj")
	rc, _ := monitoringRC(t, f, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != obj.GetUID() {
				return errors.New("delete without the UID precondition of the object that was read")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(forgejo.ServiceMonitorGVK)
	sm.SetName(forgejo.ServiceMonitorName(f))
	sm.SetNamespace(f.Namespace)
	sm.SetUID("sm-uid")
	if err := controllerutil.SetControllerReference(f, sm, rc.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := rc.Client.Create(context.Background(), sm.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(forgejo.ServiceMonitorGVK)
	target.SetName(sm.GetName())
	target.SetNamespace(sm.GetNamespace())
	if err := deleteIfOwned(context.Background(), rc, target); err != nil {
		t.Fatal(err)
	}
	if err := rc.Client.Get(context.Background(), client.ObjectKeyFromObject(sm), sm.DeepCopy()); !apierrors.IsNotFound(err) {
		t.Fatalf("the ServiceMonitor was not deleted: %v", err)
	}
}
