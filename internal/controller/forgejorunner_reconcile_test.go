package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/runner"
)

const runnerTestNS = "runner-test"

// fakeRunnerAPI is Forgejo's runner API in memory. Registration Jobs are simulated by
// registerJob, which adds what the Job registers.
type fakeRunnerAPI struct {
	mu      sync.Mutex
	runners map[string]runner.APIRunner // by UUID
	nextID  int64
	err     error
	deleted []int64
	waiting int
	user    string
	pass    string
	// hidden runners (by UUID) are left out of ListRunners, as a list read in pages can miss
	// them; GetRunner still finds them.
	hidden map[string]bool
}

func newFakeRunnerAPI() *fakeRunnerAPI {
	return &fakeRunnerAPI{runners: map[string]runner.APIRunner{}, nextID: 1}
}

func (a *fakeRunnerAPI) ListRunners(context.Context) ([]runner.APIRunner, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	out := make([]runner.APIRunner, 0, len(a.runners))
	for _, r := range a.runners {
		if !a.hidden[r.UUID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *fakeRunnerAPI) GetRunner(_ context.Context, id int64) (*runner.APIRunner, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	for _, r := range a.runners {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, nil
}

func (a *fakeRunnerAPI) DeleteRunner(_ context.Context, id int64) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return false, a.err
	}
	for u, r := range a.runners {
		if r.ID == id {
			delete(a.runners, u)
			a.deleted = append(a.deleted, id)
			return true, nil
		}
	}
	return false, nil
}

func (a *fakeRunnerAPI) WaitingJobs(context.Context, string, []string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waiting, a.err
}

func (a *fakeRunnerAPI) register(uuid, name, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.runners[uuid]; ok {
		return
	}
	a.runners[uuid] = runner.APIRunner{ID: a.nextID, UUID: uuid, Name: name, Status: status}
	a.nextID++
}

type runnerFixture struct {
	t   *testing.T
	c   client.Client
	r   *ForgejoRunnerReconciler
	api *fakeRunnerAPI
	rec *events.FakeRecorder
	// clock is the reconciler's time; zero is the wall clock.
	clock time.Time
	// failStatusPatches makes that many ForgejoRunner status patches fail with a conflict, as
	// when another write (a scale, a label) came in since the object was read.
	failStatusPatches int
}

func readyForgejo() *forgejov1.Forgejo {
	return &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: runnerTestNS, UID: "forgejo-uid"},
		Status: forgejov1.ForgejoStatus{
			Image: "codeberg.org/forgejo/forgejo:17",
			Phase: forgejov1.PhaseRunning,
			Conditions: []metav1.Condition{{
				Type: forgejov1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func newRunnerFixture(t *testing.T, objs ...client.Object) *runnerFixture {
	t.Helper()
	s := testScheme(t)
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-admin", Namespace: runnerTestNS},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: runnerTestNS}}
	all := append([]client.Object{admin, ns}, objs...)
	fx := &runnerFixture{t: t}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(all...).
		WithStatusSubresource(&forgejov1.ForgejoRunner{}, &forgejov1.Forgejo{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*forgejov1.ForgejoRunner); ok && sub == "status" && fx.failStatusPatches > 0 {
					fx.failStatusPatches--
					return apierrors.NewConflict(schema.GroupResource{Group: forgejov1.GroupVersion.Group, Resource: "forgejorunners"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	api := newFakeRunnerAPI()
	rec := events.NewFakeRecorder(100)
	r := &ForgejoRunnerReconciler{Client: c, Scheme: s, Recorder: rec}
	r.NewAPI = func(base, user, pass string) runner.API {
		api.user, api.pass = user, pass
		return api
	}
	r.Now = func() time.Time {
		if fx.clock.IsZero() {
			return time.Now()
		}
		return fx.clock
	}
	fx.c, fx.r, fx.api, fx.rec = c, r, api, rec
	return fx
}

func testForgejoRunner(name string, replicas int32) *forgejov1.ForgejoRunner {
	return &forgejov1.ForgejoRunner{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: runnerTestNS, Generation: 1},
		Spec: forgejov1.ForgejoRunnerSpec{
			ForgejoRef: forgejov1.ForgejoRef{Name: "git"},
			Replicas:   ptr.To(replicas),
			Capacity:   1,
			Executor:   forgejov1.RunnerExecutorDocker,
			Image:      forgejov1.DefaultRunnerImage,
		},
	}
}

func (fx *runnerFixture) reconcile(name string) ctrl.Result {
	fx.t.Helper()
	res, err := fx.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: runnerTestNS, Name: name}})
	if err != nil {
		fx.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (fx *runnerFixture) runner(name string) *forgejov1.ForgejoRunner {
	fx.t.Helper()
	fr := &forgejov1.ForgejoRunner{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name}, fr); err != nil {
		fx.t.Fatal(err)
	}
	return fr
}

func (fx *runnerFixture) condition(name, t string) *metav1.Condition {
	return meta.FindStatusCondition(fx.runner(name).Status.Conditions, t)
}

func (fx *runnerFixture) tokens(name string) map[string][]byte {
	fx.t.Helper()
	sec := &corev1.Secret{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-tokens"}, sec); err != nil {
		fx.t.Fatal(err)
	}
	return sec.Data
}

func (fx *runnerFixture) configKeys(name string) []string {
	fx.t.Helper()
	sec := &corev1.Secret{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-runner"}, sec); err != nil {
		fx.t.Fatal(err)
	}
	keys := make([]string, 0, len(sec.Data))
	for k := range sec.Data {
		keys = append(keys, k)
	}
	return keys
}

func (fx *runnerFixture) statefulSet(name string) *appsv1.StatefulSet {
	fx.t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name}, sts); err != nil {
		fx.t.Fatal(err)
	}
	return sts
}

// completeRegistration lets the registration Job succeed: the runners it names are registered
// in the fake Forgejo (online), and the Job is marked Complete.
func (fx *runnerFixture) completeRegistration(name string) {
	fx.t.Helper()
	job := fx.registerJob(name)
	for i, uuid := range runner.ParseRegistrationAnnotation(job.Annotations[runner.AnnotationRegistrationRunners]) {
		fx.api.register(uuid, runner.RunnerName(fx.runner(name), i), "idle")
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	if err := fx.c.Status().Update(context.Background(), job); err != nil {
		fx.t.Fatal(err)
	}
}

// failRegistration marks the registration Job Failed at the fixture's clock.
func (fx *runnerFixture) failRegistration(name string) {
	fx.t.Helper()
	job := fx.registerJob(name)
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		LastTransitionTime: metav1.NewTime(fx.clock),
	})
	if err := fx.c.Status().Update(context.Background(), job); err != nil {
		fx.t.Fatal(err)
	}
}

// setReady pretends the StatefulSet controller ran its pods.
func (fx *runnerFixture) setReady(name string) {
	fx.t.Helper()
	sts := fx.statefulSet(name)
	n := *sts.Spec.Replicas
	sts.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: sts.Generation, Replicas: n, ReadyReplicas: n, UpdatedReplicas: n, AvailableReplicas: n,
	}
	if err := fx.c.Status().Update(context.Background(), sts); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *runnerFixture) scale(name string, replicas int32) {
	fx.t.Helper()
	fr := fx.runner(name)
	fr.Spec.Replicas = ptr.To(replicas)
	if err := fx.c.Update(context.Background(), fr); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *runnerFixture) registerJob(name string) *batchv1.Job {
	fx.t.Helper()
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-register"}, job); err != nil {
		fx.t.Fatalf("registration job: %v", err)
	}
	return job
}

func (fx *runnerFixture) registerJobExists(name string) bool {
	fx.t.Helper()
	err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-register"}, &batchv1.Job{})
	if err != nil && !apierrors.IsNotFound(err) {
		fx.t.Fatal(err)
	}
	return err == nil
}

func (fx *runnerFixture) addPod(name string, i int32) {
	fx.t.Helper()
	fr := fx.runner(name)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: runner.RunnerName(fr, i), Namespace: runnerTestNS, Labels: runner.Labels(fr, forgejo.ComponentRunner),
	}}
	if err := fx.c.Create(context.Background(), pod); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *runnerFixture) deletePod(name string, i int32) {
	fx.t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: runner.RunnerName(fx.runner(name), i), Namespace: runnerTestNS}}
	if err := fx.c.Delete(context.Background(), pod); err != nil {
		fx.t.Fatal(err)
	}
}

// drainEvents empties rec and returns the events that contain every one of substrs.
func drainEvents(rec *events.FakeRecorder, substrs ...string) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			match := true
			for _, s := range substrs {
				match = match && strings.Contains(e, s)
			}
			if match {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// listedNames are the names of the runners Forgejo lists.
func (fx *runnerFixture) listedNames() []string {
	fx.t.Helper()
	list, _ := fx.api.ListRunners(context.Background())
	names := make([]string, 0, len(list))
	for _, r := range list {
		names = append(names, r.Name)
	}
	return names
}

// startRunners takes a ForgejoRunner from creation to running, registered runners.
func (fx *runnerFixture) startRunners(name string) {
	fx.t.Helper()
	fx.reconcile(name) // secrets, registration Job
	fx.completeRegistration(name)
	fx.reconcile(name) // records, StatefulSet scaled up
	fx.setReady(name)
	fx.reconcile(name)
}

func TestRunnerRegistersBeforeStartingPods(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.reconcile("ci")

	tokens := fx.tokens("ci")
	if len(tokens) != 2 || !runner.ValidToken(string(tokens["token-0"])) || !runner.ValidToken(string(tokens["token-1"])) {
		t.Fatalf("tokens %v", tokens)
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 0 {
		t.Fatalf("pods started before their runners were registered: replicas %d", *sts.Spec.Replicas)
	}
	if c := fx.registerJob("ci").Spec.Template.Spec.Containers[0]; c.Image != "codeberg.org/forgejo/forgejo:17" {
		t.Errorf("registration image %s", c.Image)
	}
	if c := fx.condition("ci", forgejov1.ConditionRunnerRegistered); c == nil || c.Reason != reasonRegistering {
		t.Fatalf("Registered %+v", c)
	}

	fx.completeRegistration("ci")
	fx.reconcile("ci")
	fr := fx.runner("ci")
	if len(fr.Status.Runners) != 2 || fr.Status.Runners[1].Name != "ci-1" ||
		fr.Status.Runners[1].UUID != runner.UUIDOf(string(tokens["token-1"])) || fr.Status.Runners[0].ID == 0 {
		t.Fatalf("records %+v", fr.Status.Runners)
	}
	if fx.registerJobExists("ci") {
		t.Error("the finished registration Job was kept")
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d after registration", *sts.Spec.Replicas)
	}
	if c := fx.condition("ci", forgejov1.ConditionRunnerRegistered); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Registered %+v", c)
	}
	fx.setReady("ci")
	fx.reconcile("ci")
	if c := fx.condition("ci", forgejov1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonRunnersOnline {
		t.Fatalf("Ready %+v", c)
	}
	if fx.runner("ci").Status.OnlineRunners != 2 {
		t.Errorf("onlineRunners %d", fx.runner("ci").Status.OnlineRunners)
	}
	if fx.api.user != "forgejo_admin" || fx.api.pass != "admin-password" {
		t.Errorf("API credentials %s/%s", fx.api.user, fx.api.pass)
	}
	// A further pass registers nothing again.
	fx.reconcile("ci")
	if fx.registerJobExists("ci") {
		t.Error("registered again")
	}
}

func TestRunnerWaitsForForgejo(t *testing.T) {
	f := readyForgejo()
	f.Status.Phase = forgejov1.PhaseUpgrading
	fx := newRunnerFixture(t, f, testForgejoRunner("ci", 1))
	fx.reconcile("ci")
	if fx.registerJobExists("ci") {
		t.Fatal("registered with an upgrading instance")
	}
	if c := fx.condition("ci", forgejov1.ConditionRunnerRegistered); c == nil || c.Reason != reasonWaitingForForgejo || !strings.Contains(c.Message, "upgrading") {
		t.Fatalf("Registered %+v", c)
	}
}

func TestRunnerForgejoNotFound(t *testing.T) {
	fx := newRunnerFixture(t, testForgejoRunner("ci", 1))
	fx.reconcile("ci")
	if c := fx.condition("ci", forgejov1.ConditionReady); c == nil || c.Reason != reasonForgejoNotFound {
		t.Fatalf("Ready %+v", c)
	}
}

func TestRunnerScaleDownUnregistersOnceThePodIsGone(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	fx.addPod("ci", 1)
	gone := fx.runner("ci").Status.Runners[1]

	fr := fx.runner("ci")
	fr.Spec.Replicas = ptr.To[int32](1)
	if err := fx.c.Update(context.Background(), fr); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 1 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
	}
	if len(fx.api.deleted) != 0 {
		t.Fatalf("unregistered %v while its pod still runs (it may be finishing a job)", fx.api.deleted)
	}

	fx.deletePod("ci", 1)
	fx.reconcile("ci")
	if len(fx.api.deleted) != 1 || fx.api.deleted[0] != gone.ID {
		t.Fatalf("deleted %v, want [%d]", fx.api.deleted, gone.ID)
	}
	if recs := fx.runner("ci").Status.Runners; len(recs) != 1 || recs[0].Index != 0 {
		t.Fatalf("records %+v", recs)
	}
	fx.reconcile("ci")
	if tokens := fx.tokens("ci"); len(tokens) != 1 || tokens["token-0"] == nil {
		t.Fatalf("tokens after scale-down %v", tokens)
	}
	if names := fx.listedNames(); len(names) != 1 {
		t.Fatalf("Forgejo has %v, want 1 runner", names)
	}
	if len(drainEvents(fx.rec, "Unregistered runners ci-1")) == 0 {
		t.Error("no Unregistered event")
	}
}

func TestRunnerUnregisterFailsWithoutAPI(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	fx.api.err = errors.New("HTTP 401: forgejo refused the admin credentials")
	fr := fx.runner("ci")
	fr.Spec.Replicas = ptr.To[int32](1)
	if err := fx.c.Update(context.Background(), fr); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if c := fx.condition("ci", forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonUnregisterFailed {
		t.Fatalf("Degraded %+v", c)
	}
	if recs := fx.runner("ci").Status.Runners; len(recs) != 2 {
		t.Fatalf("the record of a runner still registered was dropped: %+v", recs)
	}
	if tokens := fx.tokens("ci"); len(tokens) != 2 {
		t.Fatalf("the secret of a runner still registered was dropped")
	}
	fx.api.err = nil
	fx.reconcile("ci")
	if recs := fx.runner("ci").Status.Runners; len(recs) != 1 {
		t.Fatalf("records %+v after the API recovered", recs)
	}
	if c := fx.condition("ci", forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded %+v", c)
	}
}

// A runner deleted in Forgejo is registered again under the same UUID while the runner pods,
// its own and the ones after it, keep running.
func TestRunnerMissingInForgejoIsRegisteredAgain(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	fx.clock = time.Now().Add(time.Hour) // registered long ago
	ci0 := fx.runner("ci").Status.Runners[0]
	if _, err := fx.api.DeleteRunner(context.Background(), ci0.ID); err != nil { // deleted in Forgejo's UI
		t.Fatal(err)
	}
	fx.reconcile("ci")
	rec := fx.runner("ci").Status.Runners[0]
	if !rec.Missing || rec.UUID != ci0.UUID {
		t.Fatalf("record of the deleted runner %+v", rec)
	}
	if !fx.registerJobExists("ci") {
		t.Fatal("not registered again")
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d while ci-0 is registered again: the healthy runners were stopped", *sts.Spec.Replicas)
	}
	if len(drainEvents(fx.rec, "RunnerMissing", "ci-0")) == 0 {
		t.Error("no RunnerMissing event")
	}

	// The Job has registered the runner but has not finished: Forgejo lists it again, and it
	// is recorded (with its event) when the Job is collected.
	fx.api.register(ci0.UUID, "ci-0", "idle")
	fx.reconcile("ci")
	if rec := fx.runner("ci").Status.Runners[0]; !rec.Missing {
		t.Fatalf("recorded before its registration Job finished: %+v", rec)
	}
	fx.completeRegistration("ci")
	fx.reconcile("ci")
	rec = fx.runner("ci").Status.Runners[0]
	if rec.Missing || rec.UUID != ci0.UUID || rec.ID == 0 || rec.ID == ci0.ID {
		t.Fatalf("record after registering again %+v (old id %d)", rec, ci0.ID)
	}
	if len(drainEvents(fx.rec, "Registered runners ci-0 with")) == 0 {
		t.Error("no Registered event for the runner registered again")
	}
	if c := fx.condition("ci", forgejov1.ConditionRunnerRegistered); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Registered %+v", c)
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
	}

	// Recorded a moment ago: a runner deleted again at once is not registered in a loop.
	if _, err := fx.api.DeleteRunner(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if fx.registerJobExists("ci") {
		t.Fatal("registered again right after registering")
	}
}

// Forgejo's list can miss a runner (pages read while runners move between them): a recorded
// runner is asked for by its id before it is taken for missing, and deleted by its id.
func TestRunnerListThatMissesARunner(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	fx.clock = time.Now().Add(time.Hour)
	recs := fx.runner("ci").Status.Runners
	fx.api.hidden = map[string]bool{recs[1].UUID: true}
	fx.reconcile("ci")
	if rec := fx.runner("ci").Status.Runners[1]; rec.Missing || rec.ID != recs[1].ID {
		t.Fatalf("a runner the list missed was taken for missing: %+v", rec)
	}
	if fx.registerJobExists("ci") {
		t.Fatal("a runner the list missed is registered again")
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
	}

	// Scaled away: deleted by its id although the list lacks it, so no ghost stays.
	fx.scale("ci", 1)
	fx.reconcile("ci")
	if len(fx.api.deleted) != 1 || fx.api.deleted[0] != recs[1].ID {
		t.Fatalf("deleted %v, want [%d]", fx.api.deleted, recs[1].ID)
	}

	// Deletion: the recorded runner the list misses is deleted by its id too.
	fx.api.hidden[recs[0].UUID] = true
	if err := fx.c.Delete(context.Background(), fx.runner("ci")); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if len(fx.api.runners) != 0 {
		t.Fatalf("Forgejo still has %+v", fx.api.runners)
	}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci"}, &forgejov1.ForgejoRunner{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer kept: %v", err)
	}
}

// Scaling up or down changes nothing on the pod template, so the running runners are not
// rolled.
func TestRunnerScalingLeavesThePodTemplateAlone(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	template := fx.statefulSet("ci").Spec.Template
	unchanged := func(when string) {
		t.Helper()
		if got := fx.statefulSet("ci").Spec.Template; !apiequality.Semantic.DeepEqual(got, template) {
			t.Fatalf("%s changed the pod template, which rolls the running runners: annotations %v, were %v",
				when, got.Annotations, template.Annotations)
		}
	}
	fx.scale("ci", 2)
	fx.reconcile("ci")
	if len(fx.tokens("ci")) != 2 || !fx.registerJobExists("ci") {
		t.Fatal("ci-1 has no secret or registration")
	}
	unchanged("the secret of a new runner")
	fx.completeRegistration("ci")
	fx.reconcile("ci")
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
	}
	unchanged("scaling up")
	fx.setReady("ci")
	fx.addPod("ci", 1)

	fx.scale("ci", 1)
	fx.reconcile("ci")
	unchanged("scaling down")
	fx.deletePod("ci", 1)
	fx.reconcile("ci")
	fx.reconcile("ci")
	if len(fx.tokens("ci")) != 1 {
		t.Fatalf("tokens %v", fx.tokens("ci"))
	}
	unchanged("dropping the secret of a removed runner")
}

// The registration Job is deleted only once its runners are recorded: a status write that
// fails keeps it (and the pods down), and the next pass records the runners, also when a
// scale-down came in between, which then unregisters the extra runner instead of leaving it
// registered without a record.
func TestRunnerRegistrationJobOutlivesAFailedStatusWrite(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("late", 2))
	fx.reconcile("late")
	fx.completeRegistration("late")
	fx.failStatusPatches = 2 // the write of the records and the pass's own status write
	fx.reconcile("late")
	if !fx.registerJobExists("late") {
		t.Fatal("the registration Job was deleted although its runners were not recorded")
	}
	if recs := fx.runner("late").Status.Runners; len(recs) != 0 {
		t.Fatalf("records %+v", recs)
	}
	if sts := fx.statefulSet("late"); *sts.Spec.Replicas != 0 {
		t.Fatalf("pods started on records that were not stored: replicas %d", *sts.Spec.Replicas)
	}

	fx.scale("late", 1)
	fx.reconcile("late")
	if fx.registerJobExists("late") {
		t.Fatal("the registration Job was kept after its runners were recorded")
	}
	if recs := fx.runner("late").Status.Runners; len(recs) != 1 || recs[0].Name != "late-0" {
		t.Fatalf("records %+v", recs)
	}
	if names := fx.listedNames(); len(names) != 1 || names[0] != "late-0" {
		t.Fatalf("Forgejo has %v, want only late-0", names)
	}
	if sts := fx.statefulSet("late"); *sts.Spec.Replicas != 1 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
	}
}

// A registration Job that registered ci-1 and then failed leaves ci-1 in Forgejo without a
// record. Scaled back meanwhile, the secrets of the failed Job's runners are kept (and get no
// configuration) while the Job is kept for registrationRetry and while Forgejo's API cannot be
// read. Once it can, ci-1 is deleted in Forgejo before its secret is dropped, so no offline
// ghost stays behind.
func TestRunnerFailedRegistrationLeavesNoGhost(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	fx.clock = time.Now()
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	fx.scale("ci", 3)
	fx.reconcile("ci")
	regs := runner.ParseRegistrationAnnotation(fx.registerJob("ci").Annotations[runner.AnnotationRegistrationRunners])
	if len(regs) != 2 || regs[1] == "" || regs[2] == "" {
		t.Fatalf("registration Job registers %v, want ci-1 and ci-2", regs)
	}
	fx.api.register(regs[1], "ci-1", "offline")
	ci1 := fx.api.runners[regs[1]].ID
	fx.failRegistration("ci")
	fx.scale("ci", 1)
	fx.reconcile("ci")
	if !fx.registerJobExists("ci") {
		t.Fatal("the failed registration Job was not kept for registrationRetry")
	}
	if tokens := fx.tokens("ci"); len(tokens) != 3 {
		t.Fatalf("the secrets of the failed Job's runners were dropped while it exists: %d left", len(tokens))
	}

	fx.clock = fx.clock.Add(registrationRetry + time.Second)
	fx.api.err = errors.New("connection refused")
	fx.reconcile("ci") // the failed Job goes
	fx.reconcile("ci")
	if fx.registerJobExists("ci") {
		t.Fatal("the failed registration Job was kept after registrationRetry")
	}
	if tokens := fx.tokens("ci"); len(tokens) != 3 {
		t.Fatalf("secrets dropped while Forgejo's API cannot be read: %d left", len(tokens))
	}
	if keys := fx.configKeys("ci"); len(keys) != 1 || keys[0] != "config-0.yaml" {
		t.Fatalf("configuration files %v: a secret kept for pruning got one", keys)
	}
	if c := fx.condition("ci", forgejov1.ConditionDegraded); c == nil || c.Reason != reasonAPIUnavailable {
		t.Fatalf("Degraded %+v", c)
	}

	drainEvents(fx.rec)
	fx.api.err = nil
	fx.reconcile("ci")
	if len(fx.api.deleted) != 1 || fx.api.deleted[0] != ci1 {
		t.Fatalf("deleted %v in Forgejo, want ci-1 (id %d)", fx.api.deleted, ci1)
	}
	if names := fx.listedNames(); len(names) != 1 || names[0] != "ci-0" {
		t.Fatalf("Forgejo has %v, want only ci-0", names)
	}
	if tokens := fx.tokens("ci"); len(tokens) != 1 || tokens["token-0"] == nil {
		t.Fatalf("tokens %v, want only token-0", tokens)
	}
	if len(drainEvents(fx.rec, "Unregistered runners ci-1 (id")) == 0 {
		t.Error("no Unregistered event for ci-1")
	}
	fx.reconcile("ci")
	if keys := fx.configKeys("ci"); len(keys) != 1 || keys[0] != "config-0.yaml" {
		t.Fatalf("configuration files %v", keys)
	}
	if c := fx.condition("ci", forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded %+v", c)
	}
}

// A runner registered again for another scope keeps the version Forgejo lists for it: the
// registration writes the version, and the pods are not restarted to declare it again.
func TestRunnerScopeChangeKeepsTheVersion(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	versions := map[string]string{"ci-0": "13.2.0", "ci-1": "12.7.0"}
	for u, r := range fx.api.runners {
		r.Version = versions[r.Name]
		fx.api.runners[u] = r
	}
	fr := fx.runner("ci")
	fr.Spec.Scope.Organization = "acme"
	if err := fx.c.Update(context.Background(), fr); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	job := fx.registerJob("ci")
	var env string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "RUNNERS" {
			env = e.Value
		}
	}
	if env != "0:13.2.0:ci-0 1:12.7.0:ci-1" {
		t.Fatalf("RUNNERS %q, want the listed versions", env)
	}
	if sts := fx.statefulSet("ci"); *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas %d during a scope change", *sts.Spec.Replicas)
	}
}

// A scaled-away pod stuck Terminating (a lost node) holds its runner's unregistration only
// until podStopMargin after its deletionTimestamp (which includes its grace period).
func TestRunnerStuckTerminatingPodIsUnregistered(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.clock = time.Now()
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	fr := fx.runner("ci")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: runner.RunnerName(fr, 1), Namespace: runnerTestNS, Labels: runner.Labels(fr, forgejo.ComponentRunner),
			Finalizers: []string{"example.com/lost-node"},
		},
		Spec: corev1.PodSpec{NodeName: "node-b"},
	}
	if err := fx.c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	ci1 := fx.runner("ci").Status.Runners[1]
	fx.scale("ci", 1)
	fx.reconcile("ci")
	if err := fx.c.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := fx.c.Get(context.Background(), client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp.IsZero() {
		t.Fatalf("pod not terminating: %v", err)
	}
	stopBy := pod.DeletionTimestamp.Add(podStopMargin)

	fx.clock = stopBy.Add(-time.Second)
	res := fx.reconcile("ci")
	if len(fx.api.deleted) != 0 || len(fx.runner("ci").Status.Runners) != 2 {
		t.Fatalf("unregistered within the margin: deleted %v", fx.api.deleted)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Errorf("requeued after %s, want at the end of the margin (1s)", res.RequeueAfter)
	}

	drainEvents(fx.rec)
	fx.clock = stopBy.Add(time.Second)
	fx.reconcile("ci")
	if len(fx.api.deleted) != 1 || fx.api.deleted[0] != ci1.ID {
		t.Fatalf("deleted %v, want ci-1 (id %d)", fx.api.deleted, ci1.ID)
	}
	if recs := fx.runner("ci").Status.Runners; len(recs) != 1 || recs[0].Index != 0 {
		t.Fatalf("records %+v", recs)
	}
	if stuck := drainEvents(fx.rec, "RunnerPodStuck", "ci-1", "node-b"); len(stuck) != 1 {
		t.Errorf("RunnerPodStuck events %q, want 1", stuck)
	}
	fx.reconcile("ci")
	if tokens := fx.tokens("ci"); tokens["token-1"] == nil {
		t.Fatal("the secret of a runner whose pod still exists was dropped (scaling back up reuses it)")
	}
	if len(drainEvents(fx.rec)) != 0 {
		t.Error("events repeated on the next pass")
	}

	// The pod goes (force-deleted): its secret goes too.
	pod.Finalizers = nil
	if err := fx.c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if tokens := fx.tokens("ci"); len(tokens) != 1 || tokens["token-0"] == nil {
		t.Fatalf("tokens %v after the stuck pod went", tokens)
	}
	if len(fx.api.deleted) != 1 {
		t.Fatalf("deleted %v", fx.api.deleted)
	}
}

func TestRunnerFinalizerUnregistersAll(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.startRunners("ci")
	fx.api.register("00000000-0000-0000-0000-000000000000", "someone-else", "idle")
	if err := fx.c.Delete(context.Background(), fx.runner("ci")); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("StatefulSet not deleted: %v", err)
	}
	if names := fx.listedNames(); len(names) != 1 || names[0] != "someone-else" {
		t.Fatalf("Forgejo still has %v", names)
	}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci"}, &forgejov1.ForgejoRunner{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer kept: %v", err)
	}
}

// The finalizer waits for the runner pods, then tries to unregister for unregisterGiveUp after
// its first failure, also when the pods took longer than that to stop.
func TestRunnerFinalizerWaitsForPodsThenGivesUp(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("slow", 1))
	fx.startRunners("slow")
	fx.addPod("slow", 0)
	fx.clock = time.Now()
	if err := fx.c.Delete(context.Background(), fx.runner("slow")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, 7 * time.Minute} { // the runner finishes a 7-minute job
		fx.clock = fx.clock.Add(d)
		if res := fx.reconcile("slow"); res.RequeueAfter == 0 || len(fx.api.deleted) != 0 {
			t.Fatalf("after %s: did not wait for the runner pod (unregistered %v)", d, fx.api.deleted)
		}
	}
	fx.deletePod("slow", 0)
	fx.api.err = errors.New("connection refused") // Forgejo restarts
	exists := func() bool {
		err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "slow"}, &forgejov1.ForgejoRunner{})
		return err == nil
	}
	if res := fx.reconcile("slow"); res.RequeueAfter == 0 || !exists() {
		t.Fatal("gave up at the first failed attempt")
	}
	fx.clock = fx.clock.Add(4 * time.Minute)
	fx.reconcile("slow")
	if !exists() {
		t.Fatal("gave up 4 minutes after the first failed attempt")
	}
	fx.clock = fx.clock.Add(2 * time.Minute)
	fx.reconcile("slow")
	if exists() {
		t.Fatal("did not give up 6 minutes after the first failed attempt")
	}
	if len(drainEvents(fx.rec, reasonUnregisterFailed, "slow-0")) == 0 {
		t.Error("no UnregisterFailed event")
	}
}

func TestRunnerPodSecurityBlocked(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	ns := &corev1.Namespace{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Name: runnerTestNS}, ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}
	if err := fx.c.Update(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("ci")
	fx.completeRegistration("ci")
	fx.reconcile("ci")
	if c := fx.condition("ci", forgejov1.ConditionReady); c == nil || c.Reason != reasonPodSecurityBlocked || !strings.Contains(c.Message, `"baseline"`) {
		t.Fatalf("Ready %+v", c)
	}

	host := testForgejoRunner("host", 1)
	host.Spec.Executor = forgejov1.RunnerExecutorHost
	if err := fx.c.Create(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("host")
	if c := fx.condition("host", forgejov1.ConditionReady); c != nil && c.Reason == reasonPodSecurityBlocked {
		t.Fatalf("the Host executor is blocked: %+v", c)
	}
}

func TestRunnerActionsDisabled(t *testing.T) {
	f := readyForgejo()
	f.Spec.Config = map[string]map[string]string{"actions": {"ENABLED": "false"}}
	fx := newRunnerFixture(t, f, testForgejoRunner("off", 1))
	fx.startRunners("off")
	if c := fx.condition("off", forgejov1.ConditionReady); c == nil || c.Reason != reasonActionsDisabled {
		t.Fatalf("Ready %+v", c)
	}
	if len(fx.tokens("off")) != 1 {
		t.Fatal("tokens")
	}
}

// No runner secret appears in status, events, the StatefulSet, the registration Job or the
// configuration files.
func TestRunnerSecretsStayInTheTokensSecret(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 2))
	fx.reconcile("ci")
	jobJSON, _ := json.Marshal(fx.registerJob("ci"))
	fx.completeRegistration("ci")
	fx.reconcile("ci")
	fx.setReady("ci")
	fx.reconcile("ci")

	tokens := fx.tokens("ci")
	secrets := make([]string, 0, 2*len(tokens))
	for _, v := range tokens {
		secrets = append(secrets, string(v), string(v)[16:])
	}
	cfg := &corev1.Secret{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci-runner"}, cfg); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(fx.runner("ci").Status)
	sts, _ := json.Marshal(fx.statefulSet("ci"))
	cfgJSON, _ := json.Marshal(cfg.Data)
	evs := strings.Join(drainEvents(fx.rec), "\n")
	for where, text := range map[string]string{"status": string(status), "statefulset": string(sts), "job": string(jobJSON),
		"configuration": string(cfgJSON), "events": evs} {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("a runner secret is in the %s", where)
			}
		}
	}
}

// A ForgejoRunner and a Forgejo of the same name do not share objects, and the Forgejo
// controller ignores the runner pods.
func TestRunnerObjectsDoNotCollideWithTheInstance(t *testing.T) {
	f := readyForgejo()
	fr := testForgejoRunner("git", 1)
	for _, n := range []string{runner.TokensSecretName(fr), runner.ConfigSecretName(fr), runner.RegisterJobName(fr)} {
		for _, own := range []string{forgejo.ConfigSecretName(f), forgejo.SecretsName(f), forgejo.AdminSecretName(f),
			forgejo.LKGConfigSecretName(f), forgejo.SSHHostKeysSecretName(f), forgejo.BackupKeySecretName(f), forgejo.TLSSecretName(f)} {
			if n == own {
				t.Errorf("runner object %s collides with the instance's", n)
			}
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: runner.Labels(fr, forgejo.ComponentRunner)}}
	if managedByOperator().Generic(event.GenericEvent{Object: pod}) {
		t.Error("the Forgejo controller reconciles on runner pods")
	}
}
