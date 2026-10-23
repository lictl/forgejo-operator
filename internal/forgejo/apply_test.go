package forgejo

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// mapperErr answers every lookup with err, or with a mapping for served kinds.
type mapperErr struct {
	meta.RESTMapper
	served map[schema.GroupVersionKind]bool
	err    error
}

func (m *mapperErr) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.served[gk.WithVersion(versions[0])] {
		return &meta.RESTMapping{}, nil
	}
	return nil, m.err
}

// Only a NoMatch answer means "not served"; a failed discovery is an error to retry on.
func TestAPIServed(t *testing.T) {
	gv := schema.GroupVersion{Group: GatewayAPIGroup, Version: "v1"}
	other := schema.GroupVersion{Group: "cert-manager.io", Version: "v1"}
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "")
	timeout := errors.New("the server was unable to return a response in the time allotted")
	for _, tc := range []struct {
		name   string
		err    error
		served bool
		fails  bool
	}{
		{"no match", &meta.NoKindMatchError{GroupKind: HTTPRouteGVK.GroupKind()}, false, false},
		{"wrapped no match", fmt.Errorf("x: %w", &meta.NoResourceMatchError{}), false, false},
		{"discovery: group not found", &apiutil.ErrResourceDiscoveryFailed{gv: notFound}, false, false},
		{"discovery: all not found", &apiutil.ErrResourceDiscoveryFailed{gv: notFound, other: &meta.NoResourceMatchError{}}, false, false},
		// errors.Is matches when any entry is NotFound, but the other one failed.
		{"discovery: partly failed", &apiutil.ErrResourceDiscoveryFailed{gv: notFound, other: timeout}, false, true},
		{"timeout", timeout, false, true},
	} {
		rc := &ReconcileContext{RESTMapper: &mapperErr{err: tc.err}}
		served, err := rc.APIServed(HTTPRouteGVK)
		if served != tc.served || (err != nil) != tc.fails {
			t.Errorf("%s: served %v, err %v", tc.name, served, err)
		}
	}

	// FirstServedAPI tries the versions in order and stops at a failed discovery.
	rc := &ReconcileContext{RESTMapper: &mapperErr{served: map[schema.GroupVersionKind]bool{TCPRouteGVKs[1]: true},
		err: &meta.NoKindMatchError{}}}
	if gvk, ok, err := rc.FirstServedAPI(TCPRouteGVKs...); !ok || err != nil || gvk != TCPRouteGVKs[1] {
		t.Errorf("v1alpha2 only: %v %v %v", gvk, ok, err)
	}
	rc.RESTMapper = &mapperErr{served: map[schema.GroupVersionKind]bool{TCPRouteGVKs[1]: true}, err: timeout}
	if _, ok, err := rc.FirstServedAPI(TCPRouteGVKs...); ok || err == nil {
		t.Errorf("failed discovery of v1: ok %v, err %v", ok, err)
	}
	if served, err := (&ReconcileContext{}).APIServed(HTTPRouteGVK); served || err != nil {
		t.Errorf("no mapper: %v %v", served, err)
	}
}
