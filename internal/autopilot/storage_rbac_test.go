package autopilot

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// Without ClusterRole volume-usage bound (pods/exec), nothing is measured or expanded:
// StorageHealthy is Unknown/MeasurementUnavailable and names the ClusterRole, and du is not
// tried after a refused df.
func TestStorageWithoutPodsExec(t *testing.T) {
	f := storageInstance()
	pvc := dataPVC(f, "1Gi", "1Gi")
	env := newTestEnv(t, f, pvc, mountingPod(f))
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, "pod",
		errors.New(`User "system:serviceaccount:forgejo-operator-system:forgejo-operator-controller-manager" cannot create resource "pods/exec"`))
	fx := &fakeExec{
		df: func() (string, error) { return "", forbidden },
		du: func() (string, error) { return "", forbidden },
	}
	fx.install(t)
	if err := reconcileStorage(context.Background(), env.rc, stateFor(f), nil); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "MeasurementUnavailable" ||
		!strings.Contains(c.Message, "forgejo-operator-volume-usage") {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	if n := fx.count("df"); n != 1 {
		t.Fatalf("df ran %d times", n)
	}
	if n := fx.count("sh"); n != 0 {
		t.Fatalf("du ran %d times after a refused df", n)
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Resources.Requests.Storage().Equal(resource.MustParse("1Gi")) {
		t.Fatal("PVC changed without a measurement")
	}
}
