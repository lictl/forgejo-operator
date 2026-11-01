package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

func boundClaim(name, volume string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes, VolumeName: volume},
	}
}

func hostPV(name, host string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
	pv.Spec.AccessModes = modes
	if host != "" {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{host}}},
		}}}}
	}
	return pv
}

func node(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}}}
}

func serverPod(f *forgejov1.Forgejo) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fb-0", Namespace: "ns", Labels: forgejo.SelectorLabels(f)}}
	p.Spec.NodeName = "n1"
	p.Status.Phase = corev1.PodRunning
	return p
}

func TestPlacement(t *testing.T) {
	s := FakeScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	rwx := corev1.ReadWriteMany

	hasPodAffinity := func(a *corev1.Affinity) bool { return a != nil && a.PodAffinity != nil }
	nodeTerms := func(a *corev1.Affinity) string {
		if a == nil || a.NodeAffinity == nil {
			return ""
		}
		var hosts []string
		for _, t := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, r := range t.MatchExpressions {
				hosts = append(hosts, r.Values...)
			}
		}
		return strings.Join(hosts, "+")
	}

	cases := []struct {
		name       string
		objs       []client.Object
		mountsData bool
		podAff     bool
		nodes      string
		conflict   bool
	}{{
		name:       "RWO data with pods: join the pods",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "")},
		mountsData: true, podAff: true,
	}, {
		name: "RWX data with pods: no pod affinity",
		objs: []client.Object{boundClaim("fb-data", "pv-data", rwx), hostPV("pv-data", "", rwx), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, nodes: "n2",
	}, {
		name:       "database-only first bind lands next to the data",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "")},
		mountsData: false, podAff: true,
	}, {
		name:       "cleanup of a bound backup volume: no data constraint",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2")},
		mountsData: false,
	}, {
		name: "backup volume on another node than the running pods",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, conflict: true,
	}, {
		name: "backup volume next to the running pods",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n1"), node("n1"), node("n2")},
		mountsData: true, podAff: true, nodes: "n1",
	}, {
		name: "no pods: both PVs' node affinities, disjoint",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, conflict: true,
	}, {
		name: "no pods: both PVs' node affinities, same node",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n1"), node("n1"), node("n2")},
		mountsData: true, nodes: "n1",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			aff, err := Placement(ctx, c, f, tc.mountsData, "fb-backups")
			var conflict *VolumeConflictError
			if got := errors.As(err, &conflict); got != tc.conflict {
				t.Fatalf("conflict=%v (err %v), want %v", got, err, tc.conflict)
			}
			if tc.conflict {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if hasPodAffinity(aff) != tc.podAff || nodeTerms(aff) != tc.nodes {
				t.Errorf("pod affinity=%v node terms=%q, want %v %q", hasPodAffinity(aff), nodeTerms(aff), tc.podAff, tc.nodes)
			}
		})
	}
}
