package autopilot

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// An object that Valkey, the HPA or a PDB left behind is deleted on what the cache showed, and
// the delete names that object's UID: one created again under the name meanwhile survives.
// The fake client does not enforce the precondition, the real API server does.
func TestDeleteOwnedNamesTheUID(t *testing.T) {
	f := testInstance()
	s := testScheme(t)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "fj-valkey", Namespace: f.Namespace, UID: "sts-uid"}}
	if err := controllerutil.SetControllerReference(f, sts, s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sts).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != obj.GetUID() {
				return errors.New("delete without the UID precondition of the object that was read")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	deleted, err := deleteOwned(context.Background(), rc, client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{})
	if err != nil || !deleted {
		t.Fatalf("deleted %t, %v", deleted, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the StatefulSet was not deleted: %v", err)
	}
}
