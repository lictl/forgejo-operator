package backup_test

import (
	"context"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
)

func TestOrphanCleanupKeepsRecordedRegistryReferences(t *testing.T) {
	const (
		account    = "registry-account"
		pullSecret = "registry-key"
	)
	for _, tc := range []struct {
		name, serviceAccount string
		pullSecrets          []corev1.LocalObjectReference
		replacement          bool
	}{
		{name: "default"},
		{name: "service-account", serviceAccount: account},
		{name: "explicit-pull-secret", pullSecrets: []corev1.LocalObjectReference{{Name: pullSecret}}},
		{name: "both", serviceAccount: account, pullSecrets: []corev1.LocalObjectReference{{Name: pullSecret}, {Name: "mirror-key"}}},
		{name: "replacement-instance", serviceAccount: account, pullSecrets: []corev1.LocalObjectReference{{Name: pullSecret}}, replacement: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := instance("")
			f.Spec.Image = "registry.example.test/forgejo:16"
			f.Spec.PodTemplate.ServiceAccountName = tc.serviceAccount
			f.Spec.ImagePullSecrets = tc.pullSecrets
			b := queuedBackup("orphan", forgejov1.BackupReasonManual)
			live := newFakeClient(backup.FakeScheme(t), f, b, dbAppSecret("pw"), fbSecrets())
			c := interceptor.NewClient(live, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if job, ok := obj.(*batchv1.Job); ok && job.Name == backup.BackupJobName(b) {
						got := getBackup(t, live, b.Name)
						if got.Status.ServiceAccountName != tc.serviceAccount || !slices.Equal(got.Status.ImagePullSecrets, tc.pullSecrets) {
							t.Fatalf("registry references were not journaled before dump creation: %+v", got.Status)
						}
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			reconcileBackup(t, c, b.Name)
			if err := live.Delete(ctx, f); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{}
			if err := live.Get(ctx, client.ObjectKey{Namespace: ns, Name: backup.BackupJobName(b)}, job); err != nil {
				t.Fatal(err)
			}
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := live.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, c, b.Name)
			got := getBackup(t, live, b.Name)
			if got.Status.Phase != forgejov1.BackupPhaseCompleted {
				t.Fatalf("backup failed after source removal: %+v", got.Status)
			}
			if tc.replacement {
				other := instance("")
				other.UID = "replacement-instance"
				other.Spec.PodTemplate.ServiceAccountName = "other-account"
				other.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "other-key"}}
				if err := live.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			if err := live.Delete(ctx, got); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, c, b.Name)
			cleanup := &batchv1.Job{}
			if err := live.Get(ctx, client.ObjectKey{Namespace: ns, Name: backup.CleanupJobName(b)}, cleanup); err != nil {
				t.Fatal(err)
			}
			spec := cleanup.Spec.Template.Spec
			if spec.ServiceAccountName != tc.serviceAccount || !slices.Equal(spec.ImagePullSecrets, tc.pullSecrets) {
				t.Fatalf("orphan cleanup lost original pull references: account=%q, secrets=%v", spec.ServiceAccountName, spec.ImagePullSecrets)
			}
			if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
				t.Fatal("orphan cleanup enabled ServiceAccount token mounting")
			}
			if spec.Containers[0].Image != f.Spec.Image {
				t.Fatalf("orphan cleanup changed recorded image: %s", spec.Containers[0].Image)
			}
		})
	}
}
