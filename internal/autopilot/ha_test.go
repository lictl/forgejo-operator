package autopilot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testValkeyPassword = "s3cr3tvalkeypassword"
	testValkeyPod0     = "fj-valkey-0"
	testValkeyPod1     = "fj-valkey-1"
	testValkeyPod2     = "fj-valkey-2"
)

// The Forgejo URL names every Sentinel, the group and both passwords; no database.
func TestValkeyURL(t *testing.T) {
	f := haInstance()
	u, err := url.Parse(valkeyURL(f, testValkeyPassword))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "redis+sentinel" || u.Path != "" {
		t.Fatalf("scheme %q path %q", u.Scheme, u.Path)
	}
	if pw, _ := u.User.Password(); pw != testValkeyPassword {
		t.Fatalf("password %q", pw)
	}
	want := "fj-valkey-0.fj-valkey.ns.svc.cluster.local:26379,fj-valkey-1.fj-valkey.ns.svc.cluster.local:26379," +
		"fj-valkey-2.fj-valkey.ns.svc.cluster.local:26379"
	if u.Host != want {
		t.Fatalf("hosts %q, want %q", u.Host, want)
	}
	if q := u.Query(); q.Get("mastername") != valkeyMasterName || q.Get("sentinelpassword") != testValkeyPassword {
		t.Fatalf("query %v", q)
	}
}

func reconcileValkey(t *testing.T, f *forgejov1.Forgejo, zoned bool) (testEnv, *appsv1.StatefulSet) {
	t.Helper()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	stubNodeZones(t, zoned)
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts) {
		t.Fatal("no StatefulSet")
	}
	return env, sts
}

// Three pods with a server and a Sentinel each, the password only in env references and in
// the generated configuration files (never in arguments), a headless Service that publishes
// not-ready pods, a PDB and spreading.
func TestValkeyStatefulSet(t *testing.T) {
	f := haInstance()
	f.Spec.HighAvailability.Valkey.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")}}
	_, sts := reconcileValkey(t, f, true)
	if *sts.Spec.Replicas != valkeyMembers || sts.Spec.ServiceName != forgejo.ValkeyName(f) ||
		sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Fatalf("spec: replicas %d service %q policy %q", *sts.Spec.Replicas, sts.Spec.ServiceName, sts.Spec.PodManagementPolicy)
	}
	ps := sts.Spec.Template.Spec
	if len(ps.Containers) != 2 || ps.Containers[0].Name != valkeyName || ps.Containers[1].Name != sentinelName || len(ps.InitContainers) != 1 {
		t.Fatalf("containers %v init %v", ps.Containers, ps.InitContainers)
	}
	for _, c := range append(ps.InitContainers, ps.Containers...) {
		all := strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")
		if strings.Contains(all, testValkeyPassword) {
			t.Fatalf("%s: password in command/args", c.Name)
		}
		for _, e := range c.Env {
			if e.Value == testValkeyPassword {
				t.Fatalf("%s: password as a literal env value", c.Name)
			}
		}
		if c.SecurityContext == nil || !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("%s: security context %+v", c.Name, c.SecurityContext)
		}
	}
	server := ps.Containers[0]
	if server.Lifecycle == nil || !strings.Contains(strings.Join(server.Lifecycle.PreStop.Exec.Command, " "), "sentinel failover "+valkeyMasterName) {
		t.Fatalf("preStop %+v", server.Lifecycle)
	}
	config := strings.Join(ps.InitContainers[0].Command, " ")
	for _, want := range []string{
		"sentinel monitor forgejo $master 6379 2", "sentinel down-after-milliseconds forgejo 5000",
		"sentinel failover-timeout forgejo 60000", "sentinel resolve-hostnames yes", "sentinel announce-hostnames yes",
		"replica-announce-ip $self", "requirepass $VALKEY_PASSWORD", "masterauth $VALKEY_PASSWORD",
		"maxmemory 83886080", "maxmemory-policy volatile-lru", "appendonly yes", "sentinel failover forgejo",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config script lacks %q", want)
		}
	}
	if len(ps.TopologySpreadConstraints) != 2 || ps.TopologySpreadConstraints[1].TopologyKey != corev1.LabelTopologyZone {
		t.Fatalf("spread %+v", ps.TopologySpreadConstraints)
	}
	for _, v := range ps.Volumes {
		if v.Name == volConf && v.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Fatal("configuration with the password not in memory")
		}
	}

}

// The headless Service publishes pods before they are Ready; the PDB lets one pod go at a
// time; spec.highAvailability.valkey.topologySpreadConstraints replace the default spread.
func TestValkeyServicePDBAndSpreadOverride(t *testing.T) {
	f := haInstance()
	env, sts := reconcileValkey(t, f, false)
	svc := &corev1.Service{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), svc) || svc.Spec.ClusterIP != corev1.ClusterIPNone ||
		!svc.Spec.PublishNotReadyAddresses || len(svc.Spec.Ports) != 2 {
		t.Fatalf("service %+v", svc.Spec)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), pdb) || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("pdb %+v", pdb.Spec)
	}

	// spec.highAvailability.valkey.topologySpreadConstraints replace the default.
	f.Spec.HighAvailability.Valkey.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "rack", WhenUnsatisfiable: corev1.DoNotSchedule}}
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts)
	if s := sts.Spec.Template.Spec.TopologySpreadConstraints; len(s) != 1 || s[0].TopologyKey != "rack" {
		t.Fatalf("override ignored: %+v", s)
	}
}

// Reconciling again does not change the StatefulSet (no update loop on API server defaults
// that the operator sets itself).
func TestValkeyStatefulSetStable(t *testing.T) {
	f := haInstance()
	env, sts := reconcileValkey(t, f, false)
	rv := sts.ResourceVersion
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts)
	if sts.ResourceVersion != rv {
		t.Fatal("StatefulSet updated without a change")
	}
}

// --- Sentinel status ---

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

// Without highAvailability the report and its condition are removed.
func TestReportValkeyClearedWithoutHA(t *testing.T) {
	f := testInstance()
	f.Status.HighAvailability = &forgejov1.HighAvailabilityStatus{Valkey: &forgejov1.ValkeyStatus{Master: "x"}}
	env := newTestEnv(t, f)
	env.rc.SetCondition(forgejov1.ConditionValkeyReady, metav1.ConditionTrue, ReasonValkeyAvailable, "")
	if _, err := ReportValkey(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.HighAvailability != nil || condition(f, forgejov1.ConditionValkeyReady) != nil {
		t.Fatalf("status %+v, condition %+v", f.Status.HighAvailability, condition(f, forgejov1.ConditionValkeyReady))
	}
}

// --- cron runner in the autopilot ---

// cronRunnerFor is the instance's cron runner StatefulSet, rolled out (or not) with testHash.
func cronRunnerFor(f *forgejov1.Forgejo, rolledOut bool) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace, Generation: 1,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo", Name: f.Name,
				UID: f.UID, Controller: ptr.To(true)}}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash}}}},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
			AvailableReplicas: 1, CurrentRevision: "fj-cron-1", UpdateRevision: "fj-cron-1"},
	}
	if !rolledOut {
		s.Status.AvailableReplicas, s.Status.ReadyReplicas, s.Status.UpdateRevision = 0, 0, "fj-cron-2"
	}
	return s
}

func cronPod(f *forgejov1.Forgejo, name string, since time.Duration) *corev1.Pod {
	p := readyPod(f, name, testHash, since)
	p.Labels = forgejo.CronSelectorLabels(f)
	return p
}

// The last known good app.ini also covers the cron runner: it is recorded only once that pod
// has run it healthy too.
func TestLastKnownGoodWaitsForCronRunner(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name      string
		rolledOut bool
		podSoak   time.Duration // 0: no cron pod
		recorded  bool
	}{
		{"cron runner not available", false, 0, false},
		{"cron runner pod not soaked", true, 10 * time.Second, false},
		{"cron runner soaked", true, time.Hour, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
			dep := testDeployment(f, 1)
			objs := []client.Object{dep, cronRunnerFor(f, tt.rolledOut), configSecret(f), readyPod(f, "fj-a", testHash, time.Hour)}
			if tt.podSoak != 0 {
				objs = append(objs, cronPod(f, "fj-cron-0", tt.podSoak))
			}
			env := newTestEnv(t, f, objs...)
			pods, err := forgejoPods(ctx, env.rc)
			if err != nil {
				t.Fatal(err)
			}
			cron, err := cronRunner(ctx, env.rc)
			if err != nil || cron == nil {
				t.Fatalf("cronRunner = %v, %v", cron, err)
			}
			if err := recordLastKnownGood(ctx, env.rc, dep, cron, pods, false); err != nil {
				t.Fatal(err)
			}
			if got := found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), &corev1.Secret{}); got != tt.recorded {
				t.Fatalf("LKG recorded = %v, want %v", got, tt.recorded)
			}
		})
	}
}

// "<name>-cron" that the instance does not control (another instance's or a user's) is not
// its cron runner: it neither blocks nor counts towards the last known good config.
func TestCronRunnerOfAnotherOwnerIgnoredByAutopilot(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	foreign := cronRunnerFor(f, false)
	foreign.OwnerReferences[0].UID, foreign.OwnerReferences[0].Name = "uid-other", "fj-other"
	env := newTestEnv(t, f, foreign)
	if cron, err := cronRunner(ctx, env.rc); err != nil || cron != nil {
		t.Fatalf("cronRunner = %v, %v; want none", cron, err)
	}
	f.Spec.HighAvailability.Enabled = false
	own := cronRunnerFor(f, true)
	env = newTestEnv(t, f, own)
	if cron, err := cronRunner(ctx, env.rc); err != nil || cron != nil {
		t.Fatalf("cronRunner without HA = %v, %v; want none", cron, err)
	}
}

// With highAvailability, an LKG rendered without it (LevelDB queues, cron tasks in every pod)
// is not rolled back to, nor one rendered with a Valkey password that Valkey no longer has.
func TestLKGWithoutHAUnusableWithHA(t *testing.T) {
	f := haInstance()
	env := newTestEnv(t, f)
	env.rc.ValkeyURL = valkeyURL(f, "new-password")
	plain := lkgSecret(f, false)
	if why, err := lkgUnusable(context.Background(), env.rc, plain); err != nil || !strings.Contains(why, "without highAvailability") {
		t.Fatalf("why = %q, err = %v", why, err)
	}
	ha := lkgSecret(f, true)
	ha.Annotations[annotationUsesValkey] = valueTrue
	ha.Annotations[annotationValkeyURL] = valkeyURLDigest(env.rc.ValkeyURL)
	ha.Data[forgejo.ConfigSecretKeyWeb] = []byte("[cron]\nENABLED = false\n")
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || why != "" {
		t.Fatalf("HA LKG: why = %q, err = %v", why, err)
	}
	// valkey-password changed after the LKG was recorded: Valkey is emptied and restarted
	// with the new password only, so the LKG's app.ini could not reach it.
	ha.Annotations[annotationValkeyURL] = valkeyURLDigest(valkeyURL(f, "old-password"))
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || !strings.Contains(why, "another Valkey password") {
		t.Fatalf("LKG with the old password: why = %q, err = %v", why, err)
	}
	delete(ha.Annotations, annotationValkeyURL)
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || !strings.Contains(why, "another Valkey password") {
		t.Fatalf("LKG without a password digest: why = %q, err = %v", why, err)
	}
}

// The cron runner is one more pod sharing PostgreSQL's connections.
func TestDBMaxOpenConnsCountsCronRunner(t *testing.T) {
	f := haInstance()
	f.Spec.Replicas = ptr.To[int32](3)
	// (100 - 10) / (3 replicas + 1 surge + 1 cron runner) = 18
	if got := dbMaxOpenConns(f); got != 18 {
		t.Fatalf("MAX_OPEN_CONNS = %d, want 18", got)
	}
}

// --- node zones and shared storage ---

// withNodes serves nodeCapacity from a fake clientset with nodes, starting with an empty
// cache; the returned func makes every later List fail with err.
func withNodes(t *testing.T, nodes ...*corev1.Node) func(err error) {
	t.Helper()
	objs := make([]k8sruntime.Object, len(nodes))
	for i, n := range nodes {
		objs[i] = n
	}
	cs := kubefake.NewSimpleClientset(objs...)
	kubeMu.Lock()
	oldClient, oldConfig := kubeClient, kubeConfig
	kubeClient, kubeConfig = cs, &rest.Config{}
	kubeMu.Unlock()
	nodeInfo.Lock()
	oldAt, oldCount, oldMem, oldZoned, oldKnown := nodeInfo.at, nodeInfo.count, nodeInfo.maxMem, nodeInfo.zoned, nodeInfo.zonesKnown
	nodeInfo.at, nodeInfo.zonesKnown = time.Time{}, false
	nodeInfo.Unlock()
	t.Cleanup(func() {
		kubeMu.Lock()
		kubeClient, kubeConfig = oldClient, oldConfig
		kubeMu.Unlock()
		nodeInfo.Lock()
		nodeInfo.at, nodeInfo.count, nodeInfo.maxMem, nodeInfo.zoned, nodeInfo.zonesKnown = oldAt, oldCount, oldMem, oldZoned, oldKnown
		nodeInfo.Unlock()
	})
	return func(err error) {
		cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, k8sruntime.Object, error) { return true, nil, err })
		nodeInfo.Lock()
		nodeInfo.at = time.Now().Add(-time.Hour) // expire the cache
		nodeInfo.Unlock()
	}
}

func testNode(name, zone string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if zone != "" {
		n.Labels[corev1.LabelTopologyZone] = zone
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

// The zones are in every HA pod template, so they must not flip on a failed read or on
// nodes that are NotReady for a moment: either would roll every HA pod twice.
func TestNodeZonesStable(t *testing.T) {
	ctx := context.Background()
	fail := withNodes(t, testNode("a", "zone-a", false), testNode("b", "", true))
	if zoned, err := nodeZones(ctx); err != nil || !zoned {
		t.Fatalf("zoned = %v, %v; a NotReady node with a zone counts", zoned, err)
	}
	fail(errors.New("connection refused"))
	if zoned, err := nodeZones(ctx); err != nil || !zoned {
		t.Fatalf("after a failed read: zoned = %v, %v; want the last answer", zoned, err)
	}
	if _, err := schedulableNodes(ctx); err == nil {
		t.Fatal("schedulableNodes hid the failed read")
	}

	// Before any successful read, the error is returned and the HA step fails instead of
	// guessing; without permission to read nodes it spreads across nodes only.
	fail = withNodes(t)
	fail(errors.New("connection refused"))
	if _, err := nodeZones(ctx); err == nil {
		t.Fatal("no error before the first successful read")
	}
	f := haInstance()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	nodeZonesFn = func(context.Context) (bool, error) { return false, errors.New("connection refused") }
	if _, err := ReconcileHA(ctx, env.rc); err == nil || !strings.Contains(err.Error(), "zones") {
		t.Fatalf("ReconcileHA err = %v", err)
	}
	if found(t, env.rc.Client, f, forgejo.ValkeyName(f), &appsv1.StatefulSet{}) {
		t.Fatal("Valkey rendered without knowing the zones")
	}
	nodeZonesFn = func(context.Context) (bool, error) {
		return false, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("namespaced"))
	}
	if _, err := ReconcileHA(ctx, env.rc); err != nil || env.rc.NodeZones {
		t.Fatalf("forbidden: err = %v, zones %v", err, env.rc.NodeZones)
	}
}

// With highAvailability the data volume always has several pods, even at one replica: the
// cron runner and a surge pod. On a ReadWriteOnce volume and several nodes that is Degraded.
func TestSharedStorageCountsCronRunnerAndSurge(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name     string
		mode     corev1.PersistentVolumeAccessMode
		nodes    int
		degraded bool
	}{
		{"RWO, 3 nodes", corev1.ReadWriteOnce, 3, true},
		{"RWO, 1 node", corev1.ReadWriteOnce, 1, false},
		{"RWX, 3 nodes", corev1.ReadWriteMany, 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			f.Spec.Replicas = ptr.To[int32](1)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{tt.mode}}}
			env := newTestEnv(t, f, pvc)
			stubNodeCapacity(t, tt.nodes, 0)
			if err := checkSharedStorage(ctx, env.rc, stateFor(f)); err != nil {
				t.Fatal(err)
			}
			c := condition(f, forgejov1.ConditionDegraded)
			got := c != nil && c.Status == metav1.ConditionTrue && c.Reason == ReasonHARequiresSharedStorage
			if got != tt.degraded {
				t.Fatalf("Degraded = %+v, want HARequiresSharedStorage %v", c, tt.degraded)
			}
			if got && !strings.Contains(c.Message, "up to 3 Forgejo pods (server pods: up to 1, plus") {
				t.Fatalf("message %q", c.Message)
			}
		})
	}
}
