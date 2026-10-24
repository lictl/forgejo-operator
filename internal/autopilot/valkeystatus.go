package autopilot

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ValkeyReady reasons.
const (
	ReasonValkeyAvailable       = "Available"
	ReasonValkeyDegraded        = "Degraded"
	ReasonValkeyNoMaster        = "NoMaster"
	ReasonValkeyNotReady        = "NotReady"
	ReasonValkeyUnreachable     = "UnreachableFromOperator"
	ReasonValkeyNotProvisioned  = "NotProvisioned"
	valkeyStatusRequeueHealthy  = 30 * time.Second
	valkeyStatusRequeueDegraded = 10 * time.Second
)

// The Sentinels are asked at most every sentinelQueryInterval while the Valkey pods stay the
// same (names, UIDs, IPs, readiness) and the password does too; reconciles in between (an
// owned Deployment's status during a rollout, another step's requeue) reuse the answers.
// When none answered although pods are Ready (UnreachableFromOperator, e.g. a NetworkPolicy
// that drops the operator's packets, which is only reported), they are asked again only after
// sentinelRetryInterval, so that dial timeouts do not keep the controller's worker busy.
const (
	sentinelQueryInterval = 10 * time.Second
	sentinelRetryInterval = time.Minute
)

// sentinelAddr is how the operator reaches a pod's Sentinel; tests replace it.
var sentinelAddr = func(p *corev1.Pod) string {
	return net.JoinHostPort(p.Status.PodIP, strconv.Itoa(sentinelPort))
}

// sentinelView is what one Sentinel reports.
type sentinelView struct {
	pod      string
	master   string // host name of the primary ("" when the Sentinel names none)
	replicas []map[string]string
	err      error
}

// ReportValkey reports the Valkey replication group of a highAvailability instance as its
// Sentinels see it: status.highAvailability.valkey (primary, roles, readiness), the
// ValkeyReady condition, the forgejo_operator_valkey_* metrics, and a ValkeyFailedOver
// event when the primary moved. It is a step of its own without prerequisites, so that the
// report stays current while the database (which the high-availability step waits for) is
// down. The operator connects to each pod's Sentinel (port 26379); where it cannot, roles
// are unknown and ValkeyReady follows pod readiness (Unknown/UnreachableFromOperator). The
// Sentinels' answers are reused between reconciles (sentinelQueryInterval,
// sentinelRetryInterval) while the pods stay the same; pod readiness is always current.
func ReportValkey(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !f.Spec.HighAvailability.Enabled {
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionValkeyReady)
		f.Status.HighAvailability = nil
		forgejo.ValkeyMasterAvailable.DeleteLabelValues(f.Namespace, f.Name)
		forgejo.ValkeyReadyMembers.DeleteLabelValues(f.Namespace, f.Name)
		// Valkey created when HA is turned on again is a new replication group.
		st := stateFor(f)
		st.mu.Lock()
		st.valkeyMaster, st.valkeyGroup, st.sentinels = "", nil, sentinelAnswers{}
		st.mu.Unlock()
		return forgejo.StepResult{}, nil
	}
	if f.Status.HighAvailability == nil {
		f.Status.HighAvailability = &forgejov1.HighAvailabilityStatus{}
	}
	ha := f.Status.HighAvailability
	if ha.Valkey == nil {
		ha.Valkey = &forgejov1.ValkeyStatus{}
	}
	vs := ha.Valkey
	now := metav1.Now()

	sts := &appsv1.StatefulSet{}
	err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ValkeyName(f)}, sts)
	if client.IgnoreNotFound(err) != nil {
		return forgejo.StepResult{}, err
	}
	if err != nil || !metav1.IsControlledBy(sts, f) {
		vs.LastCheckTime = &now
		vs.Master, vs.Members, vs.Sentinels = "", nil, 0
		vs.Message = "StatefulSet " + forgejo.ValkeyName(f) + " does not exist yet"
		if err == nil {
			vs.Message = "StatefulSet " + forgejo.ValkeyName(f) + " is not controlled by this Forgejo"
		}
		setValkeyReady(rc, metav1.ConditionFalse, ReasonValkeyNotProvisioned, vs.Message, 0)
		return forgejo.StepResult{RequeueAfter: valkeyStatusRequeueDegraded}, nil
	}
	list := &corev1.PodList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(valkeySelector(f))); err != nil {
		return forgejo.StepResult{}, err
	}
	// Only the StatefulSet's own pods are asked (and get the password): any pod can carry the
	// labels.
	pods := list.Items[:0]
	for _, p := range list.Items {
		if metav1.IsControlledBy(&p, sts) && p.DeletionTimestamp == nil &&
			p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded {
			pods = append(pods, p)
		}
	}
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })

	password := ""
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec); err == nil {
		password = string(sec.Data[valkeyPasswordKey])
	} else if client.IgnoreNotFound(err) != nil {
		return forgejo.StepResult{}, err
	}
	st := stateFor(f)
	key := sentinelQueryKey(pods, password)
	st.mu.Lock()
	answers := st.sentinels
	st.mu.Unlock()
	if !answers.fresh(key, now.Time) {
		answers = sentinelAnswers{key: key, at: now.Time, views: querySentinels(ctx, pods, password)}
	}
	r := summarizeValkey(pods, answers.views)
	answers.unreachable = r.reason == ReasonValkeyUnreachable
	at := metav1.NewTime(answers.at)
	vs.LastCheckTime = &at

	st.mu.Lock()
	st.sentinels = answers
	if st.valkeyMaster == "" {
		// After an operator restart: the primary recorded in status.
		st.valkeyMaster = vs.Master
	}
	vs.Master, vs.Members, vs.Sentinels, vs.Message = r.master, r.members, r.sentinels, r.message
	if r.master != "" {
		if failedOver(st.valkeyMaster, st.valkeyGroup, r.master, pods) {
			vs.LastFailoverTime = &now
			forgejo.ValkeyFailovers.WithLabelValues(f.Namespace, f.Name).Inc()
			rc.Event(corev1.EventTypeWarning, "ValkeyFailedOver", "HighAvailability",
				"the Valkey primary moved from %s to %s", st.valkeyMaster, r.master)
		}
		st.valkeyMaster, st.valkeyGroup = r.master, podUIDs(pods)
	}
	st.mu.Unlock()

	setValkeyReady(rc, r.status, r.reason, r.message, r.ready)
	switch {
	case r.status == metav1.ConditionTrue && r.reason == ReasonValkeyAvailable:
		return forgejo.StepResult{RequeueAfter: valkeyStatusRequeueHealthy}, nil
	case answers.unreachable:
		return forgejo.StepResult{RequeueAfter: max(answers.next().Sub(now.Time), time.Second)}, nil
	}
	return forgejo.StepResult{RequeueAfter: valkeyStatusRequeueDegraded}, nil
}

// sentinelAnswers are the Sentinels' answers of the last query, and what they were asked for.
type sentinelAnswers struct {
	key   string // sentinelQueryKey of the query
	at    time.Time
	views []sentinelView
	// unreachable: no Sentinel answered although pods were Ready.
	unreachable bool
}

// next is when the Sentinels are asked again while nothing changes.
func (a sentinelAnswers) next() time.Time {
	if a.unreachable {
		return a.at.Add(sentinelRetryInterval)
	}
	return a.at.Add(sentinelQueryInterval)
}

// fresh reports whether the answers can stand in for a query with key at now.
func (a sentinelAnswers) fresh(key string, now time.Time) bool {
	return a.key != "" && a.key == key && now.Before(a.next()) && !now.Before(a.at)
}

// sentinelQueryKey identifies what the Sentinels would be asked: the pods (name, UID, IP,
// readiness) and a digest of the password.
func sentinelQueryKey(pods []corev1.Pod, password string) string {
	var b strings.Builder
	b.WriteString(passwordDigest(password))
	for i := range pods {
		p := &pods[i]
		fmt.Fprintf(&b, "|%s/%s/%s/%t", p.Name, p.UID, p.Status.PodIP, podIsReady(p))
	}
	return b.String()
}

// podUIDs maps the pods' names to their UIDs.
func podUIDs(pods []corev1.Pod) map[string]types.UID {
	m := make(map[string]types.UID, len(pods))
	for i := range pods {
		m[pods[i].Name] = pods[i].UID
	}
	return m
}

// failedOver reports whether the primary moved from prev to master within the replication
// group in which prev was seen; group maps the names of that group's pods to their UIDs (nil
// after an operator restart, when only status names the previous primary). The Sentinels only
// promote a replica, a pod of the group. A primary whose pod is not one of them heads a new
// group whose pods all replaced the old ones (highAvailability turned off and on, the
// StatefulSet recreated, Valkey emptied after a restore or a password change): its pod 0
// starts as the primary, which is no failover.
func failedOver(prev string, group map[string]types.UID, master string, pods []corev1.Pod) bool {
	if prev == "" || master == prev {
		return false
	}
	if group == nil {
		return true
	}
	uid, member := group[master]
	if !member {
		return false
	}
	for i := range pods {
		if pods[i].Name == master {
			return pods[i].UID == uid
		}
	}
	return true
}

func setValkeyReady(rc *forgejo.ReconcileContext, status metav1.ConditionStatus, reason, msg string, ready int) {
	f := rc.Instance
	rc.SetCondition(forgejov1.ConditionValkeyReady, status, reason, msg)
	v := 0.0
	if status == metav1.ConditionTrue || (status == metav1.ConditionUnknown && ready > 0) {
		v = 1
	}
	forgejo.ValkeyMasterAvailable.WithLabelValues(f.Namespace, f.Name).Set(v)
	forgejo.ValkeyReadyMembers.WithLabelValues(f.Namespace, f.Name).Set(float64(ready))
}

// querySentinels asks every pod's Sentinel, in parallel, for the primary and its replicas.
func querySentinels(ctx context.Context, pods []corev1.Pod, password string) []sentinelView {
	views := make([]sentinelView, len(pods))
	var wg sync.WaitGroup
	for i := range pods {
		views[i].pod = pods[i].Name
		if pods[i].Status.PodIP == "" {
			views[i].err = fmt.Errorf("pod %s has no IP", pods[i].Name)
			continue
		}
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			views[i].master, views[i].replicas, views[i].err = askSentinel(ctx, addr, password)
		}(i, sentinelAddr(&pods[i]))
	}
	wg.Wait()
	return views
}

// askSentinel returns the host name of the primary a Sentinel names and the replicas it
// knows.
func askSentinel(ctx context.Context, addr, password string) (string, []map[string]string, error) {
	c, err := dialSentinel(ctx, addr, password)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = c.Close() }()
	v, err := c.do("SENTINEL", "GET-MASTER-ADDR-BY-NAME", valkeyMasterName)
	if err != nil {
		return "", nil, err
	}
	addrs := respStrings(v)
	if len(addrs) == 0 || addrs[0] == "" {
		return "", nil, nil
	}
	v, err = c.do("SENTINEL", "REPLICAS", valkeyMasterName)
	if err != nil {
		return addrs[0], nil, err
	}
	return addrs[0], respMaps(v), nil
}

// valkeyReport is the summary of the pods and the Sentinels' answers.
type valkeyReport struct {
	master    string
	members   []forgejov1.ValkeyMember
	sentinels int32
	ready     int
	status    metav1.ConditionStatus
	reason    string
	message   string
}

// podOfHost maps a Valkey host name ("<pod>.<service>.<namespace>.svc...", or an IP the
// Sentinels resolved) to the pod.
func podOfHost(host string, pods []corev1.Pod) string {
	for i := range pods {
		if host == pods[i].Status.PodIP || host == pods[i].Name || strings.HasPrefix(host, pods[i].Name+".") {
			return pods[i].Name
		}
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}

// replicaInSync reports whether a replica of SENTINEL REPLICAS replicates from the primary.
func replicaInSync(r map[string]string) bool {
	flags := strings.Split(r["flags"], ",")
	for _, bad := range []string{"s_down", "o_down", "disconnected"} {
		if slices.Contains(flags, bad) {
			return false
		}
	}
	return r["master-link-status"] == "ok"
}

func summarizeValkey(pods []corev1.Pod, views []sentinelView) valkeyReport {
	var r valkeyReport
	votes := map[string]int{}
	var errs []string
	var best *sentinelView
	for i := range views {
		v := &views[i]
		if v.err != nil && v.master == "" {
			errs = append(errs, fmt.Sprintf("%s: %v", v.pod, v.err))
			continue
		}
		r.sentinels++
		if v.master == "" {
			continue
		}
		m := podOfHost(v.master, pods)
		votes[m]++
		if best == nil || votes[m] > votes[podOfHost(best.master, pods)] {
			best = v
		}
	}
	if best != nil {
		r.master = podOfHost(best.master, pods)
	}
	inSync := map[string]bool{}
	if best != nil {
		for _, rep := range best.replicas {
			host := rep["ip"]
			if host == "" {
				host, _, _ = strings.Cut(rep["name"], ":")
			}
			inSync[podOfHost(host, pods)] = replicaInSync(rep)
		}
	}
	masterReady := false
	syncing := 0
	for i := range pods {
		p := &pods[i]
		m := forgejov1.ValkeyMember{Name: p.Name, Role: forgejov1.ValkeyRoleUnknown, Ready: podIsReady(p)}
		if m.Ready {
			r.ready++
		}
		switch ok, known := inSync[p.Name]; {
		case p.Name == r.master:
			m.Role = forgejov1.ValkeyRoleMaster
			masterReady = m.Ready
		case known:
			m.Role = forgejov1.ValkeyRoleReplica
			if ok {
				syncing++
			}
		}
		r.members = append(r.members, m)
	}

	disagree := len(votes) > 1
	switch {
	case r.sentinels == 0 && r.ready > 0:
		r.status, r.reason = metav1.ConditionUnknown, ReasonValkeyUnreachable
		r.message = fmt.Sprintf("%d/%d Valkey pods are Ready, but the operator cannot reach their Sentinels on port %d, so roles are unknown (%s)",
			r.ready, valkeyMembers, sentinelPort, strings.Join(errs, "; "))
	case r.sentinels == 0:
		r.status, r.reason = metav1.ConditionFalse, ReasonValkeyNotReady
		r.message = fmt.Sprintf("no Valkey pod is Ready and no Sentinel answered (%d pods)", len(pods))
	case r.master == "":
		r.status, r.reason = metav1.ConditionFalse, ReasonValkeyNoMaster
		r.message = fmt.Sprintf("%d Sentinels answered, none names a primary", r.sentinels)
	case !masterReady:
		r.status, r.reason = metav1.ConditionFalse, ReasonValkeyNoMaster
		r.message = fmt.Sprintf("the Sentinels name %s as the primary, but it is not Ready; a failover follows after %dms",
			r.master, sentinelDownAfter)
	default:
		r.status, r.reason = metav1.ConditionTrue, ReasonValkeyAvailable
		r.message = fmt.Sprintf("primary %s, %d/%d replicas in sync, %d/%d Sentinels answered",
			r.master, syncing, valkeyMembers-1, r.sentinels, valkeyMembers)
		if syncing < valkeyMembers-1 || r.sentinels < valkeyMembers || r.ready < valkeyMembers {
			r.reason = ReasonValkeyDegraded
			r.message += fmt.Sprintf(", %d/%d pods Ready: a further failure would leave no replica to promote or no Sentinel majority",
				r.ready, valkeyMembers)
		}
	}
	if disagree {
		r.message += fmt.Sprintf(" (the Sentinels disagree on the primary: %v)", votes)
	}
	return r
}

func podIsReady(p *corev1.Pod) bool {
	c := podReadyCondition(p)
	return c != nil && c.Status == corev1.ConditionTrue
}
