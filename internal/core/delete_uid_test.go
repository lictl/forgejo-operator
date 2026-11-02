package core

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// requireUID makes the fake API server refuse a delete that does not name the UID of the object
// the operator decided on: the fake client does not enforce that precondition, the real one does.
func requireUID() interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		o := &client.DeleteOptions{}
		o.ApplyOptions(opts)
		if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != obj.GetUID() {
			return errors.New("delete without the UID precondition of the object that was read")
		}
		return c.Delete(ctx, obj, opts...)
	}}
}

func contextWith(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) *forgejo.ReconcileContext {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(requireUID()).Build()
	return &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, Instance: f}
}

func ownedBy(t *testing.T, f *forgejov1.Forgejo, obj client.Object, uid string) client.Object {
	t.Helper()
	obj.SetUID(types.UID(uid))
	if err := controllerutil.SetControllerReference(f, obj, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return obj
}

// The objects of a feature that is switched off are deleted on what the cache showed: the
// delete names that object's UID, so one created again under the name meanwhile survives.
func TestDeletesOfOwnedChildrenNameTheirUID(t *testing.T) {
	f := testInstance()
	ctx := context.Background()

	t.Run("Service", func(t *testing.T) {
		svc := ownedBy(t, f, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: f.Namespace}}, "svc-uid")
		rc := contextWith(t, f, svc)
		if err := deleteOwned(ctx, rc, &corev1.Service{}, "gone"); err != nil {
			t.Fatal(err)
		}
		if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(svc), &corev1.Service{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the Service was not deleted: %v", err)
		}
	})

	t.Run("cron runner StatefulSet", func(t *testing.T) {
		sts := ownedBy(t, f, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace}}, "cron-uid")
		rc := contextWith(t, f, sts) // highAvailability is off: the cron runner goes
		if err := reconcileCronRunner(ctx, rc, 1, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the cron runner was not deleted: %v", err)
		}
	})
}
