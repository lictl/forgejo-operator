package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Retention decides on the informer's copy of a scheduled backup, and deleting a ForgejoBackup
// removes its files. A backup that was deleted and created again under the same name since
// must survive: the delete names the UID it decided on.
func TestPruneDoesNotDeleteABackupThatWasReplaced(t *testing.T) {
	s := FakeScheme(t)
	ctx := context.Background()
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"}}
	f.Spec.Backup.Retention = 1
	now := time.Now()
	completed := func(name string, uid types.UID, age time.Duration) *forgejov1.ForgejoBackup {
		b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid,
			Labels: map[string]string{forgejo.LabelBackupReason: string(forgejov1.BackupReasonScheduled)}}}
		b.Status.Phase = forgejov1.BackupPhaseCompleted
		b.Status.CompletionTime = &metav1.Time{Time: now.Add(-age)}
		return b
	}
	cached := []*forgejov1.ForgejoBackup{completed("kept", "kept-uid", time.Hour), completed("expired", "stale-uid", 2*time.Hour)}
	replacement := completed("expired", "replacement-uid", time.Minute)
	// The fake client does not enforce a UID precondition, the API server does.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(replacement).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			live := &forgejov1.ForgejoBackup{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
				return err
			}
			if o.Preconditions != nil && o.Preconditions.UID != nil && *o.Preconditions.UID != live.UID {
				return apierrors.NewConflict(forgejov1.GroupVersion.WithResource("forgejobackups").GroupResource(), obj.GetName(),
					errors.New("Precondition failed: UID in precondition does not match UID in record"))
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}

	if _, err := prune(ctx, rc, cached, nil, now); err == nil {
		t.Fatal("the prune of a replaced backup did not fail")
	}
	got := &forgejov1.ForgejoBackup{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "expired"}, got); err != nil {
		t.Fatalf("the replacement backup was deleted: %v", err)
	}

	// The same backup, unchanged, is pruned.
	cached[1].UID = "replacement-uid"
	if _, err := prune(ctx, rc, cached, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "expired"}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("the backup beyond retention was not pruned: %v", err)
	}
}
