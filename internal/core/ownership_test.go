package core

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

func instanceRef(f *forgejov1.Forgejo) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: forgejov1.GroupVersion.String(), Kind: f.Kind, Name: f.Name, UID: f.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
}

// ownedDataObjects are the data-bearing objects of f, each owned by it as under retainPolicy
// Delete, plus one reference that is not the instance's and must survive.
func ownedDataObjects(f *forgejov1.Forgejo) []client.Object {
	foreign := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "unrelated", UID: "unrelated-uid"}
	// A Forgejo of the same name that has been replaced since: garbage collection would delete
	// the object with it just the same.
	earlier := metav1.OwnerReference{APIVersion: forgejov1.GroupVersion.String(), Kind: f.Kind, Name: f.Name, UID: "earlier-uid"}
	meta1 := func(name string, extra ...metav1.OwnerReference) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: f.Namespace, OwnerReferences: append([]metav1.OwnerReference{instanceRef(f)}, extra...)}
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	cluster.SetName(forgejo.DatabaseClusterName(f))
	cluster.SetNamespace(f.Namespace)
	cluster.SetOwnerReferences([]metav1.OwnerReference{instanceRef(f)})
	return []client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: meta1(forgejo.DataPVCName(f))},
		cluster,
		&corev1.Secret{ObjectMeta: meta1(forgejo.SecretsName(f), foreign)},
		&corev1.Secret{ObjectMeta: meta1(forgejo.AdminSecretName(f), earlier)},
		&corev1.Secret{ObjectMeta: meta1(forgejo.SSHHostKeysSecretName(f))},
	}
}

func refsOf(t *testing.T, rc *forgejo.ReconcileContext, obj client.Object) []metav1.OwnerReference {
	t.Helper()
	if err := rc.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return obj.GetOwnerReferences()
}

// retainPolicy Retain: the data volume, the CNPG cluster and the identity Secrets carry no
// reference to the instance, whatever the rest of the pipeline did or did not do. References
// of others stay.
func TestOwnershipStepDropsInstanceReferencesUnderRetain(t *testing.T) {
	f := testInstance()
	objs := ownedDataObjects(f)
	rc := cnpgContext(t, f, objs...)
	if _, err := ReconcileOwnership(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		refs := refsOf(t, rc, o)
		if forgejo.HasInstanceRef(refs, f) {
			t.Errorf("%T %s still references the instance: %+v", o, o.GetName(), refs)
		}
	}
	if refs := refsOf(t, rc, objs[2]); len(refs) != 1 || refs[0].Kind != "ConfigMap" {
		t.Errorf("a reference of another owner was dropped: %+v", refs)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v, want True", c)
	}

	// Nothing left to do: no further write.
	before := objs[0].GetResourceVersion()
	if _, err := ReconcileOwnership(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if rv := refsOf(t, rc, objs[0]); len(rv) != 0 || objs[0].GetResourceVersion() != before {
		t.Errorf("second pass changed the claim: %+v rv %s -> %s", rv, before, objs[0].GetResourceVersion())
	}
}

// Under Delete, the owning steps put the references; this step neither adds nor drops one, and
// the condition about Retain is not reported.
func TestOwnershipStepLeavesReferencesUnderDelete(t *testing.T) {
	f := testInstance()
	f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionRetainPolicyApplied,
		Status: metav1.ConditionTrue, Reason: "Applied"})
	objs := ownedDataObjects(f)
	rc := cnpgContext(t, f, objs...)
	if _, err := ReconcileOwnership(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !forgejo.HasInstanceRef(refsOf(t, rc, o), f) {
			t.Errorf("%T %s lost its reference under Delete", o, o.GetName())
		}
	}
	if meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied) != nil {
		t.Error("RetainPolicyApplied is still reported under Delete")
	}
}

// A write that loses a race is tried again soon, and the condition says it is not applied yet:
// that is the time not to delete the Forgejo.
func TestOwnershipStepReportsPendingOnConflict(t *testing.T) {
	f := testInstance()
	objs := ownedDataObjects(f)
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == forgejo.DataPVCName(f) {
				return apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), obj.GetName(), errors.New("changed"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, RESTMapper: mapper, Instance: f}
	if _, err := ReconcileOwnership(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Pending" || rc.RequeueAfter == 0 {
		t.Fatalf("condition %+v, requeue %v; want False/Pending and a retry", cond, rc.RequeueAfter)
	}
	if !forgejo.HasInstanceRef(refsOf(t, rc, objs[0]), f) {
		t.Error("the claim was changed although its write conflicted")
	}
	if forgejo.HasInstanceRef(refsOf(t, rc, objs[2]), f) {
		t.Error("one conflict stopped the other objects from being handled")
	}
}

// An object of the instance's name that the instance does not control is never written, and an
// error names it; one it controls, or an orphan with its labels, is. The objects a person puts
// in place to recover an instance are adopted as they are.
func TestApplyDoesNotTakeOverForeignObjects(t *testing.T) {
	const name = "fj-config"
	f := testInstance()
	secret := func(mutate func(*corev1.Secret)) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace},
			Data: map[string][]byte{"smtp-password": []byte("keep")}}
		if mutate != nil {
			mutate(s)
		}
		return s
	}
	write := func(rc *forgejo.ReconcileContext, adopt bool) (*corev1.Secret, error) {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace}}
		mutate := func() error { sec.Data = map[string][]byte{"app.ini": []byte("generated")}; return nil }
		var err error
		if adopt {
			err = applyAdopting(context.Background(), rc, sec, true, mutate)
		} else {
			err = apply(context.Background(), rc, sec, mutate)
		}
		return sec, err
	}
	stored := func(rc *forgejo.ReconcileContext) *corev1.Secret {
		got := &corev1.Secret{}
		if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: f.Namespace, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	labelled := func(s *corev1.Secret) { s.Labels = forgejo.Labels(f, forgejo.ComponentServer) }
	otherController := func(s *corev1.Secret) {
		s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "x", UID: "x-uid", Controller: ptr.To(true)}}
	}

	for _, tc := range []struct {
		name     string
		existing *corev1.Secret
		adopt    bool
		written  bool
	}{
		{"a user's Secret without labels", secret(nil), false, false},
		{"a Secret with another controller", secret(func(s *corev1.Secret) { labelled(s); otherController(s) }), false, false},
		{"an orphan with the instance's labels", secret(labelled), false, true},
		{"nothing exists yet", nil, false, true},
		{"a recovery Secret put in place by a person", secret(nil), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.existing != nil {
				objs = append(objs, tc.existing)
			}
			rc := testContext(t, f, objs...)
			_, err := write(rc, tc.adopt)
			var conflict *forgejo.NameConflictError
			switch {
			case tc.written && err != nil:
				t.Fatalf("write refused: %v", err)
			case !tc.written && !errors.As(err, &conflict):
				t.Fatalf("error %v, want a *NameConflictError", err)
			}
			got := stored(rc)
			if tc.written {
				if string(got.Data["app.ini"]) != "generated" || !metav1.IsControlledBy(got, f) {
					t.Errorf("not written or not owned: %+v", got)
				}
			} else if string(got.Data["smtp-password"]) != "keep" || got.Data["app.ini"] != nil || metav1.IsControlledBy(got, f) {
				t.Errorf("a foreign Secret was changed: %+v", got)
			}
		})
	}
}

// An object that cannot be changed (a webhook, RBAC) must not keep the others from being
// handled, and the condition says the policy is not applied.
func TestOwnershipStepHandlesTheRestWhenOneObjectFails(t *testing.T) {
	f := testInstance()
	objs := ownedDataObjects(f)
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	denied := apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), forgejo.DataPVCName(f), errors.New("webhook denied"))
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == forgejo.DataPVCName(f) {
				return denied
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, RESTMapper: mapper, Instance: f}
	_, err := ReconcileOwnership(context.Background(), rc)
	if !errors.Is(err, denied) {
		t.Fatalf("error %v, want the denial", err)
	}
	for _, o := range objs[1:] {
		if forgejo.HasInstanceRef(refsOf(t, rc, o), f) {
			t.Errorf("%T %s keeps its reference because the volume failed", o, o.GetName())
		}
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("condition %+v, want False", cond)
	}
}

// failingDiscovery answers every RESTMapping with an error that is not "kind not served".
type failingDiscovery struct{ meta.RESTMapper }

func (failingDiscovery) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("discovery failed: the server is not reachable")
}

// "Applied" is a promise that deleting the Forgejo keeps its database. It must not be made while
// the CNPG API could not be asked whether the cluster exists.
func TestOwnershipStepIsNotAppliedWhenTheCNPGAPICannotBeAsked(t *testing.T) {
	f := testInstance()
	objs := ownedDataObjects(f)
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, RESTMapper: failingDiscovery{}, Instance: f}
	if _, err := ReconcileOwnership(context.Background(), rc); err == nil {
		t.Fatal("a failed discovery was not reported")
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("condition %+v, want False", cond)
	}

	// With an External database there is no cluster to look for, so nothing depends on the API.
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	if _, err := ReconcileOwnership(context.Background(), rc); err != nil {
		t.Fatalf("external database: %v", err)
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v, want True", cond)
	}
}
