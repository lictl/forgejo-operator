package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=list

const (
	oomGrowthPercent = 50
	// Without autoscaling.vertical.maxAllowed.memory, OOM growth stops at
	// max(defaultOOMCeilingFactor × the spec's memory, defaultOOMCeilingFloor).
	defaultOOMCeilingFactor = 4
	defaultOOMCeilingFloor  = 1 << 30
	// nodeRequestPercent of the largest node's allocatable memory is the most OOM growth
	// requests, so the pod still fits a node.
	nodeRequestPercent = 90
	// unschedulableAfter is how long a pod may stay unschedulable for lack of memory before
	// the resource change that caused it is reverted.
	unschedulableAfter = 2 * time.Minute
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

// annotationVerticalBase on the Forgejo Deployment holds specResourcesHash of the
// spec.resources that status.autoPilot.appliedResources were derived from.
const annotationVerticalBase = "forgejo.lucas.icu/vertical-base"

// specResourcesHash identifies spec.resources (quantities in canonical form).
func specResourcesHash(f *forgejov1.Forgejo) string {
	b, _ := json.Marshal(f.Spec.Resources)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// planVertical applies status.autoPilot.appliedResources through rc.ResourcesOverride while
// vertical scaling is enabled, and forgets operator-chosen resources once it is disabled.
// Applied resources derived from an older spec.resources are dropped, so a spec change
// takes effect, and the rest are kept within minAllowed/maxAllowed. Neither happens while a
// migration holds the workload, since both roll the pods.
func planVertical(ctx context.Context, rc *forgejo.ReconcileContext) {
	f := rc.Instance
	ap := f.Status.AutoPilot
	if !forgejo.VerticalAutoscalingEnabled(f) {
		if ap != nil {
			ap.AppliedResources = nil
			ap.RecommendedResources = nil
		}
		return
	}
	if ap == nil || ap.AppliedResources == nil {
		return
	}
	if !workloadHeld(rc) {
		if base := verticalBase(ctx, rc); base != "" && base != specResourcesHash(f) {
			rc.Event(corev1.EventTypeNormal, "VerticalRebased", "AutoPilot",
				"spec.resources changed; dropping applied resources (%s) and starting again from spec.resources (%s)",
				describeResources(ap.AppliedResources), describeResources(&f.Spec.Resources))
			ap.AppliedResources = nil
			return
		}
		if clamped := clampResources(f, ap.AppliedResources); !equality.Semantic.DeepEqual(clamped, ap.AppliedResources) {
			rc.Event(corev1.EventTypeNormal, "VerticalClamped", "AutoPilot",
				"applied resources %s -> %s to stay within autoscaling.vertical minAllowed/maxAllowed",
				describeResources(ap.AppliedResources), describeResources(clamped))
			ap.AppliedResources = clamped
		}
	}
	rc.ResourcesOverride = ap.AppliedResources.DeepCopy()
}

// verticalBase returns the vertical-base annotation of the Forgejo Deployment, or "".
func verticalBase(ctx context.Context, rc *forgejo.ReconcileContext) string {
	dep := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: rc.Instance.Namespace, Name: forgejo.DeploymentName(rc.Instance)}, dep); err != nil {
		return ""
	}
	return dep.Annotations[annotationVerticalBase]
}

// recordVerticalBase stores which spec.resources the applied resources derive from: after
// vertical scaling changed them, and once for applied resources recorded before the
// annotation existed.
func recordVerticalBase(ctx context.Context, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, scaled bool) error {
	f := rc.Instance
	ap := f.Status.AutoPilot
	if dep == nil || ap == nil || ap.AppliedResources == nil {
		return nil
	}
	h := specResourcesHash(f)
	cur, ok := dep.Annotations[annotationVerticalBase]
	if cur == h || (ok && !scaled) {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[annotationVerticalBase] = h
	return client.IgnoreNotFound(rc.Client.Patch(ctx, dep, patch))
}

// clampResources keeps CPU and memory within autoscaling.vertical bounds: requests within
// [minAllowed, maxAllowed], limits at most maxAllowed and requests at most limits.
func clampResources(f *forgejov1.Forgejo, r *corev1.ResourceRequirements) *corev1.ResourceRequirements {
	out := r.DeepCopy()
	v := f.Spec.Autoscaling.Vertical
	if v == nil {
		return out
	}
	for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if req, ok := out.Requests[n]; ok {
			if lo, ok := v.MinAllowed[n]; ok && req.Cmp(lo) < 0 {
				req = lo.DeepCopy()
			}
			if hi, ok := v.MaxAllowed[n]; ok && req.Cmp(hi) > 0 {
				req = hi.DeepCopy()
			}
			out.Requests[n] = req
		}
		if lim, ok := out.Limits[n]; ok {
			if hi, ok := v.MaxAllowed[n]; ok && lim.Cmp(hi) > 0 {
				lim = hi.DeepCopy()
				out.Limits[n] = lim
			}
			if req, ok := out.Requests[n]; ok && req.Cmp(lim) > 0 {
				out.Requests[n] = lim.DeepCopy()
			}
		}
	}
	return out
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

// memoryRequest is the memory the scheduler reserves for these resources: the request, or
// the limit the API server copies into it when no request is set.
func memoryRequest(r *corev1.ResourceRequirements) int64 {
	if q := r.Requests.Memory(); !q.IsZero() {
		return q.Value()
	}
	return r.Limits.Memory().Value()
}

// rememberApplied keeps the applied resources before a change, so revertUnschedulable can
// return to them.
func rememberApplied(st *instanceState, ap *forgejov1.AutoPilotStatus) {
	st.prevApplied, st.prevKnown = ap.AppliedResources.DeepCopy(), true
}

// memoryCaps bound OOM growth. limit and request are nil when unbounded.
type memoryCaps struct {
	limit, request       *resource.Quantity
	limitWhy, requestWhy string
}

// oomMemoryCaps: maxAllowed.memory, or without it max(4 × the spec's memory, 1Gi); the
// largest schedulable node's allocatable memory for limits and 90% of it for requests; and a
// request that already failed to schedule.
func oomMemoryCaps(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState) memoryCaps {
	f := rc.Instance
	var c memoryCaps
	if q := maxAllowed(f, corev1.ResourceMemory); q != nil {
		c.limit, c.limitWhy = ptr.To(q.DeepCopy()), "autoscaling.vertical.maxAllowed.memory "+q.String()
	} else if base := memoryRequest(&corev1.ResourceRequirements{Requests: f.Spec.Resources.Requests, Limits: f.Spec.Resources.Limits}); base > 0 {
		def := resource.NewQuantity(roundMiB(max(base*defaultOOMCeilingFactor, defaultOOMCeilingFloor)), resource.BinarySI)
		c.limit, c.limitWhy = def, fmt.Sprintf("the default ceiling %s (%d× spec.resources memory; set autoscaling.vertical.maxAllowed.memory to change it)",
			def.String(), defaultOOMCeilingFactor)
	}
	c.request, c.requestWhy = c.limit, c.limitWhy
	if _, nodeMax, err := nodeCapacityFn(ctx); err == nil && nodeMax > 0 {
		if c.limit == nil || nodeMax < c.limit.Value() {
			c.limit, c.limitWhy = resource.NewQuantity(nodeMax&^(1<<20-1), resource.BinarySI), "the largest node's allocatable memory"
		}
		if req := nodeMax * nodeRequestPercent / 100 &^ (1<<20 - 1); c.request == nil || req < c.request.Value() {
			c.request, c.requestWhy = resource.NewQuantity(req, resource.BinarySI),
				fmt.Sprintf("%d%% of the largest node's allocatable memory", nodeRequestPercent)
		}
	} else if err != nil {
		logger(ctx).V(1).Info("cannot list nodes; OOM growth is not capped by node capacity", "error", err.Error())
	}
	if st.memRequestCap > 0 && (c.request == nil || st.memRequestCap < c.request.Value()) {
		c.request, c.requestWhy = resource.NewQuantity(st.memRequestCap, resource.BinarySI), "a larger memory request could not be scheduled"
	}
	return c
}

// handleOOM raises the memory request and limit by 50% when a container of a pod running the
// current memory limit was OOM-killed, within oomMemoryCaps and never lowering either. It
// reports whether an OOM was seen, so usage-based right-sizing stays out of the way, and
// whether it changed the applied resources.
func handleOOM(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod) (seen, scaled bool) {
	f := rc.Instance
	if rc.Maintenance || workloadHeld(rc) {
		return false, false
	}
	cur := effectiveResources(rc)
	curLimit, curReq := cur.Limits.Memory(), cur.Requests.Memory()
	since := lastAction(f, ActionResourcesScaled)

	victim, container := oomVictim(pods, curLimit, since)
	if victim == "" {
		return false, false
	}
	if curLimit.IsZero() && curReq.IsZero() {
		warnEvery(rc, st, "oom-unbounded", "OOMWithoutResources", "container %s of pod %s was OOM-killed but has no memory request or limit to raise", container, victim)
		return true, false
	}

	caps := oomMemoryCaps(ctx, rc, st)
	grow := func(q *resource.Quantity, limit *resource.Quantity) resource.Quantity {
		n := *resource.NewQuantity(roundMiB(q.Value()*(100+oomGrowthPercent)/100), resource.BinarySI)
		if limit != nil && n.Cmp(*limit) > 0 {
			n = limit.DeepCopy()
		}
		if n.Cmp(*q) < 0 {
			n = q.DeepCopy() // never lower: a value already above its cap stays
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
	why := caps.limitWhy
	switch {
	case !curLimit.IsZero() && !curReq.IsZero():
		next.Limits[corev1.ResourceMemory] = grow(curLimit, caps.limit)
		next.Requests[corev1.ResourceMemory] = minQuantity(grow(curReq, caps.request), next.Limits[corev1.ResourceMemory])
		if next.Requests.Memory().Equal(*curReq) && !curReq.Equal(*curLimit) {
			why = caps.requestWhy
		}
	case !curLimit.IsZero():
		// The API server copies the limit into the request, so the limit is also scheduled.
		limit := caps.limit
		if caps.request != nil && (limit == nil || caps.request.Cmp(*limit) < 0) {
			limit, why = caps.request, caps.requestWhy
		}
		next.Limits[corev1.ResourceMemory] = grow(curLimit, limit)
	default:
		// No limit: the kill came from node memory pressure; reserve more memory instead.
		next.Requests[corev1.ResourceMemory] = grow(curReq, caps.request)
		why = caps.requestWhy
	}
	if next.Limits.Memory().Equal(*curLimit) && next.Requests.Memory().Equal(*curReq) {
		reason := "OOMAtMaxAllowed"
		if !strings.Contains(why, "maxAllowed") && !strings.Contains(why, "default ceiling") {
			reason = "OOMAtNodeCapacity"
		}
		warnEvery(rc, st, "oom-max", reason,
			"container %s of pod %s was OOM-killed with memory limit %s, request %s; not raised further: capped by %s",
			container, victim, curLimit.String(), curReq.String(), why)
		return true, false
	}

	ap := autoPilotStatus(rc)
	rememberApplied(st, ap)
	ap.AppliedResources = next
	rc.RecordAutoPilotAction(ActionResourcesScaled, fmt.Sprintf("container %s of pod %s was OOMKilled; memory limit %s -> %s, request %s -> %s",
		container, victim, curLimit.String(), next.Limits.Memory().String(), curReq.String(), next.Requests.Memory().String()))
	rc.Requeue(time.Second)
	return true, true
}

// oomVictim returns the pod and container of the last OOM kill after since in a pod that runs
// the current memory limit, or empty strings when there is none. Pods still running an older,
// smaller limit were already handled.
func oomVictim(pods []corev1.Pod, curLimit *resource.Quantity, since time.Time) (victim, container string) {
	for i := range pods {
		p := &pods[i]
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			t := cs.LastTerminationState.Terminated
			if cs.State.Terminated != nil {
				t = cs.State.Terminated
			}
			if t == nil || t.Reason != reasonOOMKilled || !t.FinishedAt.After(since) {
				continue
			}
			if spec := podContainer(p, cs.Name); spec == nil || !spec.Resources.Limits.Memory().Equal(*curLimit) {
				continue
			}
			victim, container = p.Name, cs.Name
		}
	}
	return victim, container
}

// revertUnschedulable undoes the last resource change when a server pod that requests the
// current memory has been unschedulable for lack of memory, and keeps OOM growth from
// requesting that much again while the operator runs. It reports whether it reverted.
func revertUnschedulable(rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod) bool {
	f := rc.Instance
	ap := f.Status.AutoPilot
	if ap == nil || ap.AppliedResources == nil || lastAction(f, ActionResourcesScaled).IsZero() {
		return false
	}
	cur := effectiveResources(rc)
	curReq := memoryRequest(&cur)
	var stuck *corev1.Pod
	var msg string
	for i := range pods {
		p := &pods[i]
		c := podCondition(p, corev1.PodScheduled)
		if c == nil || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable ||
			!strings.Contains(c.Message, "Insufficient memory") || time.Since(c.LastTransitionTime.Time) < unschedulableAfter {
			continue
		}
		if spec := podContainer(p, serverContainerName); spec == nil || memoryRequest(&spec.Resources) != curReq {
			continue
		}
		stuck, msg = p, c.Message
		break
	}
	if stuck == nil {
		return false
	}
	var target *corev1.ResourceRequirements // nil: back to spec.resources
	if st.prevKnown {
		target = st.prevApplied.DeepCopy()
	}
	targetRes := f.Spec.Resources
	if target != nil {
		targetRes = *target
	}
	targetReq := memoryRequest(&targetRes)
	if targetReq >= curReq {
		warnEvery(rc, st, "unschedulable", "ResourcesUnschedulable",
			"pod %s cannot be scheduled (%s) and no smaller memory request is known to revert to; lower spec.resources or autoscaling.vertical.maxAllowed",
			stuck.Name, msg)
		return false
	}
	st.memRequestCap, st.prevKnown, st.prevApplied = targetReq, false, nil
	ap.AppliedResources = target
	text := fmt.Sprintf("pod %s requesting %s of memory could not be scheduled for %s (%s); reverted to %s",
		stuck.Name, resource.NewQuantity(curReq, resource.BinarySI).String(), unschedulableAfter, msg, describeResources(&targetRes))
	rc.RecordAutoPilotAction(ActionResourcesScaled, text)
	warnEvery(rc, st, "unschedulable", "ResourcesUnschedulable", "%s", text)
	rc.Requeue(time.Second)
	return true
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// hpaScalesOn reports which resources the HPA scales on (it targets their utilization of
// the requests), which right-sizing must therefore leave alone.
func hpaScalesOn(f *forgejov1.Forgejo) (cpu, memory bool) {
	if !forgejo.HorizontalAutoscalingEnabled(f) {
		return false, false
	}
	m := f.Spec.Autoscaling.Horizontal.TargetMemoryUtilizationPercentage
	return true, m != nil && *m > 0
}

// rightSize recommends requests from the 90th percentile of observed usage divided by the
// target utilization, clamped to minAllowed/maxAllowed. Recommendations are applied when
// they differ by more than 20%, at most every 30 minutes. Limits are only ever raised (to fit
// a larger request); lowering them is left to the user so right-sizing cannot cause OOM loops.
// Requests the HPA scales on (CPU, and memory with a memory target) are left alone. It
// reports whether it changed the applied resources.
func rightSize(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod, suppress bool) bool {
	if rc.Maintenance || workloadHeld(rc) || !rc.HasAPI(podMetricsGVK) {
		return false
	}
	if err := samplePodMetrics(ctx, rc, st, pods); err != nil {
		logger(ctx).V(1).Info("cannot read pod metrics", "error", err.Error())
	}
	return applyRecommendation(rc, st, suppress)
}

// applyRecommendation computes the recommendation from the usage samples, publishes it and
// applies it when due.
func applyRecommendation(rc *forgejo.ReconcileContext, st *instanceState, suppress bool) bool {
	f := rc.Instance
	if len(st.samples) < minSamples {
		return false
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
	if st.memRequestCap > 0 && recMem.Value() > st.memRequestCap {
		recMem = *resource.NewQuantity(st.memRequestCap, resource.BinarySI)
	}

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

	hpaCPU, hpaMem := hpaScalesOn(f)
	apply := rec.DeepCopy()
	keep := func(n corev1.ResourceName) {
		for _, pair := range []struct{ from, to corev1.ResourceList }{{cur.Requests, apply.Requests}, {cur.Limits, apply.Limits}} {
			if pair.to == nil {
				continue
			}
			if q, ok := pair.from[n]; ok {
				pair.to[n] = q.DeepCopy()
			} else {
				delete(pair.to, n)
			}
		}
	}
	var held []string
	if hpaCPU {
		keep(corev1.ResourceCPU)
		held = append(held, "cpu")
	}
	if hpaMem {
		keep(corev1.ResourceMemory)
		held = append(held, "memory")
	}
	if len(held) > 0 && st.allow("vertical-hpa", time.Hour) {
		rc.Event(corev1.EventTypeNormal, "VerticalLimitedByHPA", "AutoPilot",
			"the HPA scales on %s utilization of the requests, so usage-based right-sizing leaves those requests alone (OOM-driven memory growth still applies)",
			strings.Join(held, " and "))
	}

	if suppress || inCooldown(f, ActionResourcesScaled, rightSizeCooldown) {
		return false
	}
	if (hpaCPU || !differs(cur.Requests.Cpu(), &recCPU)) && (hpaMem || !differs(cur.Requests.Memory(), &recMem)) {
		return false
	}
	rememberApplied(st, ap)
	ap.AppliedResources = apply
	rc.RecordAutoPilotAction(ActionResourcesScaled, fmt.Sprintf("usage-based right-sizing (p90 of %d samples at %d%% target): %s -> %s",
		len(st.samples), target, describeResources(&cur), describeResources(apply)))
	rc.Requeue(time.Second)
	return true
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
