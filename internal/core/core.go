// Package core reconciles the Level I/II building blocks of a Forgejo instance:
// generated secrets, the database (CloudNativePG or external), the data PVC, app.ini,
// the Deployment, Services/Ingress and the summarized status.
package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"math/big"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=core,resources=secrets;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// setOwnership makes the instance the controller owner of obj when owned is true and
// drops that reference otherwise (data-bearing children under retainPolicy Retain).
func setOwnership(rc *forgejo.ReconcileContext, obj client.Object, owned bool) error {
	if owned {
		return controllerutil.SetControllerReference(rc.Instance, obj, rc.Scheme)
	}
	refs := obj.GetOwnerReferences()
	kept := refs[:0]
	for _, r := range refs {
		if r.UID != rc.Instance.UID {
			kept = append(kept, r)
		}
	}
	obj.SetOwnerReferences(kept)
	return nil
}

// apply creates or updates obj and logs changes. Ownership is handled by the mutate
// function (see setOwnership) so that it can also be removed.
func apply(ctx context.Context, rc *forgejo.ReconcileContext, obj client.Object, owned bool, mutate func() error) error {
	op, err := rc.CreateOrUpdate(ctx, obj, false, func() error {
		if err := mutate(); err != nil {
			return err
		}
		return setOwnership(rc, obj, owned)
	})
	if err == nil && op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("reconciled child", "kind", kindOf(rc, obj), "name", obj.GetName(), "operation", op)
	}
	return err
}

func kindOf(rc *forgejo.ReconcileContext, obj client.Object) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	if gvks, _, err := rc.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		return gvks[0].Kind
	}
	return "?"
}

// mergeMaps returns a new map with the entries of all maps, later maps winning.
func mergeMaps(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// randomBase64 returns n random bytes in unpadded base64url, the format of
// "forgejo generate secret SECRET_KEY|JWT_SECRET|LFS_JWT_SECRET".
func randomBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const alphanumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomString returns n random alphanumeric characters.
func randomString(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphanumeric)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphanumeric[idx.Int64()]
	}
	return string(out), nil
}
