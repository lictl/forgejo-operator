package autopilot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;patch

const (
	storageCheckInterval = time.Minute
	// duInterval throttles the du fallback, which walks the whole volume.
	duInterval = 5 * time.Minute
	// expansionSettle gives the CSI driver/kubelet time to report the new size.
	expansionSettle = 5 * time.Minute

	cnpgClusterLabel = "cnpg.io/cluster"
	cnpgPVCRoleLabel = "cnpg.io/pvcRole"
)

type duResult struct {
	used int64
	at   time.Time
}

type claimRole int

const (
	roleData claimRole = iota
	roleDB
	roleWAL
)

// reconcileStorage measures the data and CNPG PVCs, reports status.storage, the
// VolumeUsedRatio metric and StorageHealthy, and expands volumes above the threshold.
func reconcileStorage(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState) error {
	f := rc.Instance
	if !forgejo.StorageAutoExpandEnabled(f) {
		f.Status.Storage = nil
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionStorageHealthy)
		forgejo.VolumeUsedRatio.DeletePartialMatch(map[string]string{"namespace": f.Namespace, "name": f.Name})
		return nil
	}
	if f.Status.Storage != nil && time.Since(st.lastStorageCheck) < storageCheckInterval {
		return nil
	}
	st.lastStorageCheck = time.Now()

	claims, err := instanceClaims(ctx, rc)
	if err != nil {
		return err
	}
	podList := &corev1.PodList{}
	if err := rc.Client.List(ctx, podList, client.InNamespace(f.Namespace)); err != nil {
		return err
	}
	kubelet := kubeletStats(ctx, podList.Items, claims, f.Namespace)

	prev := map[string]forgejov1.VolumeUsage{}
	if f.Status.Storage != nil {
		for _, v := range f.Status.Storage.Volumes {
			prev[v.ClaimName] = v
		}
	}
	ae := f.Spec.AutoPilot.StorageAutoExpand
	threshold := int64(ae.ThresholdPercent)
	if threshold <= 0 {
		threshold = 80
	}
	maxSize := forgejo.QuantityOr(ae.MaxSize, "200Gi")
	scCache := map[string]bool{}

	var volumes []forgejov1.VolumeUsage
	var problems []string
	problemReason, expanding := "", false
	expandedRoles := map[claimRole]bool{}
	for _, c := range claims {
		pvc := c.pvc
		req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		vu := forgejov1.VolumeUsage{ClaimName: pvc.Name, RequestedSize: req.String(), LastExpansionTime: prev[pvc.Name].LastExpansionTime}
		vu.ExpansionSupported = expansionSupported(ctx, pvc, scCache)

		used, capacity, ok := measureClaim(ctx, st, podList.Items, pvc, kubelet)
		if !ok {
			if p, found := prev[pvc.Name]; found && p.CapacityBytes > 0 {
				used, capacity, ok = p.UsedBytes, p.CapacityBytes, true
			}
		}
		if !ok || capacity <= 0 {
			volumes = append(volumes, vu)
			continue
		}
		vu.UsedBytes, vu.CapacityBytes = used, capacity
		vu.UsedPercent = int32(used * 100 / capacity)
		forgejo.VolumeUsedRatio.WithLabelValues(f.Namespace, f.Name, pvc.Name).Set(float64(used) / float64(capacity))

		if int64(vu.UsedPercent) >= threshold {
			switch {
			case !vu.ExpansionSupported:
				problemReason = ReasonExpansionUnsupported
				problems = append(problems, fmt.Sprintf("%s is %d%% full and its StorageClass does not allow volume expansion", pvc.Name, vu.UsedPercent))
				warnEvery(rc, st, "storage-unsupported/"+pvc.Name, time.Hour, ReasonExpansionUnsupported,
					"PVC %s is %d%% full (threshold %d%%) but its StorageClass does not allow expansion; grow it manually", pvc.Name, vu.UsedPercent, threshold)
			case req.Cmp(maxSize) >= 0:
				if problemReason == "" {
					problemReason = ReasonMaxSizeReached
				}
				problems = append(problems, fmt.Sprintf("%s is %d%% full at the autoPilot.storageAutoExpand.maxSize %s", pvc.Name, vu.UsedPercent, maxSize.String()))
				warnEvery(rc, st, "storage-max/"+pvc.Name, time.Hour, ReasonMaxSizeReached,
					"PVC %s is %d%% full and already at maxSize %s", pvc.Name, vu.UsedPercent, maxSize.String())
			case resizePending(pvc) || (vu.LastExpansionTime != nil && time.Since(vu.LastExpansionTime.Time) < expansionSettle) || expandedRoles[c.role]:
				expanding = true
			default:
				newSize := grownSize(req, int64(ae.IncrementPercent), maxSize)
				if err := expandClaim(ctx, rc, c, newSize); err != nil {
					return err
				}
				expandedRoles[c.role] = true
				expanding = true
				now := metav1.Now()
				vu.LastExpansionTime, vu.RequestedSize = &now, newSize.String()
				rc.RecordAutoPilotAction(ActionStorageExpanded, fmt.Sprintf("PVC %s was %d%% full (threshold %d%%); requested size %s -> %s",
					pvc.Name, vu.UsedPercent, threshold, req.String(), newSize.String()))
			}
		}
		volumes = append(volumes, vu)
	}

	now := metav1.Now()
	f.Status.Storage = &forgejov1.StorageStatus{Volumes: volumes, LastCheckTime: &now}
	switch {
	case len(problems) > 0:
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionFalse, problemReason, strings.Join(problems, "; "))
	case expanding:
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionTrue, "Expanding", "a volume above the threshold is being expanded")
	case !anyMeasured(volumes):
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionUnknown, "MeasurementUnavailable", "no volume usage could be measured yet")
	default:
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionTrue, "WithinThreshold",
			fmt.Sprintf("all volumes are below %d%% used", threshold))
	}
	return nil
}

type instanceClaim struct {
	pvc  *corev1.PersistentVolumeClaim
	role claimRole
}

func instanceClaims(ctx context.Context, rc *forgejo.ReconcileContext) ([]instanceClaim, error) {
	f := rc.Instance
	var out []instanceClaim
	data := &corev1.PersistentVolumeClaim{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: dataClaimName(f)}, data); err == nil {
		out = append(out, instanceClaim{pvc: data, role: roleData})
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	if forgejo.DatabaseType(f) != forgejov1.DatabaseTypeCNPG {
		return out, nil
	}
	list := &corev1.PersistentVolumeClaimList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace),
		client.MatchingLabels{cnpgClusterLabel: forgejo.DatabaseClusterName(f)}); err != nil {
		return nil, err
	}
	for i := range list.Items {
		pvc := &list.Items[i]
		switch pvc.Labels[cnpgPVCRoleLabel] {
		case "PG_WAL":
			out = append(out, instanceClaim{pvc: pvc, role: roleWAL})
		case "PG_DATA", "":
			out = append(out, instanceClaim{pvc: pvc, role: roleDB})
		}
	}
	return out, nil
}

// kubeletStats fetches kubelet volume stats from every node running a pod that mounts one
// of the claims. Failures are logged and leave the du fallback in charge.
func kubeletStats(ctx context.Context, pods []corev1.Pod, claims []instanceClaim, namespace string) map[string]volumeStat {
	wanted := map[string]bool{}
	for _, c := range claims {
		wanted[c.pvc.Name] = true
	}
	nodes := map[string]bool{}
	for i := range pods {
		for _, v := range pods[i].Spec.Volumes {
			if v.PersistentVolumeClaim != nil && wanted[v.PersistentVolumeClaim.ClaimName] && pods[i].Spec.NodeName != "" {
				nodes[pods[i].Spec.NodeName] = true
			}
		}
	}
	out := map[string]volumeStat{}
	for node := range nodes {
		stats, err := nodeVolumeStats(ctx, node, namespace)
		if err != nil {
			logger(ctx).V(1).Info("kubelet volume stats unavailable", "node", node, "error", err.Error())
			continue
		}
		for k, v := range stats {
			out[k] = v
		}
	}
	return out
}

// measureClaim returns used bytes and the capacity to compare them with. Kubelet stats are
// used when they describe a dedicated volume. Provisioners such as local-path report the
// whole node filesystem (or nothing at all for hostPath), so then the operator runs du in a
// container that mounts the claim, at most every duInterval, and compares the result with
// the PVC's requested size.
func measureClaim(ctx context.Context, st *instanceState, pods []corev1.Pod, pvc *corev1.PersistentVolumeClaim, kubelet map[string]volumeStat) (int64, int64, bool) {
	req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	requested := req.Value()
	if s, ok := kubelet[pvc.Name]; ok && s.capacity > 0 && (requested == 0 || s.capacity <= requested*5/4) {
		return s.used, s.capacity, true
	}
	if requested <= 0 {
		return 0, 0, false
	}
	if r, ok := st.du[pvc.Name]; ok && time.Since(r.at) < duInterval {
		return r.used, requested, true
	}
	pod, container, path := claimMount(pods, pvc.Name)
	if pod == "" {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := execInPod(ctx, pvc.Namespace, pod, container, []string{"du", "-skx", path})
	fields := strings.Fields(out)
	kib, perr := int64(0), fmt.Errorf("no output")
	if len(fields) > 0 {
		kib, perr = strconv.ParseInt(fields[0], 10, 64)
	}
	if perr != nil {
		// du exits non-zero for unreadable entries but still prints the total.
		logger(ctx).Info("cannot measure volume usage with du", "claim", pvc.Name, "pod", pod, "error", fmt.Sprint(err, perr))
		return 0, 0, false
	}
	st.du[pvc.Name] = duResult{used: kib * 1024, at: time.Now()}
	return kib * 1024, requested, true
}

// claimMount finds a running container that mounts the whole claim.
func claimMount(pods []corev1.Pod, claim string) (pod, container, path string) {
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != claim {
				continue
			}
			for _, c := range p.Spec.Containers {
				cs := containerStatus(p, c.Name)
				if cs == nil || cs.State.Running == nil {
					continue
				}
				for _, m := range c.VolumeMounts {
					if m.Name == v.Name && m.SubPath == "" && m.SubPathExpr == "" {
						return p.Name, c.Name, m.MountPath
					}
				}
			}
		}
	}
	return "", "", ""
}

func expansionSupported(ctx context.Context, pvc *corev1.PersistentVolumeClaim, cache map[string]bool) bool {
	name := ""
	if pvc.Spec.StorageClassName != nil {
		name = *pvc.Spec.StorageClassName
	}
	if name == "" {
		return false
	}
	if v, ok := cache[name]; ok {
		return v
	}
	cs, _, err := kube()
	if err != nil {
		return false
	}
	sc, err := cs.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	ok := err == nil && sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion
	cache[name] = ok
	return ok
}

func resizePending(pvc *corev1.PersistentVolumeClaim) bool {
	req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
	if ok && capacity.Cmp(req) < 0 {
		return true
	}
	for _, c := range pvc.Status.Conditions {
		if (c.Type == corev1.PersistentVolumeClaimResizing || c.Type == corev1.PersistentVolumeClaimFileSystemResizePending) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// grownSize adds incrementPercent (rounded up to whole MiB) and caps at maxSize.
func grownSize(cur resource.Quantity, incrementPercent int64, maxSize resource.Quantity) resource.Quantity {
	if incrementPercent <= 0 {
		incrementPercent = 50
	}
	n := *resource.NewQuantity(roundMiB(cur.Value()*(100+incrementPercent)/100), resource.BinarySI)
	if n.Cmp(maxSize) > 0 {
		return maxSize.DeepCopy()
	}
	return n
}

// expandClaim grows the data PVC directly, and CNPG volumes through the Cluster spec so
// that CNPG resizes every instance consistently.
func expandClaim(ctx context.Context, rc *forgejo.ReconcileContext, c instanceClaim, size resource.Quantity) error {
	if c.role == roleData {
		patch := client.MergeFrom(c.pvc.DeepCopy())
		c.pvc.Spec.Resources.Requests[corev1.ResourceStorage] = size
		return rc.Client.Patch(ctx, c.pvc, patch)
	}
	field := "storage"
	if c.role == roleWAL {
		field = "walStorage"
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	cluster.SetNamespace(rc.Instance.Namespace)
	cluster.SetName(forgejo.DatabaseClusterName(rc.Instance))
	body := fmt.Sprintf(`{"spec":{%q:{"size":%q}}}`, field, size.String())
	return rc.Client.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, []byte(body)))
}

func anyMeasured(volumes []forgejov1.VolumeUsage) bool {
	for _, v := range volumes {
		if v.CapacityBytes > 0 {
			return true
		}
	}
	return false
}
