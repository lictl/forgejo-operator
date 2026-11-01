package controller

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgejov1alpha1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/runner"
)

// The fake client of the other tests applies no CRD defaults and validates neither the CRD
// schemas nor finalizers nor the built-in types, and serves every kind it has a scheme for.
// This spec runs the reconcilers against an API server: the children, status (through the
// status subresource) and finalizers a pass writes must be accepted, a deleted object is
// released, and an optional API that is not installed is reported, not an error.
var _ = Describe("Reconcilers against an API server", func() {
	It("write objects and status that the API server accepts", func() {
		const ns, instance = "api-server", "git"
		key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
		reconcileOK := func(r reconcile.Reconciler, name string) {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
			Expect(err).NotTo(HaveOccurred())
		}
		get := func(name string, obj client.Object) {
			GinkgoHelper()
			Expect(k8sClient.Get(ctx, key(name), obj)).To(Succeed())
		}
		gone := func(name string, obj client.Object) {
			GinkgoHelper()
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), obj))).To(BeTrue(), "%s still exists", name)
		}
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		forgejos := &ForgejoReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		By("reporting the bundled database while CloudNativePG is not installed")
		bundled := &forgejov1alpha1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "bundled", Namespace: ns}}
		Expect(k8sClient.Create(ctx, bundled)).To(Succeed())
		DeferCleanup(forgetInstance, ns, bundled.Name)
		reconcileOK(forgejos, bundled.Name)
		get(bundled.Name, bundled)
		dbReady := meta.FindStatusCondition(bundled.Status.Conditions, forgejov1alpha1.ConditionDatabaseReady)
		Expect(dbReady).NotTo(BeNil())
		Expect(dbReady.Status).To(Equal(metav1.ConditionFalse))
		Expect(dbReady.Reason).To(Equal("CNPGNotInstalled"))

		By("running a Forgejo pass up to the workload")
		// An external database the operator can reach: the pass does not wait for CloudNativePG.
		db, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns},
			StringData: map[string]string{"username": "forgejo", "password": "secret"},
		})).To(Succeed())
		f := &forgejov1alpha1.Forgejo{
			ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: ns},
			Spec: forgejov1alpha1.ForgejoSpec{Database: forgejov1alpha1.DatabaseSpec{
				Type: forgejov1alpha1.DatabaseTypeExternal,
				External: &forgejov1alpha1.ExternalDatabaseSpec{
					Host: "127.0.0.1", Port: int32(db.Addr().(*net.TCPAddr).Port),
					CredentialsSecret: forgejov1alpha1.SecretCredentialsRef{Name: "db"},
				},
			}},
		}
		Expect(k8sClient.Create(ctx, f)).To(Succeed())
		DeferCleanup(forgetInstance, ns, f.Name)
		reconcileOK(forgejos, f.Name)
		get(forgejo.DeploymentName(f), &appsv1.Deployment{})
		get(f.Name, f)
		Expect(f.Status.ObservedGeneration).To(Equal(f.Generation))
		Expect(meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1alpha1.ConditionDatabaseReady)).To(BeTrue())

		By("starting a ForgejoBackup")
		b := &forgejov1alpha1.ForgejoBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: ns},
			Spec:       forgejov1alpha1.ForgejoBackupSpec{ForgejoRef: forgejov1alpha1.ForgejoRef{Name: f.Name}},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		backups := &ForgejoBackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileOK(backups, b.Name) // finalizer and labels
		reconcileOK(backups, b.Name) // backup volume and Job
		get(b.Name, b)
		Expect(b.Finalizers).To(ContainElement(forgejo.FinalizerName))
		Expect(b.Status.Phase).To(Equal(forgejov1alpha1.BackupPhaseRunning))
		get(b.Status.JobName, &batchv1.Job{})

		By("failing a ForgejoRestore of the unfinished backup, then deleting it")
		rs := &forgejov1alpha1.ForgejoRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: ns},
			Spec:       forgejov1alpha1.ForgejoRestoreSpec{ForgejoRef: forgejov1alpha1.ForgejoRef{Name: f.Name}, BackupName: b.Name},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		restores := &ForgejoRestoreReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileOK(restores, rs.Name) // finalizer and label
		reconcileOK(restores, rs.Name) // BackupNotCompleted
		get(rs.Name, rs)
		Expect(rs.Finalizers).To(ContainElement(forgejo.FinalizerName))
		Expect(rs.Status.Phase).To(Equal(forgejov1alpha1.RestorePhaseFailed))
		failed := meta.FindStatusCondition(rs.Status.Conditions, conditionReady)
		Expect(failed).NotTo(BeNil())
		Expect(failed.Reason).To(Equal("BackupNotCompleted"))
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
		reconcileOK(restores, rs.Name)
		gone(rs.Name, rs)

		By("registering a ForgejoRunner with the instance once it is Ready, then deleting it")
		get(f.Name, f)
		f.Status.Phase = forgejov1alpha1.PhaseRunning
		f.Status.Image = forgejo.Image(f)
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
			Type: forgejov1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Available",
		})
		Expect(k8sClient.Status().Update(ctx, f)).To(Succeed())
		fr := &forgejov1alpha1.ForgejoRunner{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: ns},
			Spec: forgejov1alpha1.ForgejoRunnerSpec{
				ForgejoRef: forgejov1alpha1.ForgejoRef{Name: f.Name}, Executor: forgejov1alpha1.RunnerExecutorDocker,
			},
		}
		Expect(k8sClient.Create(ctx, fr)).To(Succeed())
		api := newFakeRunnerAPI()
		runners := &ForgejoRunnerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewAPI: func(string, string, string) runner.API { return api }}
		reconcileOK(runners, fr.Name)
		get(fr.Name, fr)
		Expect(fr.Finalizers).To(ContainElement(forgejo.FinalizerName))
		registered := meta.FindStatusCondition(fr.Status.Conditions, forgejov1alpha1.ConditionRunnerRegistered)
		Expect(registered).NotTo(BeNil())
		Expect(registered.Reason).To(Equal(reasonRegistering))
		get(runner.RegisterJobName(fr), &batchv1.Job{})
		get(runner.StatefulSetName(fr), &appsv1.StatefulSet{})
		Expect(k8sClient.Delete(ctx, fr)).To(Succeed())
		reconcileOK(runners, fr.Name)
		gone(fr.Name, fr)
	})
})
