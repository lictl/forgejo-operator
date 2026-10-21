package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

const (
	oomGrowthPercent = 50
	// usage-based right-sizing
	sampleWindow       = 120 // samples kept per instance (about 2h at the 60s resync)
	minSamples         = 10
	rightSizeCooldown  = 30 * time.Minute
	rightSizeThreshold = 0.20
	defaultTargetUtil  = 70
)

var (
	podMetricsGVK = schema.GroupVersionKind{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetrics"}
	minCPU        = resource.MustParse("10m")
	minMemory     = resource.MustParse("32Mi")
)

type usageSample struct {
	at     time.Time
	cpu    int64 // millicores
	memory int64 // bytes
}

// planVertical applies status.autoPilot.appliedResources through rc.ResourcesOverride while
// vertical scaling is enabled, and forgets operator-chosen resources once it is disabled.
func planVertical(rc *forgejo.ReconcileContext) {
	f := rc.Instance
	ap := f.Status.AutoPilot
	if !forgejo.VerticalAutoscalingEnabled(f) {
		if ap != nil {
			ap.AppliedResources = nil
			ap.RecommendedResources = nil
		}
		return
	}
	if ap != nil && ap.AppliedResources != nil {
		rc.ResourcesOverride = ap.AppliedResources.DeepCopy()
	}
}

// effectiveResources are the Forgejo container resources the workload step deploys.
func effectiveResources(rc *forgejo.ReconcileContext) corev1.ResourceRequirements {
	if rc.ResourcesOverride != nil {
		return *rc.ResourcesOverride.DeepCopy()
	}
	if ap := rc.Instance.Status.AutoPilot; forgejo.VerticalAutoscalingEnabled(rc.Instance) && ap != nil && ap.AppliedResources != nil {
		return *ap.AppliedResources.DeepCopy()
	}
	return *rc.Instance.Spec.Resources.DeepCopy()
}

// handleOOM raises the memory request and limit by 50% (capped at maxAllowed) when a
// container of a pod running the current memory limit was OOM-killed. It reports whether an
// OOM was seen, so usage-based right-sizing stays out of the way.
func handleOOM(rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod) bool {
	f := rc.Instance
	if rc.Maintenance || rc.Upgrading {
		return false
	}
	cur := effectiveResources(rc)
	curLimit, curReq := cur.Limits.Memory(), cur.Requests.Memory()
	since := lastAction(f, ActionResourcesScaled)

	var victim, container string
	for i := range pods {
		p := &pods[i]
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			t := cs.LastTerminationState.Terminated
			if cs.State.Terminated != nil {
				t = cs.State.Terminated
			}
			if t == nil || t.Reason != "OOMKilled" || !t.FinishedAt.After(since) {
				continue
			}
			// Pods still running an older, smaller limit were already handled.
			if spec := podContainer(p, cs.Name); spec == nil || !spec.Resources.Limits.Memory().Equal(*curLimit) {
				continue
			}
			victim, container = p.Name, cs.Name
		}
	}
	if victim == "" {
		return false
	}

	maxMem := maxAllowed(f, corev1.ResourceMemory)
	grow := func(q *resource.Quantity) resource.Quantity {
		n := *resource.NewQuantity(roundMiB(q.Value()*(100+oomGrowthPercent)/100), resource.BinarySI)
		if maxMem != nil && n.Cmp(*maxMem) > 0 {
			n = maxMem.DeepCopy()
		}
		return n
	}
	next := cur.DeepCopy()
	if next.Limits == nil {
		next.Limits = corev1.ResourceList{}
	}
	if next.Requests == nil {
		next.Requests = corev1.ResourceList{}
	}
	switch {
	case !curLimit.IsZero():
		next.Limits[corev1.ResourceMemory] = grow(curLimit)
		if !curReq.IsZero() {
			next.Requests[corev1.ResourceMemory] = minQuantity(grow(curReq), next.Limits[corev1.ResourceMemory])
		}
	case !curReq.IsZero():
		// No limit: the kill came from node memory pressure; reserve more memory instead.
		next.Requests[corev1.ResourceMemory] = grow(curReq)
	default:
		warnEvery(rc, st, "oom-unbounded", time.Hour, "OOMWithoutResources", "container %s of pod %s was OOM-killed but has no memory request or limit to raise", container, victim)
		return true
	}
	if next.Limits.Memory().Equal(*curLimit) && next.Requests.Memory().Equal(*curReq) {
		warnEvery(rc, st, "oom-max", time.Hour, "OOMAtMaxAllowed", "container %s of pod %s was OOM-killed at the vertical maxAllowed memory %s", container, victim, curLimit.String())
		return true
	}

	ap := autoPilotStatus(rc)
	ap.AppliedResources = next
	rc.RecordAutoPilotAction(ActionResourcesScaled, fmt.Sprintf("container %s of pod %s was OOMKilled; memory limit %s -> %s, request %s -> %s",
		container, victim, curLimit.String(), next.Limits.Memory().String(), curReq.String(), next.Requests.Memory().String()))
	rc.Requeue(time.Second)
	return true
}

// rightSize recommends requests from the 90th percentile of observed usage divided by the
// target utilization, clamped to minAllowed/maxAllowed. Recommendations are applied when
// they differ by more than 20%, at most every 30 minutes. Limits are only ever raised (to fit
// a larger request); lowering them is left to the user so right-sizing cannot cause OOM loops.
func rightSize(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod, oomSeen bool) {
	f := rc.Instance
	if rc.Maintenance || rc.Upgrading || !rc.HasAPI(podMetricsGVK) {
		return
	}
	if err := samplePodMetrics(ctx, rc, st, pods); err != nil {
		logger(ctx).V(1).Info("cannot read pod metrics", "error", err.Error())
	}
	if len(st.samples) < minSamples {
		return
	}
	target := int64(f.Spec.Autoscaling.Vertical.TargetUtilizationPercentage)
	if target <= 0 || target > 100 {
		target = defaultTargetUtil
	}
	cpus := make([]int64, 0, len(st.samples))
	mems := make([]int64, 0, len(st.samples))
	for _, s := range st.samples {
		cpus = append(cpus, s.cpu)
		mems = append(mems, s.memory)
	}
	recCPU := clampQuantity(f, corev1.ResourceCPU, *resource.NewMilliQuantity(percentile(cpus, 90)*100/target, resource.DecimalSI), minCPU)
	recMem := clampQuantity(f, corev1.ResourceMemory, *resource.NewQuantity(roundMiB(percentile(mems, 90)*100/target), resource.BinarySI), minMemory)

	cur := effectiveResources(rc)
	rec := cur.DeepCopy()
	if rec.Requests == nil {
		rec.Requests = corev1.ResourceList{}
	}
	rec.Requests[corev1.ResourceCPU] = recCPU
	rec.Requests[corev1.ResourceMemory] = recMem
	for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if lim, ok := rec.Limits[n]; ok && lim.Cmp(rec.Requests[n]) < 0 {
			rec.Limits[n] = rec.Requests[n].DeepCopy()
		}
	}
	ap := autoPilotStatus(rc)
	ap.RecommendedResources = rec

	if oomSeen || inCooldown(f, ActionResourcesScaled, rightSizeCooldown) {
		return
	}
	if !differs(cur.Requests.Cpu(), &recCPU) && !differs(cur.Requests.Memory(), &recMem) {
		return
	}
	ap.AppliedResources = rec.DeepCopy()
	rc.RecordAutoPilotAction(ActionResourcesScaled, fmt.Sprintf("usage-based right-sizing (p90 of %d samples at %d%% target): %s -> %s",
		len(st.samples), target, describeResources(&cur), describeResources(rec)))
	rc.Requeue(time.Second)
}

// podMetricsList is the subset of metrics.k8s.io/v1beta1 PodMetricsList the operator reads.
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Timestamp  time.Time `json:"timestamp"`
		Containers []struct {
			Name  string              `json:"name"`
			Usage corev1.ResourceList `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

func samplePodMetrics(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod) error {
	cs, _, err := kube()
	if err != nil {
		return err
	}
	f := rc.Instance
	raw, err := cs.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", f.Namespace, "pods").
		Param("labelSelector", labels.SelectorFromSet(forgejo.SelectorLabels(f)).String()).
		DoRaw(ctx)
	if err != nil {
		return err
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	ready := map[string]bool{}
	for i := range pods {
		ready[pods[i].Name] = readyFor(&pods[i]) > 0
	}
	for _, m := range list.Items {
		// Starting pods (migrations, cache warm-up) are not representative.
		if !ready[m.Metadata.Name] || !m.Timestamp.After(st.lastSeen[m.Metadata.Name]) {
			continue
		}
		for _, c := range m.Containers {
			if c.Name != serverContainerName {
				continue
			}
			st.lastSeen[m.Metadata.Name] = m.Timestamp
			st.samples = append(st.samples, usageSample{at: m.Timestamp, cpu: c.Usage.Cpu().MilliValue(), memory: c.Usage.Memory().Value()})
		}
	}
	for name := range st.lastSeen {
		if _, ok := ready[name]; !ok {
			delete(st.lastSeen, name)
		}
	}
	if n := len(st.samples); n > sampleWindow {
		st.samples = slices.Clone(st.samples[n-sampleWindow:])
	}
	return nil
}

func podContainer(p *corev1.Pod, name string) *corev1.Container {
	for _, list := range [][]corev1.Container{p.Spec.InitContainers, p.Spec.Containers} {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
	}
	return nil
}

func maxAllowed(f *forgejov1.Forgejo, n corev1.ResourceName) *resource.Quantity {
	if v := f.Spec.Autoscaling.Vertical; v != nil {
		if q, ok := v.MaxAllowed[n]; ok {
			return &q
		}
	}
	return nil
}

func clampQuantity(f *forgejov1.Forgejo, n corev1.ResourceName, q, floor resource.Quantity) resource.Quantity {
	v := f.Spec.Autoscaling.Vertical
	if lo, ok := v.MinAllowed[n]; ok && q.Cmp(lo) < 0 {
		q = lo.DeepCopy()
	}
	if q.Cmp(floor) < 0 {
		q = floor.DeepCopy()
	}
	if hi, ok := v.MaxAllowed[n]; ok && q.Cmp(hi) > 0 {
		q = hi.DeepCopy()
	}
	return q
}

func minQuantity(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) > 0 {
		return b
	}
	return a
}

func differs(cur, rec *resource.Quantity) bool {
	if cur.IsZero() {
		return true
	}
	c, r := float64(cur.MilliValue()), float64(rec.MilliValue())
	d := (r - c) / c
	return d > rightSizeThreshold || d < -rightSizeThreshold
}

func percentile(v []int64, p int) int64 {
	s := slices.Clone(v)
	slices.Sort(s)
	i := (len(s)*p + 99) / 100
	return s[min(max(i-1, 0), len(s)-1)]
}

func roundMiB(v int64) int64 {
	return (v + 1<<20 - 1) &^ (1<<20 - 1)
}
