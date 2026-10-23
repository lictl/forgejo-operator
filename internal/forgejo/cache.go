package forgejo

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// CacheScope is the set of labels an object of a high-volume kind must carry to enter the
// manager's informer cache. Without it the operator would list, watch and keep in memory
// every Secret, ConfigMap, Pod, Job, Deployment and Service of the cluster (Helm release
// Secrets alone can take hundreds of MB).
type CacheScope struct {
	Object client.Object
	Labels labels.Set
}

// CacheScopes are the label-restricted kinds. Everything the operator creates of these
// kinds carries Labels(), which every feature package applies after user labels.
// Secrets and ConfigMaps are only ever read by name, so the narrowest selector fits them.
// Pods, Jobs, Deployments, StatefulSets and Services are also listed by SelectorLabels (and
// other sets that include LabelName), which must stay answerable from the cache.
func CacheScopes() []CacheScope {
	managed := labels.Set{LabelManagedBy: ManagedBy}
	app := labels.Set{LabelName: AppName}
	return []CacheScope{
		{Object: &corev1.Secret{}, Labels: managed},
		{Object: &corev1.ConfigMap{}, Labels: managed},
		{Object: &corev1.Pod{}, Labels: app},
		{Object: &batchv1.Job{}, Labels: app},
		{Object: &appsv1.Deployment{}, Labels: app},
		{Object: &appsv1.StatefulSet{}, Labels: app},
		{Object: &corev1.Service{}, Labels: app},
	}
}

// CacheByObject turns scopes into cache.Options.ByObject entries. Namespaces are left to
// cache.Options.DefaultNamespaces.
func CacheByObject(scopes []CacheScope) map[client.Object]cache.ByObject {
	out := make(map[client.Object]cache.ByObject, len(scopes))
	for _, s := range scopes {
		out[s.Object] = cache.ByObject{Label: labels.SelectorFromSet(s.Labels)}
	}
	return out
}

// NewScopedReader wraps the manager's cache reader so that label-restricted kinds stay
// correct for every caller, including reads of objects the operator does not label (a
// configFrom or admin.existingSecret Secret, the CNPG "-app" Secret and pods, a PVC's
// pods):
//
//   - Get is served from the cache; a NotFound is confirmed by a live read, because the
//     object may exist without the labels the cache requires.
//   - List is served from the cache only when its label selector implies the cache's
//     selector, i.e. every object it can match is cached; otherwise it is a live list.
//
// Kinds without a scope are passed to the cache unchanged.
func NewScopedReader(cached, live client.Reader, scheme *runtime.Scheme, scopes []CacheScope) (client.Reader, error) {
	r := &scopedReader{cached: cached, live: live, scheme: scheme, scopes: map[schema.GroupVersionKind]labels.Set{}}
	for _, s := range scopes {
		gvk, err := apiutil.GVKForObject(s.Object, scheme)
		if err != nil {
			return nil, fmt.Errorf("cache scope %T: %w", s.Object, err)
		}
		r.scopes[gvk] = s.Labels
	}
	return r, nil
}

type scopedReader struct {
	cached, live client.Reader
	scheme       *runtime.Scheme
	scopes       map[schema.GroupVersionKind]labels.Set
}

func (r *scopedReader) scope(obj runtime.Object) (labels.Set, bool) {
	gvk, err := apiutil.GVKForObject(obj, r.scheme)
	if err != nil {
		return nil, false
	}
	gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	s, ok := r.scopes[gvk]
	return s, ok
}

func (r *scopedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.cached.Get(ctx, key, obj, opts...)
	if _, scoped := r.scope(obj); scoped && apierrors.IsNotFound(err) {
		return r.live.Get(ctx, key, obj, opts...)
	}
	return err
}

func (r *scopedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if required, scoped := r.scope(list); scoped {
		lo := &client.ListOptions{}
		lo.ApplyOptions(opts)
		if !SelectorImplies(lo.LabelSelector, required) {
			return r.live.List(ctx, list, opts...)
		}
	}
	return r.cached.List(ctx, list, opts...)
}

// SelectorImplies reports whether every object matched by sel carries all labels of
// required, judged from sel's equality requirements alone.
func SelectorImplies(sel labels.Selector, required labels.Set) bool {
	if len(required) == 0 {
		return true
	}
	if sel == nil {
		return false
	}
	reqs, selectable := sel.Requirements()
	if !selectable {
		// Matches nothing (labels.Nothing()); trivially implies anything.
		return true
	}
	for k, v := range required {
		found := false
		for _, req := range reqs {
			if req.Key() != k {
				continue
			}
			switch req.Operator() {
			case selection.Equals, selection.DoubleEquals, selection.In:
				if vals := req.Values(); vals.Len() == 1 && vals.Has(v) {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}
