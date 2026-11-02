package forgejo

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// isInstanceRef reports whether r points to f: by UID, or to a Forgejo of the same name (an
// earlier instance, whose garbage collection would delete the object just as well).
func isInstanceRef(r metav1.OwnerReference, f *forgejov1.Forgejo) bool {
	if r.UID == f.UID {
		return true
	}
	gv, err := schema.ParseGroupVersion(r.APIVersion)
	return err == nil && gv.Group == forgejov1.GroupVersion.Group && r.Kind == "Forgejo" && r.Name == f.Name
}

// HasInstanceRef reports whether one of refs points to f (see isInstanceRef).
func HasInstanceRef(refs []metav1.OwnerReference, f *forgejov1.Forgejo) bool {
	for _, r := range refs {
		if isInstanceRef(r, f) {
			return true
		}
	}
	return false
}

// WithoutInstanceRefs returns refs without the ones that point to f (see isInstanceRef).
func WithoutInstanceRefs(refs []metav1.OwnerReference, f *forgejov1.Forgejo) []metav1.OwnerReference {
	var out []metav1.OwnerReference
	for _, r := range refs {
		if !isInstanceRef(r, f) {
			out = append(out, r)
		}
	}
	return out
}
