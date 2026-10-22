package autopilot

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;patch

const (
	// storageCheckInterval is how often volumes are measured and reported.
	storageCheckInterval = time.Minute
	// duInterval throttles du, which walks the whole volume.
	duInterval = 5 * time.Minute
	// maxMeasureBackoff caps the retry interval of a claim whose measurement keeps failing.
	maxMeasureBackoff = time.Hour
	// dfTimeout bounds the df exec.
	dfTimeout = 30 * time.Second
	// duRemoteTimeoutSeconds bounds du inside the container (timeout -s KILL), so an exec
	// stream that is dropped cannot leave a volume walk running. duTimeout, the client-side
	// limit, is slightly longer.
	duRemoteTimeoutSeconds = 110
	duTimeout              = 2 * time.Minute
	// measureWait is how long a reconcile waits for measurements it started; slower ones
	// (du on a large volume) finish in the background and are reported by a later reconcile.
	measureWaitDefault = 3 * time.Second
	// measureRequeue picks up background measurements sooner than the regular resync.
	measureRequeue = 30 * time.Second
	// maxConcurrentMeasurements bounds the exec sessions the operator runs at once.
	maxConcurrentMeasurements = 4

	// expansionSettle gives the CSI driver/kubelet time to report the new size.
	expansionSettle = 5 * time.Minute
	// expansionStallAfter reports a resize that has been pending this long as stalled.
	expansionStallAfter = 30 * time.Minute
	// fsResizeRestartAfter restarts Forgejo when its data volume has waited this long for a
	// file system resize that the kubelet only performs when the volume is mounted.
	fsResizeRestartAfter = 10 * time.Minute

	cnpgClusterLabel = "cnpg.io/cluster"
	cnpgPVCRoleLabel = "cnpg.io/pvcRole"
)

type claimRole int

const (
	roleData claimRole = iota
	roleDB
	roleWAL
)

// ReconcileStorage is the storage pipeline step: it measures the data and CNPG volumes,
// reports status.storage and StorageHealthy, and grows volumes above the threshold.
//
// It needs no other step. A full volume is often why an earlier step stops: PostgreSQL
// cannot start on a full CNPG volume, so the database step waits, and with it the workload
// and post-rollout steps. Growing that volume must not depend on them. The step therefore
// reads only the instance, its claims, pods and Deployment, and copes with rc.DB being nil,
// the workload not existing yet, maintenance and a missing CNPG Cluster.
func ReconcileStorage(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	st := stateFor(rc.Instance)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !storageCheckDue(rc.Instance, st) {
		return forgejo.StepResult{}, nil
	}
	dep, err := serverDeployment(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	return forgejo.StepResult{}, reconcileStorage(ctx, rc, st, dep)
}

// storageCheckDue reports whether volumes should be measured and reported again: once per
// storageCheckInterval, and as soon as a background measurement has finished.
func storageCheckDue(f *forgejov1.Forgejo, st *instanceState) bool {
	return f.Status.Storage == nil || time.Since(st.lastStorageCheck) >= storageCheckInterval || st.measure.updatedSince(st.lastStorageCheck)
}

// reconcileStorage measures the data and CNPG PVCs, reports status.storage, the
// VolumeUsedRatio metric and StorageHealthy, and expands volumes above the threshold when
// auto-expansion is enabled. Measuring and reporting continue with auto-expansion turned
// off, so the ForgejoVolumeAlmostFull alert and StorageHealthy keep working. dep is the
// Forgejo Deployment, nil when it does not exist. Must be called with st.mu held.
func reconcileStorage(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, dep *appsv1.Deployment) error {
	f := rc.Instance
	m := st.measure
	if !storageCheckDue(f, st) {
		return nil
	}

	claims, err := instanceClaims(ctx, rc)
	if err != nil {
		return err
	}
	pods, err := mountingPods(ctx, rc, claims)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(claims))
	var started []<-chan struct{}
	for _, c := range claims {
		names = append(names, c.pvc.Name)
		if ch := m.start(logger(ctx), pods, c.pvc); ch != nil {
			started = append(started, ch)
		}
	}
	m.prune(names)
	waitFor(ctx, started, measureWait)
	if m.anyRunning() {
		rc.Requeue(measureRequeue)
	}
	st.lastStorageCheck = time.Now()

	prev := map[string]forgejov1.VolumeUsage{}
	if f.Status.Storage != nil {
		for _, v := range f.Status.Storage.Volumes {
			prev[v.ClaimName] = v
		}
	}
	ae := f.Spec.AutoPilot.StorageAutoExpand
	autoExpand := forgejo.StorageAutoExpandEnabled(f)
	threshold := int64(ae.ThresholdPercent)
	if threshold <= 0 {
		threshold = 80
	}
	maxSize := forgejo.QuantityOr(ae.MaxSize, "200Gi")
	scCache := map[string]bool{}

	var volumes []forgejov1.VolumeUsage
	var problems []string
	problemReason, expanding := "", false
	addProblem := func(reason, msg string) {
		problems = append(problems, msg)
		if problemRank(reason) < problemRank(problemReason) {
			problemReason = reason
		}
	}
	expandedRoles := map[claimRole]bool{}
	for _, c := range claims {
		pvc := c.pvc
		req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		vu := forgejov1.VolumeUsage{ClaimName: pvc.Name, RequestedSize: req.String(), LastExpansionTime: prev[pvc.Name].LastExpansionTime}
		vu.ExpansionSupported = expansionSupported(ctx, rc, pvc, scCache)

		pending := resizePending(pvc)
		if pending {
			since := resizePendingSince(st, pvc, prev[pvc.Name], req)
			if time.Since(since) >= expansionStallAfter {
				msg := fmt.Sprintf("resizing %s to %s has been pending since %s%s",
					pvc.Name, req.String(), since.UTC().Format(time.RFC3339), resizeDetail(pvc))
				addProblem(ReasonExpansionStalled, msg)
				warnEvery(rc, st, "storage-stalled/"+pvc.Name, time.Hour, ReasonExpansionStalled, "%s", msg)
			}
			if c.role == roleData && fsResizeOnly(pvc) && time.Since(since) >= fsResizeRestartAfter {
				if err := restartForFilesystemResize(ctx, rc, st, dep, pvc, since); err != nil {
					return err
				}
			}
		} else {
			delete(st.resizeSeen, pvc.Name)
		}

		res, ok := m.result(pvc.Name)
		if !ok {
			if p, found := prev[pvc.Name]; found && p.CapacityBytes > 0 {
				res, ok = measurement{used: p.UsedBytes, capacity: p.CapacityBytes}, true
			}
		}
		used, capacity := res.used, res.capacity
		if !ok || capacity <= 0 {
			volumes = append(volumes, vu)
			continue
		}
		vu.UsedBytes, vu.CapacityBytes = used, capacity
		vu.UsedPercent = int32(used * 100 / capacity)
		forgejo.VolumeUsedRatio.WithLabelValues(f.Namespace, f.Name, pvc.Name).Set(float64(used) / float64(capacity))

		if int64(vu.UsedPercent) >= threshold {
			switch {
			case !autoExpand:
				addProblem(ReasonAutoExpandDisabled, fmt.Sprintf("%s is %d%% full and storage auto-expansion is disabled", pvc.Name, vu.UsedPercent))
				warnEvery(rc, st, "storage-disabled/"+pvc.Name, time.Hour, ReasonAutoExpandDisabled,
					"PVC %s is %d%% full (threshold %d%%) and autoPilot.storageAutoExpand is disabled; grow it manually", pvc.Name, vu.UsedPercent, threshold)
			case !vu.ExpansionSupported:
				addProblem(ReasonExpansionUnsupported, fmt.Sprintf("%s is %d%% full and its StorageClass does not allow volume expansion", pvc.Name, vu.UsedPercent))
				warnEvery(rc, st, "storage-unsupported/"+pvc.Name, time.Hour, ReasonExpansionUnsupported,
					"PVC %s is %d%% full (threshold %d%%) but its StorageClass does not allow expansion; grow it manually", pvc.Name, vu.UsedPercent, threshold)
			case req.Cmp(maxSize) >= 0:
				addProblem(ReasonMaxSizeReached, fmt.Sprintf("%s is %d%% full at the autoPilot.storageAutoExpand.maxSize %s", pvc.Name, vu.UsedPercent, maxSize.String()))
				warnEvery(rc, st, "storage-max/"+pvc.Name, time.Hour, ReasonMaxSizeReached,
					"PVC %s is %d%% full and already at maxSize %s", pvc.Name, vu.UsedPercent, maxSize.String())
			case pending || (vu.LastExpansionTime != nil && time.Since(vu.LastExpansionTime.Time) < expansionSettle) || expandedRoles[c.role]:
				expanding = true
			case res.predatesResize(pvc):
				// The volume has grown since this measurement was taken, typically because
				// the pods that mount it cannot run (PostgreSQL on a full volume): wait for a
				// fresh measurement instead of growing it again on stale numbers.
				expanding = true
			default:
				newSize := grownSize(req, int64(ae.IncrementPercent), maxSize)
				if err := expandClaim(ctx, rc, c, newSize); err != nil {
					if c.role == roleData || !clusterMissing(err) {
						return err
					}
					msg := fmt.Sprintf("%s is %d%% full but CNPG Cluster %s, through which it is grown, does not exist",
						pvc.Name, vu.UsedPercent, forgejo.DatabaseClusterName(f))
					addProblem(ReasonDatabaseClusterMissing, msg)
					warnEvery(rc, st, "storage-nocluster/"+pvc.Name, time.Hour, ReasonDatabaseClusterMissing, "%s", msg)
					break
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
		msg := "no volume usage could be measured yet"
		if m.isForbidden() {
			msg += "; the operator may not exec into the pods that mount the claims (grant pods/exec in this namespace)"
		}
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionUnknown, "MeasurementUnavailable", msg)
	default:
		rc.SetCondition(forgejov1.ConditionStorageHealthy, metav1.ConditionTrue, "WithinThreshold",
			fmt.Sprintf("all volumes are below %d%% used", threshold))
	}
	return nil
}

// problemRank orders StorageHealthy=False reasons; the most urgent one names the condition.
func problemRank(reason string) int {
	switch reason {
	case ReasonExpansionStalled:
		return 0
	case ReasonDatabaseClusterMissing:
		return 1
	case ReasonExpansionUnsupported:
		return 2
	case ReasonMaxSizeReached:
		return 3
	case ReasonAutoExpandDisabled:
		return 4
	}
	return 99
}

// clusterMissing reports whether growing a CNPG volume failed because its Cluster (or the
// CNPG API) is gone, e.g. a retained volume whose Cluster was deleted.
func clusterMissing(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// mountingPods lists the pods through which the claims can be measured: the Forgejo server
// pods and, for CNPG volumes, the Cluster's pods. Both are selected by label, so the server
// pods come from the operator's filtered cache and only the Cluster's pods are read live.
// Backup and restore Job pods are left out: while a restore has scaled Forgejo to zero, the
// data volume keeps its last measurement.
func mountingPods(ctx context.Context, rc *forgejo.ReconcileContext, claims []instanceClaim) ([]corev1.Pod, error) {
	f := rc.Instance
	list := &corev1.PodList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(forgejo.SelectorLabels(f))); err != nil {
		return nil, err
	}
	pods := list.Items
	if !slices.ContainsFunc(claims, func(c instanceClaim) bool { return c.role != roleData }) {
		return pods, nil
	}
	db := &corev1.PodList{}
	if err := rc.Client.List(ctx, db, client.InNamespace(f.Namespace),
		client.MatchingLabels{cnpgClusterLabel: forgejo.DatabaseClusterName(f)}); err != nil {
		return nil, err
	}
	return append(pods, db.Items...), nil
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

// --- measurement ---
//
// Volumes are measured with an exec into a running container that mounts the claim, in the
// background so that a slow volume never holds the reconcile worker:
//
//   - df -Pk <mountPath> gives the file system's size and usage. When the size is at most
//     125% of the claim's request, the file system is the volume itself (block volumes,
//     most CSI drivers) and df is the measurement.
//   - Otherwise the claim is a directory on a larger file system (local-path, hostPath, NFS
//     exports) and du -skx walks it, compared with the requested size. du runs at most every
//     duInterval per claim, niced and killed inside the container after 110s.
//
// Failed attempts are recorded as well and back off exponentially (up to an hour), so a
// volume that cannot be measured is not retried on every reconcile.

// measurement is one claim's usage.
type measurement struct {
	used, capacity int64
	// claimCapacity is the PVC's status.capacity when the measurement started; zero when
	// unknown (a measurement carried over in status.storage).
	claimCapacity int64
}

// predatesResize reports whether the volume has grown since m was taken, so that m says
// nothing about how full it is now. For a measurement taken by this process the PVC's
// capacity is compared exactly; for one carried over in status.storage (after an operator
// restart) the measured size must be clearly, more than 10%, below the PVC's capacity. The
// smallest increment is 10% and file system overhead takes a few percent.
func (m measurement) predatesResize(pvc *corev1.PersistentVolumeClaim) bool {
	current := pvc.Status.Capacity.Storage().Value()
	if current <= 0 {
		return false
	}
	if m.claimCapacity > 0 {
		return current > m.claimCapacity
	}
	return m.capacity*10 < current*9
}

// claimMeasurement is what the operator knows about one claim's usage.
type claimMeasurement struct {
	measurement           // last successful measurement
	at          time.Time // when it was taken; zero when there is none

	attempt  time.Time // start of the last measurement job
	failures int       // consecutive jobs that produced no usable measurement

	duAttempt  time.Time
	duFailures int

	running bool
}

type measureState struct {
	mu        sync.Mutex
	claims    map[string]*claimMeasurement
	updated   time.Time // when the last job finished
	forbidden bool      // the last failed exec was refused by RBAC
	logged    map[string]bool
}

func newMeasureState() *measureState {
	return &measureState{claims: map[string]*claimMeasurement{}, logged: map[string]bool{}}
}

// measureWait is measureWaitDefault; tests shorten it.
var measureWait = measureWaitDefault

// measureSem bounds the exec sessions running at once across all instances.
var measureSem = make(chan struct{}, maxConcurrentMeasurements)

// backoff doubles base for every consecutive failure, up to maxMeasureBackoff.
func backoff(base time.Duration, failures int) time.Duration {
	d := base
	for i := 0; i < failures && d < maxMeasureBackoff; i++ {
		d *= 2
	}
	return min(d, maxMeasureBackoff)
}

func (m *measureState) claim(name string) *claimMeasurement {
	c, ok := m.claims[name]
	if !ok {
		c = &claimMeasurement{}
		m.claims[name] = c
	}
	return c
}

// start launches a measurement of the claim in the background when one is due and none is
// running. It returns a channel that is closed when the measurement finishes, or nil.
func (m *measureState) start(log logr.Logger, pods []corev1.Pod, pvc *corev1.PersistentVolumeClaim) <-chan struct{} {
	requested := pvc.Spec.Resources.Requests.Storage().Value()
	claimCapacity := pvc.Status.Capacity.Storage().Value()
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.claim(pvc.Name)
	if c.running || time.Since(c.attempt) < backoff(storageCheckInterval, c.failures) {
		return nil
	}
	pod, container, path := claimMount(pods, pvc.Name)
	if pod == "" {
		return nil
	}
	c.running, c.attempt = true, time.Now()
	done := make(chan struct{})
	go m.run(log, pvc.Name, pvc.Namespace, pod, container, path, requested, claimCapacity, done)
	return done
}

func (m *measureState) run(log logr.Logger, claim, namespace, pod, container, path string, requested, claimCapacity int64, done chan struct{}) {
	defer close(done)
	measureSem <- struct{}{}
	defer func() { <-measureSem }()

	used, size, dfErr := dfUsage(namespace, pod, container, path)
	m.mu.Lock()
	c := m.claim(claim)
	finish := func(ok bool, err error) {
		if ok {
			c.failures = 0
			m.forbidden = false
		} else {
			c.failures++
			if err != nil {
				m.forbidden = execForbidden(err)
				m.logFailure(log, claim, pod, err, backoff(storageCheckInterval, c.failures))
			}
		}
		c.running = false
		m.updated = time.Now()
		m.mu.Unlock()
	}
	if dfErr == nil && size > 0 && (requested <= 0 || size <= requested*5/4) {
		c.measurement, c.at = measurement{used: used, capacity: size, claimCapacity: claimCapacity}, time.Now()
		finish(true, nil)
		return
	}
	if execForbidden(dfErr) || requested <= 0 {
		finish(false, dfErr)
		return
	}
	if time.Since(c.duAttempt) < backoff(duInterval, c.duFailures) {
		// The file system is shared (or df failed): the last du result still stands.
		finish(dfErr == nil, dfErr)
		return
	}
	c.duAttempt = time.Now()
	m.mu.Unlock()

	duUsed, duErr := duUsage(namespace, pod, container, path)

	m.mu.Lock()
	c = m.claim(claim)
	if duErr != nil {
		c.duFailures++
		finish(false, duErr)
		return
	}
	c.duFailures = 0
	c.measurement, c.at = measurement{used: duUsed, capacity: requested, claimCapacity: claimCapacity}, time.Now()
	finish(true, nil)
}

// logFailure logs a failed measurement at info level the first time per claim and kind of
// error, and at debug level afterwards. Must be called with m.mu held.
func (m *measureState) logFailure(log logr.Logger, claim, pod string, err error, retry time.Duration) {
	key := claim + "/other"
	if execForbidden(err) {
		key = claim + "/forbidden"
	}
	if m.logged[key] {
		log.V(1).Info("cannot measure volume usage", "claim", claim, "pod", pod, "error", err.Error(), "retryIn", retry.String())
		return
	}
	m.logged[key] = true
	if execForbidden(err) {
		log.Info("cannot measure volume usage: the operator may not exec into the pod; grant pods/exec in this namespace to enable it",
			"claim", claim, "pod", pod, "error", err.Error())
		return
	}
	log.Info("cannot measure volume usage", "claim", claim, "pod", pod, "error", err.Error(), "retryIn", retry.String())
}

// result returns the last successful measurement of the claim.
func (m *measureState) result(claim string) (measurement, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, found := m.claims[claim]
	if !found || c.at.IsZero() {
		return measurement{}, false
	}
	return c.measurement, true
}

func (m *measureState) anyRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.claims {
		if c.running {
			return true
		}
	}
	return false
}

func (m *measureState) updatedSince(t time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updated.After(t)
}

func (m *measureState) isForbidden() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forbidden
}

// prune forgets claims that no longer belong to the instance.
func (m *measureState) prune(keep []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, c := range m.claims {
		found := slices.Contains(keep, name)
		if !found && !c.running {
			delete(m.claims, name)
		}
	}
}

// waitFor waits until every channel is closed, d has passed or ctx is done.
func waitFor(ctx context.Context, chans []<-chan struct{}, d time.Duration) {
	if len(chans) == 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for _, ch := range chans {
		select {
		case <-ch:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// dfUsage returns the used bytes and size of the file system mounted at path.
func dfUsage(namespace, pod, container, path string) (used, size int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), dfTimeout)
	defer cancel()
	out, err := execFn(ctx, namespace, pod, container, []string{"df", "-Pk", path})
	if err != nil {
		return 0, 0, err
	}
	return parseDF(out)
}

// parseDF reads POSIX df -Pk output: Filesystem, 1024-blocks, Used, Available, Capacity,
// Mounted on. Fields are counted from the end, since a file system name may contain spaces.
func parseDF(out string) (used, size int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("unexpected df output %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 6 {
		return 0, 0, fmt.Errorf("unexpected df output %q", out)
	}
	blocks, err1 := strconv.ParseInt(f[len(f)-5], 10, 64)
	usedK, err2 := strconv.ParseInt(f[len(f)-4], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unexpected df output %q", out)
	}
	return usedK * 1024, blocks * 1024, nil
}

// duScript runs du niced and, where timeout(1) exists (busybox in the Forgejo image,
// coreutils in the CNPG image), kills it inside the container after duRemoteTimeoutSeconds.
var duScript = `if command -v timeout >/dev/null 2>&1; then exec timeout -s KILL ` + strconv.Itoa(duRemoteTimeoutSeconds) +
	` nice -n 19 du -skx "$1"; else exec nice -n 19 du -skx "$1"; fi`

// duUsage returns the bytes used below path on its file system.
func duUsage(namespace, pod, container, path string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duTimeout)
	defer cancel()
	out, err := execFn(ctx, namespace, pod, container, []string{"sh", "-c", duScript, "sh", path})
	fields := strings.Fields(out)
	if len(fields) > 0 {
		// du exits non-zero for unreadable entries but still prints the total.
		if kib, perr := strconv.ParseInt(fields[0], 10, 64); perr == nil {
			return kib * 1024, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("unexpected du output %q", out)
	}
	return 0, err
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

func expansionSupported(ctx context.Context, rc *forgejo.ReconcileContext, pvc *corev1.PersistentVolumeClaim, cache map[string]bool) bool {
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
	sc := &storagev1.StorageClass{}
	err := rc.Client.Get(ctx, client.ObjectKey{Name: name}, sc)
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

// fsResizeOnly reports whether the controller part of a resize is done and only the
// kubelet's file system resize is left, which some drivers only do while mounting.
func fsResizeOnly(pvc *corev1.PersistentVolumeClaim) bool {
	fs := false
	for _, c := range pvc.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case corev1.PersistentVolumeClaimResizing:
			return false
		case corev1.PersistentVolumeClaimFileSystemResizePending:
			fs = true
		}
	}
	return fs
}

// resizePendingSince estimates when the claim's pending resize started: the operator's own
// expansion request, the PVC's resize conditions, or when the operator first saw it.
func resizePendingSince(st *instanceState, pvc *corev1.PersistentVolumeClaim, prev forgejov1.VolumeUsage, req resource.Quantity) time.Time {
	since, ok := st.resizeSeen[pvc.Name]
	if !ok {
		since = time.Now()
		st.resizeSeen[pvc.Name] = since
	}
	for _, c := range pvc.Status.Conditions {
		if (c.Type == corev1.PersistentVolumeClaimResizing || c.Type == corev1.PersistentVolumeClaimFileSystemResizePending) &&
			c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() && c.LastTransitionTime.Before(&metav1.Time{Time: since}) {
			since = c.LastTransitionTime.Time
		}
	}
	if prev.LastExpansionTime != nil && prev.RequestedSize == req.String() && prev.LastExpansionTime.Time.Before(since) {
		since = prev.LastExpansionTime.Time
	}
	return since
}

// resizeDetail describes why a resize is still pending, from the PVC's conditions and
// allocated resource status.
func resizeDetail(pvc *corev1.PersistentVolumeClaim) string {
	var parts []string
	for _, c := range pvc.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		s := string(c.Type)
		if c.Message != "" {
			s += ": " + c.Message
		}
		parts = append(parts, s)
	}
	if s, ok := pvc.Status.AllocatedResourceStatuses[corev1.ResourceStorage]; ok {
		parts = append(parts, "allocatedResourceStatus "+string(s))
	}
	if len(parts) == 0 {
		parts = append(parts, "no resizer has acted on it; check that the CSI driver's external-resizer runs and the PVC's events")
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// restartForFilesystemResize restarts Forgejo once per pending resize of its data volume
// when only the file system resize is left, so the kubelet resizes it while mounting.
//
// The storage step needs no other step, so the maintenance or upgrade step may have failed
// or not run yet in this pass; the restore and upgrade recorded in status count as well.
// A Deployment scaled to zero mounts nothing; its next pod resizes the file system anyway.
func restartForFilesystemResize(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, dep *appsv1.Deployment,
	pvc *corev1.PersistentVolumeClaim, since time.Time) error {
	f := rc.Instance
	restoring := f.Status.Backup != nil && f.Status.Backup.ActiveRestore != ""
	if dep == nil || ptr.Deref(dep.Spec.Replicas, 1) == 0 || rc.Maintenance || restoring || replacementsHeld(rc) ||
		forgejo.UpgradeInFlight(f) || inCooldown(f, ActionPodRestarted, restartCooldown) {
		return nil
	}
	if t, ok := st.resizeRestarted[pvc.Name]; ok && t.After(since) {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[forgejo.AnnotationRestartedAt] = time.Now().UTC().Format(time.RFC3339)
	if err := rc.Client.Patch(ctx, dep, patch); err != nil {
		return err
	}
	st.resizeRestarted[pvc.Name] = time.Now()
	rc.RecordAutoPilotAction(ActionPodRestarted, fmt.Sprintf(
		"PVC %s has waited for a file system resize since %s; restarting Deployment %s so the kubelet resizes it while mounting",
		pvc.Name, since.UTC().Format(time.RFC3339), dep.Name))
	return nil
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
