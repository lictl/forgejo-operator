package autopilot

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// fakeSentinel answers AUTH, SENTINEL GET-MASTER-ADDR-BY-NAME and SENTINEL REPLICAS.
type fakeSentinel struct {
	mu       sync.Mutex
	conns    int
	password string
	master   string
	replicas [][]string
	ln       net.Listener
}

// newFakeSentinel requires testValkeyPassword.
func newFakeSentinel(t *testing.T, master string, replicas ...[]string) *fakeSentinel {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSentinel{password: testValkeyPassword, master: master, replicas: replicas, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakeSentinel) set(master string, replicas ...[]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.master, s.replicas = master, replicas
}

func (s *fakeSentinel) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func bulk(v string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(v), v) }

func (s *fakeSentinel) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *fakeSentinel) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	r := bufio.NewReader(conn)
	authed := s.password == ""
	for {
		v, err := readRESP(r)
		if err != nil {
			return
		}
		args := respStrings(v)
		s.mu.Lock()
		var out string
		switch {
		case len(args) == 2 && args[0] == "AUTH":
			if args[1] == s.password {
				authed, out = true, "+OK\r\n"
			} else {
				out = "-WRONGPASS invalid username-password pair\r\n"
			}
		case !authed:
			out = "-NOAUTH Authentication required.\r\n"
		case len(args) == 3 && args[1] == "GET-MASTER-ADDR-BY-NAME":
			if s.master == "" {
				out = "*-1\r\n"
			} else {
				out = "*2\r\n" + bulk(s.master) + bulk("6379")
			}
		case len(args) == 3 && args[1] == "REPLICAS":
			out = fmt.Sprintf("*%d\r\n", len(s.replicas))
			for _, kv := range s.replicas {
				out += fmt.Sprintf("*%d\r\n", len(kv))
				for _, x := range kv {
					out += bulk(x)
				}
			}
		default:
			out = "-ERR unknown command\r\n"
		}
		s.mu.Unlock()
		if _, err := conn.Write([]byte(out)); err != nil {
			return
		}
	}
}

// testValkeySTSUID is the UID of the Valkey StatefulSet of valkeyStatefulSet, which controls
// the pods of valkeyPod.
const testValkeySTSUID = "uid-valkey-sts"

// valkeyStatefulSet is StatefulSet "<name>-valkey", controlled by f.
func valkeyStatefulSet(f *forgejov1.Forgejo) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace, UID: testValkeySTSUID,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo", Name: f.Name,
			UID: f.UID, Controller: ptr.To(true)}}}}
}

func valkeyPod(f *forgejov1.Forgejo, i int, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", forgejo.ValkeyName(f), i), Namespace: f.Namespace, Labels: valkeySelector(f),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: forgejo.ValkeyName(f),
				UID: testValkeySTSUID, Controller: ptr.To(true)}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: fmt.Sprintf("10.0.0.%d", i+1),
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func uidPod(f *forgejov1.Forgejo, i int, uid string) *corev1.Pod {
	p := valkeyPod(f, i, true)
	p.UID = types.UID(uid)
	return p
}

func replicaEntry(host, link string, flags string) []string {
	return []string{"name", host + ":6379", "ip", host, "port", "6379", "flags", flags, "master-link-status", link}
}

func host(f *forgejov1.Forgejo, i int) string { return valkeyHosts(f)[i] }

// withSentinels routes the Sentinel of each pod to a fake server.
func withSentinels(t *testing.T, byIP map[string]*fakeSentinel) {
	t.Helper()
	old := sentinelAddr
	sentinelAddr = func(p *corev1.Pod) string {
		if s, ok := byIP[p.Status.PodIP]; ok {
			return s.ln.Addr().String()
		}
		return "127.0.0.1:1" // refused
	}
	t.Cleanup(func() { sentinelAddr = old })
}

// expireSentinelAnswers makes the next ReportValkey ask the Sentinels again.
func expireSentinelAnswers(f *forgejov1.Forgejo) {
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sentinels.at = time.Now().Add(-time.Hour)
}

// ageSentinelAnswers moves the last query d into the past.
func ageSentinelAnswers(f *forgejov1.Forgejo, d time.Duration) {
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sentinels.at = st.sentinels.at.Add(-d)
}

// countDials counts the Sentinel addresses the operator resolves, one per pod and query.
func countDials(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	inner := sentinelAddr
	sentinelAddr = func(p *corev1.Pod) string {
		mu.Lock()
		n++
		mu.Unlock()
		return inner(p)
	}
	t.Cleanup(func() { sentinelAddr = inner })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func TestReportValkeyRolesAndFailover(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	// A pod with the Valkey labels that the StatefulSet does not control: never asked, never
	// sent the password.
	intruder := valkeyPod(f, 3, true)
	intruder.Name, intruder.OwnerReferences, intruder.Status.PodIP = "intruder", nil, "10.0.0.9"
	env := newTestEnv(t, f, valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword),
		valkeyPod(f, 0, true), valkeyPod(f, 1, true), valkeyPod(f, 2, true), intruder)
	replicas := [][]string{replicaEntry(host(f, 1), "ok", "slave"), replicaEntry(host(f, 2), "ok", "slave")}
	sentinels := map[string]*fakeSentinel{}
	for i := range 3 {
		sentinels[fmt.Sprintf("10.0.0.%d", i+1)] = newFakeSentinel(t, host(f, 0), replicas...)
	}
	hostile := newFakeSentinel(t, host(f, 0), replicas...)
	sentinels[intruder.Status.PodIP] = hostile
	withSentinels(t, sentinels)

	res, err := ReportValkey(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	vs := f.Status.HighAvailability.Valkey
	if vs.Master != testValkeyPod0 || vs.Sentinels != 3 || len(vs.Members) != 3 ||
		vs.Members[0].Role != forgejov1.ValkeyRoleMaster || vs.Members[1].Role != forgejov1.ValkeyRoleReplica {
		t.Fatalf("status %+v", vs)
	}
	if c := condition(f, forgejov1.ConditionValkeyReady); c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonValkeyAvailable {
		t.Fatalf("ValkeyReady %+v", c)
	}
	if res.RequeueAfter != valkeyStatusRequeueHealthy {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	if evs := env.events(); hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("failover reported on the first look: %v", evs)
	}
	if n := hostile.connections(); n != 0 {
		t.Fatalf("the operator connected %d times to a pod its StatefulSet does not control", n)
	}

	// The Sentinels promote pod 2; pod 0 comes back as a replica.
	for _, s := range sentinels {
		s.set(host(f, 2), replicaEntry(host(f, 0), "ok", "slave"), replicaEntry(host(f, 1), "ok", "slave"))
	}
	expireSentinelAnswers(f)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if vs.Master != testValkeyPod2 || vs.LastFailoverTime == nil || vs.Members[2].Role != forgejov1.ValkeyRoleMaster {
		t.Fatalf("after failover %+v", vs)
	}
	if evs := env.events(); !hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("no ValkeyFailedOver event: %v", evs)
	}
	// Seen once, not again.
	expireSentinelAnswers(f)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if evs := env.events(); hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("failover reported twice: %v", evs)
	}
}

func TestSummarizeValkey(t *testing.T) {
	f := haInstance()
	pods := []corev1.Pod{*valkeyPod(f, 0, true), *valkeyPod(f, 1, true), *valkeyPod(f, 2, false)}
	view := func(pod, master string, reps ...[]string) sentinelView {
		m := make([]map[string]string, 0, len(reps))
		for _, kv := range reps {
			mm := map[string]string{}
			for i := 0; i+1 < len(kv); i += 2 {
				mm[kv[i]] = kv[i+1]
			}
			m = append(m, mm)
		}
		return sentinelView{pod: pod, master: master, replicas: m}
	}
	down := sentinelView{pod: testValkeyPod2, err: fmt.Errorf("refused")}
	for _, tt := range []struct {
		name   string
		pods   []corev1.Pod
		views  []sentinelView
		status metav1.ConditionStatus
		reason string
	}{
		{"one replica down", pods, []sentinelView{
			view(testValkeyPod0, host(f, 0), replicaEntry(host(f, 1), "ok", "slave"), replicaEntry(host(f, 2), "err", "slave,s_down")),
			view(testValkeyPod1, host(f, 0)), down}, metav1.ConditionTrue, ReasonValkeyDegraded},
		{"primary not ready", []corev1.Pod{*valkeyPod(f, 0, false), *valkeyPod(f, 1, true)},
			[]sentinelView{view(testValkeyPod1, host(f, 0))}, metav1.ConditionFalse, ReasonValkeyNoMaster},
		{"no primary named", pods, []sentinelView{view(testValkeyPod0, ""), view(testValkeyPod1, "")},
			metav1.ConditionFalse, ReasonValkeyNoMaster},
		{"sentinels unreachable, pods ready", pods, []sentinelView{
			{pod: testValkeyPod0, err: fmt.Errorf("i/o timeout")}, {pod: testValkeyPod1, err: fmt.Errorf("i/o timeout")}, down},
			metav1.ConditionUnknown, ReasonValkeyUnreachable},
		{"nothing ready", []corev1.Pod{*valkeyPod(f, 0, false)}, []sentinelView{{pod: testValkeyPod0, err: fmt.Errorf("refused")}},
			metav1.ConditionFalse, ReasonValkeyNotReady},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := summarizeValkey(tt.pods, tt.views)
			if r.status != tt.status || r.reason != tt.reason {
				t.Fatalf("got %s/%s (%s), want %s/%s", r.status, r.reason, r.message, tt.status, tt.reason)
			}
		})
	}
}

// A StatefulSet "<name>-valkey" that the instance does not control is not its Valkey: no pod
// of it is asked.
func TestReportValkeyIgnoresStatefulSetOfAnotherOwner(t *testing.T) {
	f := haInstance()
	sts := valkeyStatefulSet(f)
	sts.OwnerReferences[0].UID = "uid-other"
	s := newFakeSentinel(t, host(f, 0))
	withSentinels(t, map[string]*fakeSentinel{"10.0.0.1": s})
	env := newTestEnv(t, f, sts, secretsWithValkeyPassword(f, testValkeyPassword), valkeyPod(f, 0, true))
	if _, err := ReportValkey(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionValkeyReady); c == nil || c.Reason != ReasonValkeyNotProvisioned ||
		!strings.Contains(c.Message, "not controlled by this Forgejo") {
		t.Fatalf("ValkeyReady %+v", c)
	}
	if n := s.connections(); n != 0 {
		t.Fatalf("connected %d times", n)
	}
}

// A peer that is not a Sentinel (the Valkey image is the user's choice) cannot make the
// operator allocate much: declared lengths reserve nothing and are bounded, and so are nesting
// and the bytes read from a connection.
func TestReadRESPBounds(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"huge array", "*1048576\r\n", "bad array length"},
		{"huge bulk", "$1048576\r\n", "bad bulk length"},
		{"deep nesting", strings.Repeat("*1\r\n", 10) + ":1\r\n", "nested too deep"},
		{"array cut short", "*1024\r\n+a\r\n", "EOF"},
	} {
		if _, err := readRESP(bufio.NewReader(strings.NewReader(tt.in))); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
	// SENTINEL REPLICAS: an array of flat arrays.
	v, err := readRESP(bufio.NewReader(strings.NewReader("*1\r\n*2\r\n$1\r\na\r\n$1\r\nb\r\n")))
	if m := respMaps(v); err != nil || len(m) != 1 || m[0]["a"] != "b" {
		t.Fatalf("v = %v, err = %v", v, err)
	}
	if v, err := readRESP(bufio.NewReader(strings.NewReader("*0\r\n"))); err != nil || v == nil || len(v.([]any)) != 0 {
		t.Fatalf("empty array: %#v, %v", v, err)
	}

	// A peer that streams without end is cut off at maxRESPBytes, long before the deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = readRESP(bufio.NewReader(conn)) // the query
		chunk := []byte("+" + strings.Repeat("a", 64<<10))
		for range 32 { // 2 MiB without a line end
			if _, err := conn.Write(chunk[:len(chunk)-1]); err != nil {
				return
			}
		}
		time.Sleep(respTimeout)
	}()
	start := time.Now()
	if _, _, err := askSentinel(context.Background(), ln.Addr().String(), ""); err == nil {
		t.Fatal("no error from an endless reply")
	}
	if d := time.Since(start); d > respTimeout/2 {
		t.Fatalf("took %v: the reply was read until the deadline, not cut off at %d bytes", d, maxRESPBytes)
	}
}

func TestAskSentinelWrongPassword(t *testing.T) {
	s := newFakeSentinel(t, "h")
	if _, _, err := askSentinel(context.Background(), s.ln.Addr().String(), "wrong"); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("err = %v", err)
	}
	m, reps, err := askSentinel(context.Background(), s.ln.Addr().String(), testValkeyPassword)
	if err != nil || m != "h" || len(reps) != 0 {
		t.Fatalf("m=%q reps=%v err=%v", m, reps, err)
	}
}

// --- valkey-status asks the Sentinels at most every interval ---

func TestReportValkeyReusesSentinelAnswers(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	pods := []*corev1.Pod{valkeyPod(f, 0, true), valkeyPod(f, 1, true), valkeyPod(f, 2, true)}
	env := newTestEnv(t, f, valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword), pods[0], pods[1], pods[2])
	replicas := [][]string{replicaEntry(host(f, 1), "ok", "slave"), replicaEntry(host(f, 2), "ok", "slave")}
	sentinels := map[string]*fakeSentinel{}
	for i := range 3 {
		sentinels[fmt.Sprintf("10.0.0.%d", i+1)] = newFakeSentinel(t, host(f, 0), replicas...)
	}
	withSentinels(t, sentinels)
	conns := func() int {
		n := 0
		for _, s := range sentinels {
			n += s.connections()
		}
		return n
	}

	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	first := f.Status.HighAvailability.Valkey.LastCheckTime
	if n := conns(); n != 3 {
		t.Fatalf("%d connections on the first look, want 3", n)
	}
	// Reconciles in between (a rollout's status changes) reuse the answers.
	for range 5 {
		if _, err := ReportValkey(ctx, env.rc); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns(); n != 3 {
		t.Fatalf("%d connections after 5 more reconciles within %s, want 3", n, sentinelQueryInterval)
	}
	if got := f.Status.HighAvailability.Valkey.LastCheckTime; !got.Equal(first) {
		t.Fatalf("lastCheckTime moved to %v without a query (was %v)", got, first)
	}

	// A pod that changes (here: its readiness) is asked about at once, and so is everything
	// after the interval.
	pods[2].Status.Conditions[0].Status = corev1.ConditionFalse
	if err := env.rc.Client.Status().Update(ctx, pods[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := conns(); n != 6 {
		t.Fatalf("%d connections after a pod changed, want 6", n)
	}
	ageSentinelAnswers(f, sentinelQueryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := conns(); n != 9 {
		t.Fatalf("%d connections after %s, want 9", n, sentinelQueryInterval)
	}
}

// Sentinels the operator cannot reach while the pods are Ready are asked again only every
// sentinelRetryInterval, and the instance is not requeued every 10 s for it.
func TestReportValkeyBacksOffWhileSentinelsUnreachable(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	env := newTestEnv(t, f, valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword),
		valkeyPod(f, 0, true), valkeyPod(f, 1, true), valkeyPod(f, 2, true))
	withSentinels(t, map[string]*fakeSentinel{}) // every dial is refused
	dials := countDials(t)

	res, err := ReportValkey(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionValkeyReady); c == nil || c.Reason != ReasonValkeyUnreachable {
		t.Fatalf("ValkeyReady %+v", c)
	}
	if res.RequeueAfter <= valkeyStatusRequeueHealthy || res.RequeueAfter > sentinelRetryInterval {
		t.Fatalf("requeue %v, want up to %v", res.RequeueAfter, sentinelRetryInterval)
	}
	if n := dials(); n != 3 {
		t.Fatalf("%d dials, want 3", n)
	}
	ageSentinelAnswers(f, 2*sentinelQueryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := dials(); n != 3 {
		t.Fatalf("%d dials within %s of an unanswered query, want 3", n, sentinelRetryInterval)
	}
	ageSentinelAnswers(f, sentinelRetryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := dials(); n != 6 {
		t.Fatalf("%d dials after %s, want 6", n, sentinelRetryInterval)
	}
}

// --- a new replication group is no failover ---

// A new replication group is no failover; turning highAvailability off on the way also
// removes the Valkey report and its condition.
func TestValkeyNewGroupIsNoFailover(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	sentinels := map[string]*fakeSentinel{}
	for i := range 3 {
		sentinels[fmt.Sprintf("10.0.0.%d", i+1)] = newFakeSentinel(t, host(f, 1),
			replicaEntry(host(f, 0), "ok", "slave"), replicaEntry(host(f, 2), "ok", "slave"))
	}
	withSentinels(t, sentinels)
	setMaster := func(i int) {
		var reps [][]string
		for j := range 3 {
			if j != i {
				reps = append(reps, replicaEntry(host(f, j), "ok", "slave"))
			}
		}
		for _, s := range sentinels {
			s.set(host(f, i), reps...)
		}
	}
	report := func(env testEnv) []string {
		t.Helper()
		expireSentinelAnswers(f)
		if _, err := ReportValkey(ctx, env.rc); err != nil {
			t.Fatal(err)
		}
		return env.events()
	}
	podsOf := func(gen string) []client.Object {
		return []client.Object{uidPod(f, 0, gen+"0"), uidPod(f, 1, gen+"1"), uidPod(f, 2, gen+"2")}
	}

	// The group runs with pod 1 as the primary.
	objs := append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("a")...)
	env := newTestEnv(t, f, objs...)
	report(env)
	if got := f.Status.HighAvailability.Valkey.Master; got != testValkeyPod1 {
		t.Fatalf("master %q", got)
	}

	// Every pod is replaced (Valkey emptied after a restore, the StatefulSet recreated): the new
	// group starts with pod 0 as its primary. Not a failover.
	objs = append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("b")...)
	env.rc.Client = newTestEnv(t, f, objs...).rc.Client
	setMaster(0)
	if evs := report(env); hasEvent(evs, "Warning ValkeyFailedOver") || f.Status.HighAvailability.Valkey.LastFailoverTime != nil {
		t.Fatalf("a new replication group reported as a failover: %v", evs)
	}

	// Within that group, a move of the primary is one.
	setMaster(2)
	if evs := report(env); !hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("no ValkeyFailedOver for a failover within the group: %v", evs)
	}

	// highAvailability off, then on again in the same operator process: the recreated Valkey
	// is a new group, whichever pod led the old one. Without highAvailability the report and
	// its condition are removed.
	f.Spec.HighAvailability.Enabled = false
	report(env)
	if f.Status.HighAvailability != nil || condition(f, forgejov1.ConditionValkeyReady) != nil {
		t.Fatalf("without highAvailability: status %+v, condition %+v", f.Status.HighAvailability, condition(f, forgejov1.ConditionValkeyReady))
	}
	f.Spec.HighAvailability.Enabled = true
	objs = append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("c")...)
	env.rc.Client = newTestEnv(t, f, objs...).rc.Client
	setMaster(0)
	if evs := report(env); hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("turning HA off and on reported as a failover: %v", evs)
	}
}

func TestFailedOver(t *testing.T) {
	f := haInstance()
	pods := []corev1.Pod{*uidPod(f, 0, "a0"), *uidPod(f, 1, "a1"), *uidPod(f, 2, "b2")}
	group := map[string]types.UID{testValkeyPod0: "a0", testValkeyPod1: "a1", testValkeyPod2: "a2"}
	for _, tt := range []struct {
		name         string
		prev, master string
		group        map[string]types.UID
		want         bool
	}{
		{"first look", "", testValkeyPod0, group, false},
		{"unchanged", testValkeyPod0, testValkeyPod0, group, false},
		{"to a member", testValkeyPod0, testValkeyPod1, group, true},
		{"to a pod that replaced a member", testValkeyPod0, testValkeyPod2, group, false},
		{"to a pod that was not there", testValkeyPod0, testValkeyPod1, map[string]types.UID{testValkeyPod0: "a0"}, false},
		{"after an operator restart", testValkeyPod0, testValkeyPod1, nil, true},
	} {
		if got := failedOver(tt.prev, tt.group, tt.master, pods); got != tt.want {
			t.Errorf("%s: failedOver = %v, want %v", tt.name, got, tt.want)
		}
	}
}
