package backup

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// VolumeConflictError reports that no node can attach both the backup volume and the data
// volume, so a Job that mounts both could never be scheduled.
type VolumeConflictError struct {
	BackupClaim string
	DataClaim   string
	Detail      string
}

func (e *VolumeConflictError) Error() string {
	return fmt.Sprintf("backup PVC %s is bound to a volume that cannot be attached where data PVC %s is (%s); "+
		"recreate the backup PVC or use a StorageClass that is reachable from both nodes", e.BackupClaim, e.DataClaim, e.Detail)
}

// Placement returns the affinity of a Job that mounts the backup claim backupClaim and, when
// mountsData is set, the instance's data volume. nil means no constraint.
//
// The data volume constrains the Job only when the Job mounts it, or while the backup claim is
// still unbound: then its first consumer decides where a node-local or zonal backup volume is
// provisioned, and it has to end up next to the data. A ReadWriteOnce data volume is attached
// to the node of the running Forgejo pods, so the Job joins them (pod affinity); without
// running pods, and for a ReadWriteMany/ReadOnlyMany volume that any node can attach, only
// the data PV's node affinity applies.
//
// A bound backup volume is honoured by the scheduler through its PV's node affinity. For a Job
// that mounts both volumes that affinity is combined with the data constraint; when the two
// cannot be met on any node, Placement returns a *VolumeConflictError instead of a Job that
// would stay Pending.
func Placement(ctx context.Context, c client.Client, f *forgejov1.Forgejo, mountsData bool, backupClaim string) (*corev1.Affinity, error) {
	backupPV, bound, err := claimVolume(ctx, c, f.Namespace, backupClaim)
	if err != nil {
		return nil, err
	}
	if bound && !mountsData {
		return nil, nil
	}
	data, err := dataPlacement(ctx, c, f)
	if err != nil {
		return nil, err
	}
	backupTerms := pvNodeSelector(backupPV)
	if !bound || backupTerms == nil {
		return data.affinity(nil), nil
	}

	conflict := func(detail string) error {
		return &VolumeConflictError{BackupClaim: backupClaim, DataClaim: DataClaimName(f), Detail: detail}
	}
	if data.nodeName != "" {
		node := &corev1.Node{}
		switch err := c.Get(ctx, types.NamespacedName{Name: data.nodeName}, node); {
		case err == nil:
			if !NodeSelectorMatches(backupTerms, node) {
				return nil, conflict(fmt.Sprintf("Forgejo runs on node %s, which PV %s does not allow", node.Name, backupPV.Name))
			}
		case !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err):
			return nil, err
		}
		return data.affinity(backupTerms), nil
	}

	combined := IntersectNodeSelectors(data.terms, backupTerms)
	if data.terms != nil {
		nodes := &corev1.NodeList{}
		switch err := c.List(ctx, nodes); {
		case err == nil:
			if len(nodes.Items) > 0 && !anyNodeMatches(combined, nodes.Items) {
				return nil, conflict(fmt.Sprintf("no node satisfies the node affinity of both PV %s and PV %s", backupPV.Name, data.pvName))
			}
		case !apierrors.IsForbidden(err):
			return nil, err
		}
	}
	return data.affinity(combined), nil
}

// dataConstraint is where a Job must run to mount the data volume.
type dataConstraint struct {
	selector map[string]string
	// nodeName is the node of the running Forgejo pods the Job joins ("" for none).
	nodeName string
	// terms is the data PV's required node affinity (nil for none).
	terms  *corev1.NodeSelector
	pvName string
}

func (d dataConstraint) affinity(nodeTerms *corev1.NodeSelector) *corev1.Affinity {
	var aff corev1.Affinity
	if d.nodeName != "" {
		aff.PodAffinity = &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: d.selector},
				TopologyKey:   corev1.LabelHostname,
			}},
		}
	} else if nodeTerms == nil {
		nodeTerms = d.terms
	}
	if nodeTerms != nil {
		aff.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: nodeTerms.DeepCopy()}
	}
	if aff.PodAffinity == nil && aff.NodeAffinity == nil {
		return nil
	}
	return &aff
}

func dataPlacement(ctx context.Context, c client.Client, f *forgejov1.Forgejo) (dataConstraint, error) {
	d := dataConstraint{selector: forgejo.SelectorLabels(f)}
	pv, _, err := claimVolume(ctx, c, f.Namespace, DataClaimName(f))
	if err != nil {
		return d, err
	}
	shared := pv != nil && sharedAccess(pv.Spec.AccessModes)
	if !shared {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: DataClaimName(f)}, pvc); err == nil {
			shared = sharedAccess(pvc.Spec.AccessModes)
		} else if !apierrors.IsNotFound(err) {
			return d, err
		}
	}
	if !shared {
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(d.selector)); err != nil {
			return d, err
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == "" || p.DeletionTimestamp != nil ||
				p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			d.nodeName = p.Spec.NodeName
			return d, nil
		}
	}
	if pv != nil {
		d.terms, d.pvName = pvNodeSelector(pv), pv.Name
	}
	return d, nil
}

// claimVolume returns the PV bound to the claim. bound is false while the claim does not
// exist or has no volume yet; pv may be nil for a bound claim whose PV cannot be read.
func claimVolume(ctx context.Context, c client.Client, namespace, claim string) (*corev1.PersistentVolume, bool, error) {
	if claim == "" {
		return nil, false, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claim}, pvc); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if pvc.Spec.VolumeName == "" {
		return nil, false, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	return pv, true, nil
}

func pvNodeSelector(pv *corev1.PersistentVolume) *corev1.NodeSelector {
	if pv == nil || pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil ||
		len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		return nil
	}
	return pv.Spec.NodeAffinity.Required
}

// sharedAccess reports whether a volume with these access modes can be attached to several
// nodes at once.
func sharedAccess(modes []corev1.PersistentVolumeAccessMode) bool {
	for _, m := range modes {
		if m == corev1.ReadWriteMany || m == corev1.ReadOnlyMany {
			return true
		}
	}
	return false
}

// IntersectNodeSelectors returns a selector that matches the nodes both a and b match: the
// cross product of their terms (terms are ORed, the requirements within a term ANDed). A nil
// selector matches every node.
func IntersectNodeSelectors(a, b *corev1.NodeSelector) *corev1.NodeSelector {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b.DeepCopy()
	case b == nil:
		return a.DeepCopy()
	}
	out := &corev1.NodeSelector{}
	for _, ta := range a.NodeSelectorTerms {
		for _, tb := range b.NodeSelectorTerms {
			t := corev1.NodeSelectorTerm{
				MatchExpressions: appendMissing(ta.MatchExpressions, tb.MatchExpressions),
				MatchFields:      appendMissing(ta.MatchFields, tb.MatchFields),
			}
			out.NodeSelectorTerms = append(out.NodeSelectorTerms, *t.DeepCopy())
		}
	}
	return out
}

// appendMissing returns a followed by the requirements of b that a does not contain yet.
func appendMissing(a, b []corev1.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	out := append([]corev1.NodeSelectorRequirement{}, a...)
	for _, r := range b {
		if !slices.ContainsFunc(out, func(o corev1.NodeSelectorRequirement) bool { return equality.Semantic.DeepEqual(o, r) }) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func anyNodeMatches(sel *corev1.NodeSelector, nodes []corev1.Node) bool {
	for i := range nodes {
		if NodeSelectorMatches(sel, &nodes[i]) {
			return true
		}
	}
	return false
}

// NodeSelectorMatches evaluates a required node selector against a node like the scheduler:
// a nil selector matches every node, the terms are ORed, and an empty term matches nothing.
func NodeSelectorMatches(sel *corev1.NodeSelector, node *corev1.Node) bool {
	if sel == nil {
		return true
	}
	for _, t := range sel.NodeSelectorTerms {
		if termMatches(t, node) {
			return true
		}
	}
	return false
}

func termMatches(t corev1.NodeSelectorTerm, node *corev1.Node) bool {
	if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
		return false
	}
	for _, r := range t.MatchExpressions {
		v, ok := node.Labels[r.Key]
		if !requirementMatches(r, v, ok) {
			return false
		}
	}
	for _, r := range t.MatchFields {
		if r.Key != metav1.ObjectNameField {
			return false
		}
		if !requirementMatches(r, node.Name, true) {
			return false
		}
	}
	return true
}

func requirementMatches(r corev1.NodeSelectorRequirement, value string, present bool) bool {
	in := func() bool {
		return slices.Contains(r.Values, value)
	}
	compare := func(want func(have, limit int64) bool) bool {
		if !present || len(r.Values) != 1 {
			return false
		}
		have, err1 := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		limit, err2 := strconv.ParseInt(r.Values[0], 10, 64)
		return err1 == nil && err2 == nil && want(have, limit)
	}
	switch r.Operator {
	case corev1.NodeSelectorOpIn:
		return present && in()
	case corev1.NodeSelectorOpNotIn:
		return !present || !in()
	case corev1.NodeSelectorOpExists:
		return present
	case corev1.NodeSelectorOpDoesNotExist:
		return !present
	case corev1.NodeSelectorOpGt:
		return compare(func(h, l int64) bool { return h > l })
	case corev1.NodeSelectorOpLt:
		return compare(func(h, l int64) bool { return h < l })
	}
	return false
}
