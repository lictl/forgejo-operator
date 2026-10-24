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
// are unknown and ValkeyReady follows pod readiness (Unknown/UnreachableFromOperator).
func ReportValkey(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !f.Spec.HighAvailability.Enabled {
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionValkeyReady)
		f.Status.HighAvailability = nil
		forgejo.ValkeyMasterAvailable.DeleteLabelValues(f.Namespace, f.Name)
		forgejo.ValkeyReadyMembers.DeleteLabelValues(f.Namespace, f.Name)
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
	vs.LastCheckTime = &now

	sts := &appsv1.StatefulSet{}
	err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ValkeyName(f)}, sts)
	if client.IgnoreNotFound(err) != nil {
		return forgejo.StepResult{}, err
	}
	if err != nil || !metav1.IsControlledBy(sts, f) {
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
	views := querySentinels(ctx, pods, password)
	r := summarizeValkey(pods, views)

	st := stateFor(f)
	st.mu.Lock()
	if st.valkeyMaster == "" {
		// After an operator restart: the primary recorded in status.
		st.valkeyMaster = vs.Master
	}
	vs.Master, vs.Members, vs.Sentinels, vs.Message = r.master, r.members, r.sentinels, r.message
	if r.master != "" && st.valkeyMaster != "" && r.master != st.valkeyMaster {
		vs.LastFailoverTime = &now
		forgejo.ValkeyFailovers.WithLabelValues(f.Namespace, f.Name).Inc()
		rc.Event(corev1.EventTypeWarning, "ValkeyFailedOver", "HighAvailability",
			"the Valkey primary moved from %s to %s", st.valkeyMaster, r.master)
	}
	if r.master != "" {
		st.valkeyMaster = r.master
	}
	st.mu.Unlock()

	setValkeyReady(rc, r.status, r.reason, r.message, r.ready)
	if r.status == metav1.ConditionTrue && r.reason == ReasonValkeyAvailable {
		return forgejo.StepResult{RequeueAfter: valkeyStatusRequeueHealthy}, nil
	}
	return forgejo.StepResult{RequeueAfter: valkeyStatusRequeueDegraded}, nil
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
