package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1alpha1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/runner"
)

// A StatefulSet the operator renders compares equal to what the API server stored, defaults
// included, so the next pass sends no update. Kubernetes 1.37 defaults
// spec.updateStrategy.rollingUpdate.maxUnavailable to 1 (MaxUnavailableStatefulSet), older
// servers leave it unset (forgejo.StatefulSetRollingUpdate).
var _ = Describe("StatefulSet defaults", func() {
	It("leaves a rendered runner StatefulSet unchanged on the next pass", func() {
		fr := &forgejov1alpha1.ForgejoRunner{
			ObjectMeta: metav1.ObjectMeta{Name: "statefulset-defaults", Namespace: "default"},
			Spec: forgejov1alpha1.ForgejoRunnerSpec{
				ForgejoRef: forgejov1alpha1.ForgejoRef{Name: "fj"},
				Executor:   forgejov1alpha1.RunnerExecutorHost,
			},
		}
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: runner.StatefulSetName(fr), Namespace: fr.Namespace}}
		mutate := func() error {
			runner.MutateStatefulSet(sts, runner.WorkloadInput{Runner: fr, Replicas: 1, ConfigHash: "config"})
			return nil
		}
		op, err := controllerutil.CreateOrUpdate(ctx, k8sClient, sts, mutate)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, sts)).To(Succeed()) })
		Expect(op).To(Equal(controllerutil.OperationResultCreated))

		op, err = controllerutil.CreateOrUpdate(ctx, k8sClient, sts, mutate)
		Expect(err).NotTo(HaveOccurred())
		Expect(op).To(Equal(controllerutil.OperationResultNone))
	})
})
