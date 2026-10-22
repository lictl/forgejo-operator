package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// staleCache serves the Forgejo from a snapshot while one is set, as an informer cache that
// has not yet seen the latest write; everything else, and all writes, go to the live store.
type staleCache struct {
	client.WithWatch
	snapshot *forgejov1.Forgejo
}

func newStaleCache(live client.WithWatch) *staleCache {
	sc := &staleCache{}
	sc.WithWatch = interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if f, ok := obj.(*forgejov1.Forgejo); ok && sc.snapshot != nil {
				sc.snapshot.DeepCopyInto(f)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	return sc
}

func eventsWithReason(rec *events.FakeRecorder, reason string) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, " "+reason+" ") {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// The pass that the previous pass's child changes queue starts before the cache has seen
// that pass's status write. It must not emit the previous pass's transition events again
// (e2e V5.3: UnsupportedVersion went out twice, with series.count 2).
func TestReconcileAfterStatusWriteIgnoresStaleCache(t *testing.T) {
	const ns, name = "pipeline-test-stale-cache", "fj"
	f := &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       forgejov1.ForgejoSpec{Image: "codeberg.org/forgejo/forgejo:16"},
	}
	s := testScheme(t)
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(f).WithStatusSubresource(f).Build()
	sc := newStaleCache(live)
	rec := events.NewFakeRecorder(100)
	r := &ForgejoReconciler{Client: sc, APIReader: live, Scheme: s, Recorder: rec}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
	defer forgetInstance(ns, name)

	before := &forgejov1.Forgejo{}
	if err := live.Get(context.Background(), req.NamespacedName, before); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Reconcile(context.Background(), req)
	if got := eventsWithReason(rec, "UnsupportedVersion"); len(got) != 1 {
		t.Fatalf("first pass: %d UnsupportedVersion events, want 1: %q", len(got), got)
	}

	// The cache still holds the object as it was before the first pass's status write.
	sc.snapshot = before
	_, _ = r.Reconcile(context.Background(), req)
	if got := eventsWithReason(rec, "UnsupportedVersion"); len(got) != 0 {
		t.Errorf("pass on a stale cache emitted %q again", got)
	}

	// Once the cache has caught up, it is used again without a live read.
	sc.snapshot = nil
	if _, err := r.getInstance(context.Background(), req.NamespacedName); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.statusWrites.Load(req.NamespacedName); ok {
		t.Error("last status write still tracked after the cache caught up")
	}
}

// getInstance uses the cache when it holds this controller's last status write or is as
// recent as the live object, and the live object otherwise.
func TestGetInstanceAfterStatusWrite(t *testing.T) {
	const ns, name = "ns", "fj"
	key := types.NamespacedName{Namespace: ns, Name: name}
	s := testScheme(t)
	live := fake.NewClientBuilder().WithScheme(s).
		WithObjects(&forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}).Build()
	cur := &forgejov1.Forgejo{}
	if err := live.Get(context.Background(), key, cur); err != nil {
		t.Fatal(err)
	}
	older := cur.DeepCopy()
	older.ResourceVersion = "1"

	for _, tc := range []struct {
		name      string
		written   string // "" = nothing tracked
		cached    *forgejov1.Forgejo
		wantRV    string
		wantTrack bool
	}{
		{"nothing written", "", older, "1", false},
		{"cache holds the write", "1", older, "1", false},
		{"cache behind the write", cur.ResourceVersion, older, cur.ResourceVersion, true},
		{"written by someone else since, cache current", "1", cur, cur.ResourceVersion, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := newStaleCache(live)
			sc.snapshot = tc.cached
			r := &ForgejoReconciler{Client: sc, APIReader: live, Scheme: s}
			if tc.written != "" {
				r.statusWrites.Store(key, tc.written)
			}
			got, err := r.getInstance(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			if got.ResourceVersion != tc.wantRV {
				t.Errorf("resourceVersion = %s, want %s", got.ResourceVersion, tc.wantRV)
			}
			if _, ok := r.statusWrites.Load(key); ok != tc.wantTrack {
				t.Errorf("still tracked = %v, want %v", ok, tc.wantTrack)
			}
		})
	}
}
