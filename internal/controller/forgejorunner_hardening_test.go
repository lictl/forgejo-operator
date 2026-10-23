package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/runner"
)

func (fx *runnerFixture) registerJob(name string) *batchv1.Job {
	fx.t.Helper()
	job := &batchv1.Job{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Namespace: runnerTestNS, Name: name + "-register"}, job); err != nil {
		fx.t.Fatalf("registration job: %v", err)
	}
	return job
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

func (fx *runnerFixture) listedNames() []string {
	fx.t.Helper()
	list, _ := fx.api.ListRunners(context.Background())
	names := make([]string, 0, len(list))
	for _, r := range list {
		names = append(names, r.Name)
	}
	return names
}

// scaleUpThenFailHalfway scales "ci" from 1 to 3 runners, lets the registration Job register
// ci-1 and fail on ci-2, and scales back to 1 while the failed Job is kept. It returns ci-1's
// id in Forgejo.
func (fx *runnerFixture) scaleUpThenFailHalfway() int64 {
	fx.t.Helper()
	fx.clock = time.Now()
	fx.startRunners("ci")
	fx.addPod("ci", 0)
	fx.scale("ci", 3)
	fx.reconcile("ci")
	regs := runner.ParseRegistrationAnnotation(fx.registerJob("ci").Annotations[runner.AnnotationRegistrationRunners])
	if len(regs) != 2 || regs[1] == "" || regs[2] == "" {
		fx.t.Fatalf("registration Job registers %v, want ci-1 and ci-2", regs)
	}
	fx.api.register(regs[1], "ci-1", "offline")
	ci1 := fx.api.runners[regs[1]].ID
	fx.failRegistration("ci")
	fx.scale("ci", 1)
	fx.reconcile("ci")
	if !fx.registerJobExists("ci") {
		fx.t.Fatal("the failed registration Job was not kept for registrationRetry")
	}
	if tokens := fx.tokens("ci"); len(tokens) != 3 {
		fx.t.Fatalf("the secrets of the failed Job's runners were dropped while it exists: %d left", len(tokens))
	}
	return ci1
}

// A registration Job that registered ci-1 and then failed leaves ci-1 in Forgejo without a
// record. Scaled back meanwhile, its secret is dropped only after ci-1 is deleted in Forgejo,
// so no offline ghost stays behind.
func TestRunnerFailedRegistrationLeavesNoGhost(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	ci1 := fx.scaleUpThenFailHalfway()

	fx.clock = fx.clock.Add(registrationRetry + time.Second)
	fx.reconcile("ci") // the failed Job goes
	if fx.registerJobExists("ci") {
		t.Fatal("the failed registration Job was kept after registrationRetry")
	}
	drainEvents(fx.rec)
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
	var unregistered bool
	for _, e := range drainEvents(fx.rec) {
		unregistered = unregistered || strings.Contains(e, "Unregistered runners ci-1 (id")
	}
	if !unregistered {
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

// Without Forgejo's API the secrets of runners that may still be registered are kept (and get
// no configuration); they are pruned once the API is back.
func TestRunnerOrphanSecretsKeptWithoutAPI(t *testing.T) {
	fx := newRunnerFixture(t, readyForgejo(), testForgejoRunner("ci", 1))
	ci1 := fx.scaleUpThenFailHalfway()

	fx.clock = fx.clock.Add(registrationRetry + time.Second)
	fx.api.err = errors.New("connection refused")
	fx.reconcile("ci")
	fx.reconcile("ci")
	if fx.registerJobExists("ci") {
		t.Fatal("the failed registration Job was kept")
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

	fx.api.err = nil
	fx.reconcile("ci")
	if len(fx.api.deleted) != 1 || fx.api.deleted[0] != ci1 {
		t.Fatalf("deleted %v in Forgejo, want ci-1 (id %d)", fx.api.deleted, ci1)
	}
	if tokens := fx.tokens("ci"); len(tokens) != 1 || tokens["token-0"] == nil {
		t.Fatalf("tokens %v after the API recovered", tokens)
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
	var stuck int
	for _, e := range drainEvents(fx.rec) {
		if strings.Contains(e, "RunnerPodStuck") && strings.Contains(e, "ci-1") && strings.Contains(e, "node-b") {
			stuck++
		}
	}
	if stuck != 1 {
		t.Errorf("%d RunnerPodStuck events, want 1", stuck)
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
