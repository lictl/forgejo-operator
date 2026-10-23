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

const testRegisterJob = "ci-register"

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
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-register"}, job); err != nil {
		fx.t.Fatalf("registration job: %v", err)
	}
	for i, uuid := range runner.ParseRegistrationAnnotation(job.Annotations[runner.AnnotationRegistrationRunners]) {
		fx.api.register(uuid, runner.RunnerName(fx.runner(name), i), "idle")
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
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

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
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
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, job); err != nil {
		t.Fatalf("no registration job: %v", err)
	}
	if c := job.Spec.Template.Spec.Containers[0]; c.Image != "codeberg.org/forgejo/forgejo:17" {
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
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the finished registration Job was kept: %v", err)
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
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("registered again: %v", err)
	}
}

func TestRunnerWaitsForForgejo(t *testing.T) {
	f := readyForgejo()
	f.Status.Phase = forgejov1.PhaseUpgrading
	fx := newRunnerFixture(t, f, testForgejoRunner("ci", 1))
	fx.reconcile("ci")
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("registered with an upgrading instance: %v", err)
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
	list, _ := fx.api.ListRunners(context.Background())
	if len(list) != 1 {
		t.Fatalf("Forgejo has %d runners, want 1", len(list))
	}
	var unregistered bool
	for _, e := range drainEvents(fx.rec) {
		unregistered = unregistered || strings.Contains(e, "Unregistered runners ci-1")
	}
	if !unregistered {
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
	var missingEvent bool
	for _, e := range drainEvents(fx.rec) {
		missingEvent = missingEvent || strings.Contains(e, "RunnerMissing") && strings.Contains(e, "ci-0")
	}
	if !missingEvent {
		t.Error("no RunnerMissing event")
	}

	// The Job has registered the runner but has not finished: Forgejo lists it again, and it
	// is recorded (with its event) when the Job is collected.
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, job); err != nil {
		t.Fatal(err)
	}
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
	var registeredEvent bool
	for _, e := range drainEvents(fx.rec) {
		registeredEvent = registeredEvent || strings.Contains(e, "Registered runners ci-0 with")
	}
	if !registeredEvent {
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
	list, _ := fx.api.ListRunners(context.Background())
	if len(list) != 1 || list[0].Name != "late-0" {
		t.Fatalf("Forgejo has %+v, want only ci-0", list)
	}
	if sts := fx.statefulSet("late"); *sts.Spec.Replicas != 1 {
		t.Fatalf("replicas %d", *sts.Spec.Replicas)
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
	list, _ := fx.api.ListRunners(context.Background())
	if len(list) != 1 || list[0].Name != "someone-else" {
		t.Fatalf("Forgejo still has %+v", list)
	}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci"}, &forgejov1.ForgejoRunner{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer kept: %v", err)
	}
}

func TestRunnerFinalizerWaitsForPodsThenGivesUp(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	if err := fx.c.Delete(context.Background(), fx.runner("ci")); err != nil {
		t.Fatal(err)
	}
	if res := fx.reconcile("ci"); res.RequeueAfter == 0 {
		t.Fatal("did not wait for the runner pod")
	}
	if len(fx.api.deleted) != 0 {
		t.Fatal("unregistered while the pod runs")
	}
	fx.deletePod("ci", 0)
	fx.api.err = errors.New("connection refused")
	if res := fx.reconcile("ci"); res.RequeueAfter == 0 {
		t.Fatal("gave up at once")
	}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "ci"}, &forgejov1.ForgejoRunner{}); err != nil {
		t.Fatalf("released before giving up: %v", err)
	}
}

// The finalizer tries to unregister for unregisterGiveUp after its first failure, also when
// the pods took longer than that to stop.
func TestRunnerFinalizerBudgetStartsAfterThePods(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("slow", 1))
	fx.startRunners("slow")
	fx.addPod("slow", 0)
	fx.clock = time.Now()
	if err := fx.c.Delete(context.Background(), fx.runner("slow")); err != nil {
		t.Fatal(err)
	}
	fx.reconcile("slow")
	fx.clock = fx.clock.Add(7 * time.Minute) // the runner finishes a 7-minute job
	if res := fx.reconcile("slow"); res.RequeueAfter == 0 {
		t.Fatal("did not wait for the runner pod")
	}
	fx.deletePod("slow", 0)
	fx.api.err = errors.New("connection refused") // Forgejo restarts
	exists := func() bool {
		err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: "slow"}, &forgejov1.ForgejoRunner{})
		return err == nil
	}
	fx.reconcile("slow")
	if !exists() {
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
	var gaveUp bool
	for _, e := range drainEvents(fx.rec) {
		gaveUp = gaveUp || strings.Contains(e, reasonUnregisterFailed) && strings.Contains(e, "slow-0")
	}
	if !gaveUp {
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
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: testRegisterJob}, job); err != nil {
		t.Fatal(err)
	}
	jobJSON, _ := json.Marshal(job)
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
