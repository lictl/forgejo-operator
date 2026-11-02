package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ownershipRequeue is how soon a patch that lost an optimistic-lock race is tried again.
const ownershipRequeue = 5 * time.Second

// ReconcileOwnership applies spec.storage.retainPolicy: Retain to the data-bearing objects that
// exist: the data volume, the CNPG cluster and the identity Secrets (<name>-secrets, -admin,
// -ssh-host-keys) carry no owner reference to the instance, so that deleting the Forgejo keeps
// them. The steps that create these objects drop the reference too, but they sit in the chain
// behind the database, Valkey and the identity check: with the instance Broken, or paused, or
// the database not ready, they do not run, and a Delete that was just changed to Retain would
// still be garbage-collected with the Forgejo. This step has no prerequisites, runs early so
// that a conflict in a later step does not cut the pass short before it, and runs while the
// instance is paused too (see the controller).
//
// Condition RetainPolicyApplied is True only when every object was looked at and none carries
// a reference to the instance. Only the Retain direction is applied here: adding a reference
// (under Delete) stays with the steps that own the objects, and a missed one only keeps data.
func ReconcileOwnership(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !forgejo.RetainData(f) {
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionRetainPolicyApplied)
		return forgejo.StepResult{}, nil
	}
	pending := func(msg string, err error) (forgejo.StepResult, error) {
		rc.SetCondition(forgejov1.ConditionRetainPolicyApplied, metav1.ConditionFalse, "Pending", msg)
		return forgejo.StepResult{}, err
	}

	// A cluster that cannot be looked for is not a cluster that does not exist: "applied" must
	// not be reported while the CNPG API could not be asked, or an owned cluster goes unseen.
	clusterWanted := forgejo.DatabaseType(f) != forgejov1.DatabaseTypeExternal
	clusterServed := false
	if clusterWanted {
		served, err := rc.APIServed(forgejo.CNPGClusterGVK)
		if err != nil {
			return pending("Cannot tell whether the CNPG cluster exists: "+err.Error(), fmt.Errorf("discovering the CNPG API: %w", err))
		}
		clusterServed = served
	}
	objs, data, err := dataBearingObjects(ctx, rc)
	if err != nil {
		return pending("Cannot read the data-bearing objects: "+err.Error(), fmt.Errorf("reading the data-bearing objects: %w", err))
	}
	if clusterServed && !data.ClusterChecked {
		rc.Requeue(ownershipRequeue)
		return pending("The CNPG API went away while the cluster was looked up", nil)
	}

	var conflicted []string
	var failed []error
	for _, o := range objs {
		refs := o.GetOwnerReferences()
		if !forgejo.HasInstanceRef(refs, f) {
			continue
		}
		orig, ok := o.DeepCopyObject().(client.Object)
		if !ok {
			continue
		}
		o.SetOwnerReferences(forgejo.WithoutInstanceRefs(refs, f))
		if err := rc.Client.Patch(ctx, o, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				conflicted = append(conflicted, o.GetName())
			} else {
				// One object that cannot be changed must not keep the others from being handled.
				failed = append(failed, fmt.Errorf("dropping the owner reference of %s %s: %w", kindOf(rc, o), o.GetName(), err))
			}
			continue
		}
		rc.Event(corev1.EventTypeNormal, "RetainPolicyApplied", "Ownership",
			"%s %s no longer belongs to the Forgejo: spec.storage.retainPolicy is Retain, so deleting the Forgejo keeps it",
			kindOf(rc, o), o.GetName())
	}
	if len(failed) > 0 {
		err := errors.Join(failed...)
		return pending("Could not drop an owner reference: "+err.Error(), err)
	}
	if len(conflicted) > 0 {
		rc.Requeue(ownershipRequeue)
		return pending("Waiting to drop the owner reference of "+strings.Join(conflicted, ", "), nil)
	}
	rc.SetCondition(forgejov1.ConditionRetainPolicyApplied, metav1.ConditionTrue, "Applied",
		"No data volume, database or identity Secret belongs to the Forgejo: deleting it keeps them (spec.storage.retainPolicy: Retain)")
	return forgejo.StepResult{}, nil
}

// dataBearingObjects are the existing objects of the instance whose loss is data or identity:
// the data volume and the CNPG cluster (confirmed with live reads, as the identity step does)
// and the identity Secrets. It returns what it read of the data objects as well.
func dataBearingObjects(ctx context.Context, rc *forgejo.ReconcileContext) ([]client.Object, *forgejo.DataObjects, error) {
	f := rc.Instance
	data, err := forgejo.ReadDataObjects(ctx, rc)
	if err != nil {
		return nil, nil, err
	}
	var objs []client.Object
	for _, o := range data.Present() {
		objs = append(objs, o.Object)
	}
	for _, name := range []string{forgejo.SecretsName(f), forgejo.AdminSecretName(f), forgejo.SSHHostKeysSecretName(f)} {
		sec := &corev1.Secret{}
		found, err := forgejo.GetConfirmed(ctx, rc, types.NamespacedName{Namespace: f.Namespace, Name: name}, sec)
		if err != nil {
			return nil, nil, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		if found {
			objs = append(objs, sec)
		}
	}
	return objs, data, nil
}
