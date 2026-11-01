package forgejo

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// The stored maxUnavailable is kept only when it is the API server's default (Kubernetes 1.37);
// a missing one stays missing (older servers), and any other value is cleared.
func TestStatefulSetRollingUpdate(t *testing.T) {
	withMax := func(v intstr.IntOrString) appsv1.StatefulSetUpdateStrategy {
		return appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2), MaxUnavailable: &v}}
	}
	for name, tc := range map[string]struct {
		current appsv1.StatefulSetUpdateStrategy
		want    *intstr.IntOrString
	}{
		"new object":         {},
		"OnDelete":           {current: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}},
		"dropped (1.36)":     {current: appsv1.StatefulSetUpdateStrategy{RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{}}},
		"defaulted (1.37)":   {current: withMax(intstr.FromInt32(1)), want: ptr.To(intstr.FromInt32(1))},
		"changed to 2":       {current: withMax(intstr.FromInt32(2))},
		"string \"1\"":       {current: withMax(intstr.FromString("1"))}, // refused by the API server; == is strict
		"changed to \"50%\"": {current: withMax(intstr.FromString("50%"))},
	} {
		got := StatefulSetRollingUpdate(tc.current)
		if got.Type != appsv1.RollingUpdateStatefulSetStrategyType || got.RollingUpdate == nil ||
			ptr.Deref(got.RollingUpdate.Partition, -1) != 0 {
			t.Errorf("%s: strategy %+v", name, got)
			continue
		}
		if m := got.RollingUpdate.MaxUnavailable; (m == nil) != (tc.want == nil) || (m != nil && *m != *tc.want) {
			t.Errorf("%s: maxUnavailable %v, want %v", name, m, tc.want)
		}
	}
}
