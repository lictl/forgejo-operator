package forgejo

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// defaultStatefulSetMaxUnavailable is what the API server sets
// spec.updateStrategy.rollingUpdate.maxUnavailable of a StatefulSet to from Kubernetes 1.37
// (feature gate MaxUnavailableStatefulSet, on by default); servers without the gate drop the
// field unless the stored object already has it.
var defaultStatefulSetMaxUnavailable = intstr.FromInt32(1)

// StatefulSetRollingUpdate is the update strategy of the operator's StatefulSets: a rolling
// update of every pod (partition 0) with the API server's maxUnavailable. It keeps the stored
// field when that holds the default, so that an unchanged StatefulSet compares equal on servers
// that default it and on those that drop it; any other value is cleared for the server to
// default again (or, without the gate, to leave unset). current is the stored strategy (empty
// for a new object).
func StatefulSetRollingUpdate(current appsv1.StatefulSetUpdateStrategy) appsv1.StatefulSetUpdateStrategy {
	var maxUnavailable *intstr.IntOrString
	if ru := current.RollingUpdate; ru != nil && ru.MaxUnavailable != nil && *ru.MaxUnavailable == defaultStatefulSetMaxUnavailable {
		maxUnavailable = ptr.To(defaultStatefulSetMaxUnavailable)
	}
	return appsv1.StatefulSetUpdateStrategy{
		Type:          appsv1.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0), MaxUnavailable: maxUnavailable},
	}
}
