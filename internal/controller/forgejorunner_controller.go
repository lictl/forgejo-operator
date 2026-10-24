/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/core"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/runner"
)

const (
	// runnerResync re-reads Forgejo's view of the runners (online state, waiting jobs).
	runnerResync = 60 * time.Second
	// runnerBusyRequeue is used while registering, unregistering or rolling out.
	runnerBusyRequeue = 10 * time.Second
	// registrationRetry keeps a failed registration Job (and its message) this long before a
	// new attempt.
	registrationRetry = time.Minute
	// unregisterGiveUp lets a deleted ForgejoRunner go when its runners could not be
	// unregistered for this long, counted from the first failed attempt (Forgejo's API refuses
	// or cannot be reached); a Warning event names them.
	unregisterGiveUp = 5 * time.Minute
	// podStopMargin is how long, beyond their grace period, a deletion waits for runner pods
	// that do not go away (e.g. on a lost node) before it unregisters the runners anyway.
	podStopMargin = 2 * time.Minute
	// missingGrace: a runner recorded this recently is not taken for missing in Forgejo (and
	// registered again), so that a runner that keeps being deleted is not registered in a loop.
	missingGrace = 2 * time.Minute
	// podSecurityEnforceLabel is the namespace label of Pod Security Admission.
	podSecurityEnforceLabel = "pod-security.kubernetes.io/enforce"
)

// Condition reasons of ForgejoRunner.
const (
	reasonForgejoNotFound    = "ForgejoNotFound"
	reasonWaitingForForgejo  = "WaitingForForgejo"
	reasonRegistering        = "Registering"
	reasonRegistrationFailed = "RegistrationFailed"
	reasonRegistered         = "Registered"
	reasonPodSecurityBlocked = "PodSecurityBlocked"
	reasonPodsNotReady       = "PodsNotReady"
	reasonPodsReady          = "PodsReady"
	reasonRunnersOffline     = "RunnersOffline"
	reasonRunnersOnline      = "RunnersOnline"
	reasonScaledToZero       = "ScaledToZero"
	reasonInvalidConfig      = "InvalidConfig"
	reasonAPIUnavailable     = "ForgejoAPIUnavailable"
	reasonUnregisterFailed   = "UnregisterFailed"
	reasonAsExpected         = "AsExpected"
	reasonActionsDisabled    = "ActionsDisabled"
)

// ForgejoRunnerReconciler reconciles a ForgejoRunner object: a StatefulSet of Forgejo Runner
// pods, each registered with the Forgejo instance under its own name (docs/actions-runners.md).
type ForgejoRunnerReconciler struct {
	client.Client
	// APIReader reads live where the cache must not decide (namespaces, a pod that may be
	// gone). nil falls back to Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// NewAPI returns a client of a Forgejo instance's API. nil uses runner.NewAPI.
	NewAPI func(base, username, password string) runner.API
	// statusWrites maps a ForgejoRunner to the resourceVersion of this controller's last
	// status write (see getRunner).
	statusWrites sync.Map
	// unregisterFailing maps a ForgejoRunner being deleted to the time its finalizer first
	// failed to unregister the runners (see finalize). It is not persisted: after an operator
	// restart the finalizer tries for another unregisterGiveUp, never for less.
	unregisterFailing sync.Map
	// Now is the clock. nil is time.Now.
	Now func() time.Time
}

func (r *ForgejoRunnerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorunners,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorunners/status,verbs=patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorunners/finalizers,verbs=update
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives a ForgejoRunner. Status is patched once per pass with an optimistic lock.
func (r *ForgejoRunnerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	return settle(ctx, res, err)
}

func (r *ForgejoRunnerReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fr, err := r.getRunner(ctx, req.NamespacedName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			runner.ForgetMetrics(req.Namespace, req.Name)
			r.statusWrites.Delete(req.NamespacedName)
			r.unregisterFailing.Delete(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, fr)
	}
	if !controllerutil.ContainsFinalizer(fr, forgejo.FinalizerName) {
		patch := client.MergeFromWithOptions(fr.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(fr, forgejo.FinalizerName)
		if err := r.Patch(ctx, fr, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	p := &pass{fr: fr, orig: fr.DeepCopy(), desired: runner.DesiredReplicas(fr)}
	res, err := r.sync(ctx, p)
	fr.Status.ObservedGeneration = fr.Generation
	if perr := r.patchStatus(ctx, p); perr != nil {
		if err == nil || apierrors.IsConflict(perr) {
			return ctrl.Result{}, perr
		}
		err = errors.Join(err, perr)
	}
	return res, err
}

// patchStatus writes what the pass changed in the status, with an optimistic lock, and
// remembers the write (see getRunner).
func (r *ForgejoRunnerReconciler) patchStatus(ctx context.Context, p *pass) error {
	if err := r.Status().Patch(ctx, p.fr, client.MergeFromWithOptions(p.orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	r.statusWrites.Store(client.ObjectKeyFromObject(p.fr), p.fr.ResourceVersion)
	p.orig = p.fr.DeepCopy()
	return nil
}

// getRunner reads the ForgejoRunner from the cache, or live while the cache has not caught up
// with this controller's last status write. The next pass usually starts within milliseconds
// (the Job or StatefulSet the previous pass changed queues it); starting from the older status
// it would, for example, start another registration Job for runners it just recorded.
func (r *ForgejoRunnerReconciler) getRunner(ctx context.Context, key types.NamespacedName) (*forgejov1.ForgejoRunner, error) {
	cached := &forgejov1.ForgejoRunner{}
	if err := r.Get(ctx, key, cached); err != nil {
		return nil, err
	}
	written, ok := r.statusWrites.Load(key)
	if !ok || written == cached.ResourceVersion {
		r.statusWrites.Delete(key)
		return cached, nil
	}
	live := &forgejov1.ForgejoRunner{}
	if err := r.reader().Get(ctx, key, live); err != nil {
		return nil, err
	}
	if live.ResourceVersion == cached.ResourceVersion {
		r.statusWrites.Delete(key)
		return cached, nil
	}
	return live, nil
}

// pass carries what one reconcile pass learned.
type pass struct {
	fr *forgejov1.ForgejoRunner
	// orig is fr as last read or written: status patches are computed against it.
	orig    *forgejov1.ForgejoRunner
	f       *forgejov1.Forgejo
	desired int32
	// tokens by index: every runner secret of "<name>-tokens", i.e. of every runner that is
	// wanted, recorded, being registered or has a pod, and of runners pruneTokens has not
	// dropped yet.
	tokens map[int32]string
	// tokensSecret is "<name>-tokens" as reconcileTokens wrote it.
	tokensSecret *corev1.Secret
	// api is nil when the instance's API cannot be used (apiErr says why).
	api    runner.API
	apiErr error
	// listed are Forgejo's runners by UUID, when refreshFromAPI could read them.
	listed map[string]runner.APIRunner
	// registering are the runners (index to UUID) a running registration Job registers.
	registering map[int32]string
	// online is known when Forgejo's list of runners was read.
	online      int32
	onlineKnown bool
	offline     []string
	// unregisterErr reports runners that could not be unregistered.
	unregisterErr error
	requeue       time.Duration
}

func (p *pass) wantRequeue(d time.Duration) {
	if p.requeue == 0 || d < p.requeue {
		p.requeue = d
	}
}

func (r *ForgejoRunnerReconciler) sync(ctx context.Context, p *pass) (ctrl.Result, error) {
	fr := p.fr
	f, err := r.instance(ctx, fr)
	if err != nil {
		return ctrl.Result{}, err
	}
	p.f = f
	runner.RunnersDesired.WithLabelValues(fr.Namespace, fr.Spec.ForgejoRef.Name, fr.Name).Set(float64(p.desired))

	if f == nil {
		msg := fmt.Sprintf("Forgejo %q does not exist in namespace %s (or is being deleted)", fr.Spec.ForgejoRef.Name, fr.Namespace)
		r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonForgejoNotFound, msg)
		r.setCondition(fr, forgejov1.ConditionReady, metav1.ConditionFalse, reasonForgejoNotFound, msg)
		if err := r.observeWorkload(ctx, fr); err != nil {
			return ctrl.Result{}, err
		}
		runner.ForgetAPIMetrics(fr.Namespace, fr.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if fr.Status.ForgejoUID != string(f.UID) {
		// Registrations recorded for another instance of the same name are not this one's.
		fr.Status.Runners = nil
		fr.Status.ForgejoUID = string(f.UID)
	}

	job, err := r.registerJob(ctx, fr)
	if err != nil {
		return ctrl.Result{}, err
	}
	pods, err := r.runnerPods(ctx, fr)
	if err != nil {
		return ctrl.Result{}, err
	}
	need := neededIndices(fr, p.desired, job, pods)
	if err := r.reconcileTokens(ctx, p, need); err != nil {
		return ctrl.Result{}, fmt.Errorf("runner secrets: %w", err)
	}
	configHash, cfgErr := r.reconcileConfig(ctx, fr, f, p.tokens, need)
	if cfgErr != nil && !errors.Is(cfgErr, errInvalidConfig) {
		return ctrl.Result{}, fmt.Errorf("runner configuration: %w", cfgErr)
	}

	// Forgejo's view first: it can show that recorded runners are gone and must be registered
	// again, and a registration Job that just finished is recorded before it.
	if collected, err := r.collectRegistration(ctx, p, job); err != nil {
		return ctrl.Result{}, err
	} else if collected {
		job = nil
	}
	if job != nil && metav1.IsControlledBy(job, fr) && job.DeletionTimestamp.IsZero() {
		if state, _ := backup.StateOf(job); state == backup.JobRunning {
			p.registering = runner.ParseRegistrationAnnotation(job.Annotations[runner.AnnotationRegistrationRunners])
		}
	}
	p.api, p.apiErr = r.api(ctx, f)
	if p.api != nil {
		r.refreshFromAPI(ctx, p)
	}
	r.unregisterStale(ctx, p, pods)
	if err := r.pruneTokens(ctx, p, job, pods); err != nil {
		return ctrl.Result{}, fmt.Errorf("runner secrets: %w", err)
	}
	registered, err := r.reconcileRegistration(ctx, p, job)
	if err != nil {
		return ctrl.Result{}, err
	}

	var sts *appsv1.StatefulSet
	if cfgErr == nil {
		if sts, err = r.reconcileStatefulSet(ctx, p, configHash); err != nil {
			return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
		}
	} else if err := r.observeWorkload(ctx, fr); err != nil {
		return ctrl.Result{}, err
	}
	r.reportStatus(ctx, p, sts, registered, cfgErr)

	if p.requeue == 0 {
		p.requeue = runnerResync
	}
	return ctrl.Result{RequeueAfter: p.requeue}, nil
}

// instance returns the referenced Forgejo, or nil when it is missing or being deleted.
func (r *ForgejoRunnerReconciler) instance(ctx context.Context, fr *forgejov1.ForgejoRunner) (*forgejov1.Forgejo, error) {
	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: fr.Namespace, Name: fr.Spec.ForgejoRef.Name}, f); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !f.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return f, nil
}

func (r *ForgejoRunnerReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ForgejoRunnerReconciler) setCondition(fr *forgejov1.ForgejoRunner, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&fr.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: fr.Generation,
	})
}

func (r *ForgejoRunnerReconciler) event(fr *forgejov1.ForgejoRunner, eventType, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(fr, nil, eventType, reason, action, note, args...)
	}
}

// registerJob returns the registration Job, or nil. A Job the cache does not have is looked
// up live: one created by the previous pass may not have reached the cache yet.
func (r *ForgejoRunnerReconciler) registerJob(ctx context.Context, fr *forgejov1.ForgejoRunner) (*batchv1.Job, error) {
	key := types.NamespacedName{Namespace: fr.Namespace, Name: runner.RegisterJobName(fr)}
	job := &batchv1.Job{}
	err := r.Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		err = r.reader().Get(ctx, key, job)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// runnerPods lists the runner pods by index.
func (r *ForgejoRunnerReconciler) runnerPods(ctx context.Context, fr *forgejov1.ForgejoRunner) (map[int32]*corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(fr.Namespace), client.MatchingLabels(runner.SelectorLabels(fr))); err != nil {
		return nil, fmt.Errorf("listing runner pods: %w", err)
	}
	out := map[int32]*corev1.Pod{}
	for i := range list.Items {
		pod := &list.Items[i]
		prefix := runner.StatefulSetName(fr) + "-"
		if !strings.HasPrefix(pod.Name, prefix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(pod.Name, prefix), 10, 32)
		if err != nil || n < 0 {
			continue
		}
		out[int32(n)] = pod
	}
	return out, nil
}

// neededIndices are the runners that need a secret and a configuration: the wanted ones, the
// recorded ones (until they are unregistered), those a registration Job registers and those
// with a pod. The secrets of the others are dropped by pruneTokens.
func neededIndices(fr *forgejov1.ForgejoRunner, desired int32, job *batchv1.Job, pods map[int32]*corev1.Pod) []int32 {
	set := map[int32]bool{}
	for i := range desired {
		set[i] = true
	}
	for _, rec := range fr.Status.Runners {
		set[rec.Index] = true
	}
	if job != nil && metav1.IsControlledBy(job, fr) {
		for i := range runner.ParseRegistrationAnnotation(job.Annotations[runner.AnnotationRegistrationRunners]) {
			set[i] = true
		}
	}
	for i := range pods {
		set[i] = true
	}
	out := make([]int32, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// reconcileTokens keeps the runner secrets in "<name>-tokens" (p.tokens): it generates one for
// every needed runner that has none and keeps every other valid one, which pruneTokens drops
// once Forgejo does not have its runner. A secret is generated once and kept: it is the
// runner's identity in Forgejo, and the only way to find a runner registered under it by a
// Job that failed half-way or whose result was never recorded.
func (r *ForgejoRunnerReconciler) reconcileTokens(ctx context.Context, p *pass, need []int32) error {
	fr := p.fr
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: runner.TokensSecretName(fr), Namespace: fr.Namespace}}
	tokens := map[int32]string{}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = mergeStringMaps(sec.Labels, runner.Labels(fr, forgejo.ComponentRunner))
		sec.Type = corev1.SecretTypeOpaque
		clear(tokens)
		data := map[string][]byte{}
		for key, v := range sec.Data {
			if i, ok := runner.IndexOfKey(key); ok && key == runner.TokenKey(i) && runner.ValidToken(string(v)) {
				data[key] = v
				tokens[i] = string(v)
			}
		}
		for _, i := range need {
			if _, ok := tokens[i]; ok {
				continue
			}
			tok, err := runner.NewToken()
			if err != nil {
				return err
			}
			data[runner.TokenKey(i)] = []byte(tok)
			tokens[i] = tok
		}
		sec.Data = data
		return controllerutil.SetControllerReference(fr, sec, r.Scheme)
	})
	if err != nil {
		return err
	}
	p.tokens, p.tokensSecret = tokens, sec
	return nil
}

// pruneTokens drops the secrets of the runners no longer needed (neededIndices after this
// pass's records and unregistrations) once Forgejo is known not to have them. Such a runner
// has no record, but a registration Job that failed after registering it, or that went before
// it was collected, may have left it in Forgejo: it is found in Forgejo's list by the UUID its
// secret gives and deleted first. While the list cannot be read, the secrets are kept.
func (r *ForgejoRunnerReconciler) pruneTokens(ctx context.Context, p *pass, job *batchv1.Job, pods map[int32]*corev1.Pod) error {
	fr := p.fr
	need := map[int32]bool{}
	for _, i := range neededIndices(fr, p.desired, job, pods) {
		need[i] = true
	}
	var orphans []int32
	for i := range p.tokens {
		if !need[i] {
			orphans = append(orphans, i)
		}
	}
	if len(orphans) == 0 || p.listed == nil {
		return nil
	}
	slices.Sort(orphans)
	var drop []int32
	var done []string
	var errs []error
	for _, i := range orphans {
		uuid, name := runner.UUIDOf(p.tokens[i]), runner.RunnerName(fr, i)
		if a, ok := p.listed[uuid]; ok {
			deleted, err := p.api.DeleteRunner(ctx, a.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			delete(p.listed, uuid)
			if deleted {
				done = append(done, fmt.Sprintf("%s (id %d)", name, a.ID))
				logf.FromContext(ctx).Info("Unregistered runner without a record", "runner", name, "uuid", uuid)
			}
		}
		drop = append(drop, i)
	}
	if len(done) > 0 {
		r.event(fr, corev1.EventTypeNormal, "Unregistered", "Unregister", "Unregistered runners %s from Forgejo %s",
			strings.Join(done, ", "), p.f.Name)
	}
	if len(errs) > 0 {
		p.unregisterErr = errors.Join(p.unregisterErr, errors.Join(errs...))
		p.wantRequeue(runnerBusyRequeue)
	}
	if len(drop) == 0 {
		return nil
	}
	sec := p.tokensSecret
	before := sec.DeepCopy()
	for _, i := range drop {
		delete(sec.Data, runner.TokenKey(i))
		delete(p.tokens, i)
	}
	// A merge patch that removes only these keys: nothing else writes them.
	return r.Patch(ctx, sec, client.MergeFrom(before))
}

var errInvalidConfig = errors.New("invalid spec.config")

// reconcileConfig writes the configuration file of every needed runner into "<name>-runner"
// and returns the digest of the shared configuration. A secret kept only until pruneTokens
// drops it gets no configuration.
func (r *ForgejoRunnerReconciler) reconcileConfig(ctx context.Context, fr *forgejov1.ForgejoRunner, f *forgejov1.Forgejo, tokens map[int32]string, need []int32) (string, error) {
	hash, err := runner.ConfigHash(fr, f)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidConfig, err)
	}
	data := map[string][]byte{}
	for _, i := range need {
		b, err := runner.RenderConfig(fr, f, i, runner.UUIDOf(tokens[i]))
		if err != nil {
			return "", fmt.Errorf("%w: %v", errInvalidConfig, err)
		}
		data[runner.ConfigKey(i)] = b
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: runner.ConfigSecretName(fr), Namespace: fr.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = mergeStringMaps(sec.Labels, runner.Labels(fr, forgejo.ComponentRunner))
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = data
		return controllerutil.SetControllerReference(fr, sec, r.Scheme)
	})
	return hash, err
}

// slotsFor returns the wanted runners in index order.
func slotsFor(fr *forgejov1.ForgejoRunner, desired int32, tokens map[int32]string) []runner.Slot {
	out := make([]runner.Slot, 0, desired)
	for i := range desired {
		out = append(out, runner.Slot{Index: i, Name: runner.RunnerName(fr, i), Token: tokens[i]})
	}
	return out
}

// record returns the status record of runner i.
func record(fr *forgejov1.ForgejoRunner, i int32) *forgejov1.RegisteredRunner {
	for k := range fr.Status.Runners {
		if fr.Status.Runners[k].Index == i {
			return &fr.Status.Runners[k]
		}
	}
	return nil
}

func removeRecord(fr *forgejov1.ForgejoRunner, i int32) {
	fr.Status.Runners = slices.DeleteFunc(fr.Status.Runners, func(rec forgejov1.RegisteredRunner) bool { return rec.Index == i })
}

// current reports whether runner slot s is registered as wanted (name, UUID and scope) and
// Forgejo still has it.
func current(fr *forgejov1.ForgejoRunner, s runner.Slot, scope string) bool {
	rec := record(fr, s.Index)
	return rec != nil && rec.Name == s.Name && rec.UUID == s.UUID() && rec.Scope == scope && !rec.Missing
}

// collectRegistration records the runners a succeeded registration Job registered and removes
// the Job; a failed Job is kept for registrationRetry so that its message stays visible.
//
// Until its runners are recorded, the Job is the only trace of their registration: the records
// are written to the status (optimistic lock) before the Job is deleted. A write that fails
// (another write to the ForgejoRunner, e.g. a scale, came in between) leaves the Job for the
// next pass, and nothing else of this pass (the StatefulSet) builds on the records.
func (r *ForgejoRunnerReconciler) collectRegistration(ctx context.Context, p *pass, job *batchv1.Job) (bool, error) {
	fr := p.fr
	if job == nil || !metav1.IsControlledBy(job, fr) || !job.DeletionTimestamp.IsZero() {
		return false, nil
	}
	state, _ := backup.StateOf(job)
	if state != backup.JobSucceeded {
		return false, nil
	}
	scope := job.Annotations[runner.AnnotationRegistrationScope]
	now := metav1.NewTime(r.now())
	var names []string
	for i, uuid := range runner.ParseRegistrationAnnotation(job.Annotations[runner.AnnotationRegistrationRunners]) {
		name := runner.RunnerName(fr, i)
		rec := record(fr, i)
		switch {
		case rec != nil && rec.UUID != uuid:
			// The record of an older secret stays until unregisterStale removes it; the
			// runner is registered again with the current secret afterwards.
			continue
		case rec != nil && rec.Name == name && rec.Scope == scope && !rec.Missing:
			continue // recorded already (by an earlier pass whose Job removal failed)
		case rec != nil && rec.Missing:
			// Registered again after Forgejo lost it: same UUID, new id.
			rec.Name, rec.Scope, rec.RegisteredTime = name, scope, &now
			rec.Missing, rec.ID, rec.Status = false, 0, ""
		case rec != nil:
			rec.Name, rec.Scope, rec.RegisteredTime = name, scope, &now
		default:
			fr.Status.Runners = append(fr.Status.Runners, forgejov1.RegisteredRunner{
				Index: i, Name: name, UUID: uuid, Scope: scope, RegisteredTime: &now,
			})
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		slices.SortFunc(fr.Status.Runners, func(a, b forgejov1.RegisteredRunner) int { return int(a.Index - b.Index) })
		if err := r.patchStatus(ctx, p); err != nil {
			return false, fmt.Errorf("recording the runners of Job %s: %w", job.Name, err)
		}
		slices.Sort(names)
		r.event(fr, corev1.EventTypeNormal, "Registered", "Register", "Registered runners %s with Forgejo %s for %s",
			strings.Join(names, ", "), p.f.Name, scopeName(scope))
		logf.FromContext(ctx).Info("Registered runners", "runners", names, "scope", scopeName(scope))
	}
	return true, backup.RemoveJob(ctx, r.Client, job)
}

func scopeName(scope string) string {
	switch {
	case scope == "":
		return "the whole instance"
	case strings.Contains(scope, "/"):
		return "repository " + scope
	}
	return "organization " + scope
}

// api returns a client of the instance's API, authenticated as the instance's admin.
func (r *ForgejoRunnerReconciler) api(ctx context.Context, f *forgejov1.Forgejo) (runner.API, error) {
	name := core.AdminCredentialsSecret(f)
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, sec); err != nil {
		return nil, fmt.Errorf("reading the admin credentials (Secret %s): %w", name, err)
	}
	password := string(sec.Data[core.AdminPasswordKey(f)])
	if password == "" {
		return nil, fmt.Errorf("secret %s has no key %q", name, core.AdminPasswordKey(f))
	}
	newAPI := r.NewAPI
	if newAPI == nil {
		newAPI = runner.NewAPI
	}
	return newAPI(forgejo.InternalHTTPURL(f), core.AdminUsername(f), password), nil
}

// refreshFromAPI reads the runners from Forgejo: IDs, online state, and whether recorded
// runners still exist (a runner deleted in Forgejo, or a restored database, is registered
// again). It also counts the jobs that wait for these runners.
//
// A recorded runner the list lacks is asked for by its id before it is taken for missing (a
// list of several pages can miss runners). A missing runner keeps its record, flagged, so
// that its pod and the pods after it keep running while it is registered again under the
// same UUID.
func (r *ForgejoRunnerReconciler) refreshFromAPI(ctx context.Context, p *pass) {
	fr := p.fr
	list, err := p.api.ListRunners(ctx)
	if err != nil {
		p.apiErr = err
		return
	}
	listed := make(map[string]runner.APIRunner, len(list))
	for _, a := range list {
		listed[a.UUID] = a
	}
	var missing []string
	for k := range fr.Status.Runners {
		rec := &fr.Status.Runners[k]
		a, ok := listed[rec.UUID]
		if !ok && rec.ID != 0 {
			got, err := p.api.GetRunner(ctx, rec.ID)
			if err != nil {
				p.apiErr = err
				return
			}
			if got != nil && got.UUID == rec.UUID {
				a, ok = *got, true
				listed[rec.UUID] = a
			}
		}
		switch {
		case ok:
			rec.ID, rec.Status = a.ID, a.Status
			if _, inFlight := p.registering[rec.Index]; rec.Missing && !inFlight {
				// Back without this operator's Job (a restored database): the grace starts again.
				now := metav1.NewTime(r.now())
				rec.Missing, rec.RegisteredTime = false, &now
			} // else the running Job registered it again; it is recorded when the Job is collected
			if a.Online() {
				p.online++
				continue
			}
		case rec.Missing:
			// Known already; being registered again.
		case rec.RegisteredTime != nil && r.now().Sub(rec.RegisteredTime.Time) < missingGrace:
			// Just registered: never registered again in a loop.
		default:
			rec.Missing, rec.ID, rec.Status = true, 0, ""
			if rec.Index < p.desired {
				missing = append(missing, rec.Name)
			}
		}
		if rec.Index < p.desired {
			p.offline = append(p.offline, rec.Name)
		}
	}
	p.listed = listed
	p.onlineKnown = true
	if len(missing) > 0 {
		r.event(fr, corev1.EventTypeWarning, "RunnerMissing", "Register",
			"Runners %s are no longer registered in Forgejo %s; registering them again", strings.Join(missing, ", "), p.f.Name)
	}
	runner.RunnersOnline.WithLabelValues(fr.Namespace, p.f.Name, fr.Name).Set(float64(p.online))
	waiting, err := p.api.WaitingJobs(ctx, runner.Scope(fr), runner.LabelNames(runner.EffectiveLabels(fr)))
	if err != nil {
		logf.FromContext(ctx).V(1).Info("Could not count waiting jobs", "error", err.Error())
		runner.RunnerJobsWaiting.DeletePartialMatch(map[string]string{"namespace": fr.Namespace, "runner": fr.Name})
		return
	}
	runner.RunnerJobsWaiting.WithLabelValues(fr.Namespace, p.f.Name, fr.Name).Set(float64(waiting))
}

// unregisterStale unregisters the recorded runners that are no longer wanted (their index is
// scaled away and their pod is gone, or stuck Terminating, see podStopWait) or whose secret
// was replaced (a new UUID).
//
// A runner is deleted by its id (from Forgejo's list, else the recorded one): Forgejo answers
// 404 for one that is gone already, so a list that missed it does not leave it behind. A
// runner without a known id is gone when the list lacks it after missingGrace.
func (r *ForgejoRunnerReconciler) unregisterStale(ctx context.Context, p *pass, pods map[int32]*corev1.Pod) {
	fr := p.fr
	var stale []forgejov1.RegisteredRunner
	stuck := map[int32]*corev1.Pod{}
	for _, rec := range fr.Status.Runners {
		switch {
		case rec.Index >= p.desired:
			if pod, exists := pods[rec.Index]; exists {
				if wait := r.podStopWait(pod); wait > 0 {
					p.wantRequeue(wait) // the pod is still finishing its jobs
					continue
				}
				stuck[rec.Index] = pod
			}
			stale = append(stale, rec)
		case rec.UUID != runner.UUIDOf(p.tokens[rec.Index]):
			stale = append(stale, rec)
		}
	}
	if len(stale) == 0 {
		return
	}
	p.wantRequeue(runnerBusyRequeue)
	if p.api == nil {
		p.unregisterErr = fmt.Errorf("cannot unregister %s: %w", recordNames(stale), p.apiErr)
		return
	}
	listed := p.listed
	if listed == nil {
		list, err := p.api.ListRunners(ctx)
		if err != nil {
			p.unregisterErr = fmt.Errorf("cannot unregister %s: %w", recordNames(stale), err)
			return
		}
		listed = make(map[string]runner.APIRunner, len(list))
		for _, a := range list {
			listed[a.UUID] = a
		}
	}
	var errs []error
	var done []string
	for _, rec := range stale {
		id := rec.ID
		if a, ok := listed[rec.UUID]; ok {
			id = a.ID
		}
		if id != 0 {
			deleted, err := p.api.DeleteRunner(ctx, id)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rec.Name, err))
				continue
			}
			if deleted {
				done = append(done, fmt.Sprintf("%s (id %d)", rec.Name, id))
			}
		} else if !rec.Missing && rec.RegisteredTime != nil && r.now().Sub(rec.RegisteredTime.Time) < missingGrace {
			continue // registered a moment ago and not listed yet: wait for its id
		}
		removeRecord(fr, rec.Index)
		if listed != nil {
			delete(listed, rec.UUID) // Forgejo no longer has it (see pruneTokens)
		}
		logf.FromContext(ctx).Info("Unregistered runner", "runner", rec.Name, "uuid", rec.UUID)
		if pod := stuck[rec.Index]; pod != nil {
			node := pod.Spec.NodeName
			if node == "" {
				node = "(none)"
			}
			r.event(fr, corev1.EventTypeWarning, "RunnerPodStuck", "Unregister",
				"Pod %s on node %s has been stuck Terminating for %s after its grace period; unregistered its runner %s from Forgejo %s",
				pod.Name, node, r.now().Sub(pod.DeletionTimestamp.Time).Round(time.Second), rec.Name, p.f.Name)
		}
	}
	if len(done) > 0 {
		r.event(fr, corev1.EventTypeNormal, "Unregistered", "Unregister", "Unregistered runners %s from Forgejo %s",
			strings.Join(done, ", "), p.f.Name)
	}
	if len(errs) > 0 {
		p.unregisterErr = errors.Join(errs...)
	}
}

// podStopWait is how much longer the pod of a scaled-away runner is waited for before the
// runner is unregistered anyway (0: not at all). A pod that is not being deleted yet, or that
// stops within its grace period, may be finishing a job. Its deletionTimestamp already
// includes the grace period: a pod still there podStopMargin later is stuck (e.g. on a lost
// node, until someone force-deletes it), as in finalize. The pod's deletion is watched, so a
// stopping pod is checked again at the deadline or at the next resync, whichever is first.
func (r *ForgejoRunnerReconciler) podStopWait(pod *corev1.Pod) time.Duration {
	if pod.DeletionTimestamp.IsZero() {
		return runnerBusyRequeue
	}
	left := pod.DeletionTimestamp.Add(podStopMargin).Sub(r.now())
	if left <= 0 {
		return 0
	}
	return min(left, runnerResync)
}

func recordNames(recs []forgejov1.RegisteredRunner) string {
	names := make([]string, 0, len(recs))
	for _, rec := range recs {
		names = append(names, rec.Name)
	}
	return strings.Join(names, ", ")
}

// registrationBlocked says why the instance cannot register runners now ("" when it can):
// the registration Job runs the instance's image against its database, so the instance must
// run a verified image and not be in maintenance or an upgrade.
func registrationBlocked(f *forgejov1.Forgejo) string {
	switch {
	case f.Spec.Paused:
		return "is paused"
	case f.Status.Image == "":
		return "has not rolled out its image yet"
	case forgejo.DatabaseUpgradeInFlight(f):
		// forgejo-cli writes to the database, which the upgrade backs up, then stops.
		return "is upgrading PostgreSQL"
	case f.Status.Phase == forgejov1.PhaseUpgrading:
		return "is upgrading"
	case f.Status.Phase == forgejov1.PhaseRestoring:
		return "is being restored"
	case !meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionReady):
		return "is not Ready"
	}
	return ""
}

// reconcileRegistration starts the registration Job for the wanted runners that are not
// registered as wanted and reports the Registered condition. It returns true when every
// wanted runner is registered.
func (r *ForgejoRunnerReconciler) reconcileRegistration(ctx context.Context, p *pass, job *batchv1.Job) (bool, error) {
	fr, f := p.fr, p.f
	scope := runner.Scope(fr)
	var pending []runner.Slot
	for _, s := range slotsFor(fr, p.desired, p.tokens) {
		if !current(fr, s, scope) {
			// Registering writes the runner's version: pass on the one it declared (a
			// scope change re-registers runners whose pods keep running). A new or lost
			// runner has none, and declares it when its pod connects.
			s.Version = p.listed[s.UUID()].Version
			pending = append(pending, s)
		}
	}
	registeredCount := 0
	for _, rec := range fr.Status.Runners {
		if !rec.Missing {
			registeredCount++
		}
	}
	runner.RunnersRegistered.WithLabelValues(fr.Namespace, f.Name, fr.Name).Set(float64(registeredCount))

	if job != nil {
		if !metav1.IsControlledBy(job, fr) {
			msg := backup.CheckOwned(job, fr).Error()
			r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistrationFailed, msg)
			p.wantRequeue(runnerBusyRequeue)
			return false, nil
		}
		if !job.DeletionTimestamp.IsZero() {
			p.wantRequeue(runnerBusyRequeue)
			r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistering, "Waiting for the previous registration Job to go away")
			return false, nil
		}
		switch state, reason := backup.StateOf(job); state {
		case backup.JobRunning:
			p.wantRequeue(runnerBusyRequeue)
			r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistering,
				fmt.Sprintf("Job %s registers runners with Forgejo %s", job.Name, f.Name))
			return false, nil
		case backup.JobFailed:
			msg := r.registrationFailure(ctx, job, reason)
			if c := meta.FindStatusCondition(fr.Status.Conditions, forgejov1.ConditionRunnerRegistered); c == nil || c.Message != msg {
				r.event(fr, corev1.EventTypeWarning, "RegistrationFailed", "Register", "%s", msg)
			}
			r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistrationFailed, msg)
			finished := job.CreationTimestamp.Time
			for _, c := range job.Status.Conditions {
				if c.Type == batchv1.JobFailed {
					finished = c.LastTransitionTime.Time
				}
			}
			if wait := registrationRetry - r.now().Sub(finished); wait > 0 {
				p.wantRequeue(wait)
				return false, nil
			}
			if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
				return false, err
			}
			p.wantRequeue(runnerBusyRequeue)
			return false, nil
		}
		// A succeeded Job was collected and is being deleted.
		p.wantRequeue(runnerBusyRequeue)
		return false, nil
	}

	if len(pending) == 0 {
		msg := fmt.Sprintf("%s registered with Forgejo %s for %s", runners(int(p.desired)), f.Name, runner.ScopeDescription(fr))
		if p.desired == 0 {
			msg = "No runners wanted (spec.replicas is 0)"
		}
		r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionTrue, reasonRegistered, msg)
		return true, nil
	}
	if why := registrationBlocked(f); why != "" {
		r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonWaitingForForgejo,
			fmt.Sprintf("Waiting to register %s: Forgejo %s %s", runners(len(pending)), f.Name, why))
		p.wantRequeue(30 * time.Second)
		return false, nil
	}
	newJob := runner.RegisterJob(runner.RegisterJobInput{
		Runner: fr, Forgejo: f, Image: f.Status.Image, AppIniSecret: r.appIniSecret(ctx, f), Slots: pending,
	})
	if err := controllerutil.SetControllerReference(fr, newJob, r.Scheme); err != nil {
		return false, err
	}
	if err := backup.CreateJob(ctx, r.Client, fr, newJob); err != nil {
		var conflict *backup.JobConflictError
		if errors.As(err, &conflict) {
			r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistrationFailed, conflict.Error())
			p.wantRequeue(runnerBusyRequeue)
			return false, nil
		}
		return false, fmt.Errorf("creating the registration job: %w", err)
	}
	names := make([]string, 0, len(pending))
	for _, s := range pending {
		names = append(names, s.Name)
	}
	logf.FromContext(ctx).Info("Created registration Job", "job", newJob.Name, "runners", names)
	r.setCondition(fr, forgejov1.ConditionRunnerRegistered, metav1.ConditionFalse, reasonRegistering,
		fmt.Sprintf("Job %s registers runners %s with Forgejo %s", newJob.Name, strings.Join(names, ", "), f.Name))
	p.wantRequeue(runnerBusyRequeue)
	return false, nil
}

// registrationFailure builds the message of a failed registration Job from its pods'
// termination messages (the script masks secrets in them).
func (r *ForgejoRunnerReconciler) registrationFailure(ctx context.Context, job *batchv1.Job, reason string) string {
	msg := fmt.Sprintf("Registration Job %s failed", job.Name)
	if reason != "" {
		msg += " (" + reason + ")"
	}
	pods, err := backup.JobPodsByName(ctx, r.Client, job.Namespace, job.Name)
	if err != nil {
		return msg
	}
	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && strings.TrimSpace(t.Message) != "" {
				return msg + ": " + strings.TrimSpace(t.Message)
			}
		}
	}
	return msg
}

// appIniSecret is the Secret the instance's pods read app.ini from: "<name>-config", or the
// last known good copy while the config guard serves it.
func (r *ForgejoRunnerReconciler) appIniSecret(ctx context.Context, f *forgejov1.Forgejo) string {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err == nil {
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Name == "config" && v.Secret != nil && v.Secret.SecretName != "" {
				return v.Secret.SecretName
			}
		}
	}
	return forgejo.ConfigSecretName(f)
}

// registeredPrefix is the number of runner pods to run: wanted runners 0, 1, ... up to the
// first one that is not registered under its current secret (a pod without a registration
// would only be refused by Forgejo). A pending scope change keeps the pods running, and so
// does a runner Forgejo lost (Missing): it is registered again under the same UUID, which its
// pod's configuration names already.
func registeredPrefix(p *pass) int32 {
	n := int32(0)
	for i := range p.desired {
		rec := record(p.fr, i)
		if rec == nil || rec.UUID != runner.UUIDOf(p.tokens[i]) {
			break
		}
		n++
	}
	return n
}

func (r *ForgejoRunnerReconciler) reconcileStatefulSet(ctx context.Context, p *pass, configHash string) (*appsv1.StatefulSet, error) {
	fr := p.fr
	replicas := registeredPrefix(p)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: runner.StatefulSetName(fr), Namespace: fr.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		runner.MutateStatefulSet(sts, runner.WorkloadInput{
			Runner: fr, Replicas: replicas, ConfigHash: configHash,
		})
		return controllerutil.SetControllerReference(fr, sts, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	fr.Status.ConfigHash = configHash
	fr.Status.Image = runner.Image(fr)
	r.setWorkloadStatus(fr, sts)
	if sts.Status.ObservedGeneration < sts.Generation || sts.Status.UpdatedReplicas < replicas || sts.Status.ReadyReplicas < replicas || sts.Status.Replicas != replicas {
		p.wantRequeue(runnerBusyRequeue)
	}
	return sts, nil
}

// observeWorkload reports the StatefulSet as it is, without changing it.
func (r *ForgejoRunnerReconciler) observeWorkload(ctx context.Context, fr *forgejov1.ForgejoRunner) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: fr.Namespace, Name: runner.StatefulSetName(fr)}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			fr.Status.Replicas, fr.Status.ReadyReplicas = 0, 0
			fr.Status.Selector = labels.SelectorFromSet(runner.SelectorLabels(fr)).String()
			return nil
		}
		return err
	}
	r.setWorkloadStatus(fr, sts)
	return nil
}

func (r *ForgejoRunnerReconciler) setWorkloadStatus(fr *forgejov1.ForgejoRunner, sts *appsv1.StatefulSet) {
	fr.Status.Replicas = sts.Status.Replicas
	fr.Status.ReadyReplicas = sts.Status.ReadyReplicas
	fr.Status.Selector = labels.SelectorFromSet(runner.SelectorLabels(fr)).String()
}

// podSecurityBlocked says why the namespace refuses the Docker executor's privileged pod
// ("" when it does not, or cannot be read). Only the namespace label is checked: a
// cluster-wide default level (AdmissionConfiguration) is not visible to the operator.
func (r *ForgejoRunnerReconciler) podSecurityBlocked(ctx context.Context, fr *forgejov1.ForgejoRunner) string {
	if runner.Executor(fr) != forgejov1.RunnerExecutorDocker {
		return ""
	}
	ns := &corev1.Namespace{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: fr.Namespace}, ns); err != nil {
		logf.FromContext(ctx).V(1).Info("Could not read the namespace for its Pod Security level", "error", err.Error())
		return ""
	}
	switch level := ns.Labels[podSecurityEnforceLabel]; level {
	case "baseline", "restricted":
		return fmt.Sprintf("Namespace %s enforces the %q Pod Security Standard (label %s), which refuses the privileged "+
			"Docker daemon of the Docker executor: label the namespace %s=privileged, or use executor Host",
			fr.Namespace, level, podSecurityEnforceLabel, podSecurityEnforceLabel)
	}
	return ""
}

// actionsDisabled reports whether spec.config turns Forgejo Actions off ([actions] ENABLED,
// true by default on Forgejo 15 and 17, so the operator renders nothing for it).
func actionsDisabled(f *forgejov1.Forgejo) bool {
	for section, kv := range f.Spec.Config {
		if !strings.EqualFold(section, "actions") {
			continue
		}
		for k, v := range kv {
			if strings.EqualFold(k, "ENABLED") {
				if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil && !b {
					return true
				}
			}
		}
	}
	return false
}

// reportStatus sets Ready and Degraded.
func (r *ForgejoRunnerReconciler) reportStatus(ctx context.Context, p *pass, sts *appsv1.StatefulSet, registered bool, cfgErr error) {
	fr := p.fr
	if p.onlineKnown {
		fr.Status.OnlineRunners = p.online
	} else {
		fr.Status.OnlineRunners = 0
		runner.ForgetAPIMetrics(fr.Namespace, fr.Name)
	}
	ready := func(s metav1.ConditionStatus, reason, msg string) {
		r.setCondition(fr, forgejov1.ConditionReady, s, reason, msg)
	}
	blocked := r.podSecurityBlocked(ctx, fr)
	regCond := meta.FindStatusCondition(fr.Status.Conditions, forgejov1.ConditionRunnerRegistered)
	readyPods := int32(0)
	if sts != nil {
		readyPods = sts.Status.ReadyReplicas
	}
	switch {
	case cfgErr != nil:
		ready(metav1.ConditionFalse, reasonInvalidConfig, cfgErr.Error())
	case actionsDisabled(p.f):
		ready(metav1.ConditionFalse, reasonActionsDisabled, fmt.Sprintf("Forgejo %s has Actions turned off "+
			"(spec.config.actions.ENABLED); the runners cannot take jobs", p.f.Name))
	case blocked != "" && p.desired > 0 && readyPods < p.desired:
		if c := meta.FindStatusCondition(fr.Status.Conditions, forgejov1.ConditionReady); c == nil || c.Reason != reasonPodSecurityBlocked {
			r.event(fr, corev1.EventTypeWarning, reasonPodSecurityBlocked, "Deploy", "%s", blocked)
		}
		ready(metav1.ConditionFalse, reasonPodSecurityBlocked, blocked)
	case p.desired == 0:
		ready(metav1.ConditionTrue, reasonScaledToZero, "spec.replicas is 0")
	case !registered && regCond != nil:
		ready(metav1.ConditionFalse, regCond.Reason, regCond.Message)
	case readyPods < p.desired:
		ready(metav1.ConditionFalse, reasonPodsNotReady, fmt.Sprintf("%d of %d runner pods are ready", readyPods, p.desired))
	case p.onlineKnown && len(p.offline) > 0:
		ready(metav1.ConditionFalse, reasonRunnersOffline,
			fmt.Sprintf("Forgejo reports %d of %s online; offline: %s", p.online, runners(int(p.desired)), strings.Join(p.offline, ", ")))
	case p.onlineKnown:
		ready(metav1.ConditionTrue, reasonRunnersOnline, fmt.Sprintf("%s online", runners(int(p.online))))
	default:
		ready(metav1.ConditionTrue, reasonPodsReady, fmt.Sprintf("%d of %d runner pods are ready; whether they are online is unknown "+
			"because the operator cannot read Forgejo's API (see Degraded)", readyPods, p.desired))
	}
	if c := meta.FindStatusCondition(fr.Status.Conditions, forgejov1.ConditionReady); c != nil && c.Status != metav1.ConditionTrue {
		p.wantRequeue(runnerBusyRequeue)
	}

	degraded := func(s metav1.ConditionStatus, reason, msg string) {
		prev := meta.FindStatusCondition(fr.Status.Conditions, forgejov1.ConditionDegraded)
		if s == metav1.ConditionTrue && (prev == nil || prev.Status != s || prev.Message != msg) {
			r.event(fr, corev1.EventTypeWarning, reason, "Reconcile", "%s", msg)
		}
		r.setCondition(fr, forgejov1.ConditionDegraded, s, reason, msg)
	}
	switch {
	case p.unregisterErr != nil:
		degraded(metav1.ConditionTrue, reasonUnregisterFailed, "Could not unregister runners from Forgejo: "+p.unregisterErr.Error())
	case p.apiErr != nil:
		degraded(metav1.ConditionTrue, reasonAPIUnavailable, fmt.Sprintf("Cannot use Forgejo's API as the instance's admin "+
			"(%s/%s): runners cannot be unregistered and their online state is unknown: %v", p.f.Name, core.AdminUsername(p.f), p.apiErr))
	default:
		degraded(metav1.ConditionFalse, reasonAsExpected, "")
	}
}

// finalize stops the runner pods, unregisters every runner from Forgejo and releases the
// ForgejoRunner. Runners that still cannot be unregistered unregisterGiveUp after the first
// failed attempt are named in a Warning event and left for the administrator.
func (r *ForgejoRunnerReconciler) finalize(ctx context.Context, fr *forgejov1.ForgejoRunner) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(fr, forgejo.FinalizerName) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)
	key := client.ObjectKeyFromObject(fr)
	since := r.now().Sub(fr.DeletionTimestamp.Time)

	// Stop the runners first: a runner that is still connected could take a job, and a
	// registration Job could register a runner after it was unregistered.
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: fr.Namespace, Name: runner.StatefulSetName(fr)}, sts); err == nil {
		if metav1.IsControlledBy(sts, fr) && sts.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, sts, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if job, err := r.registerJob(ctx, fr); err != nil {
		return ctrl.Result{}, err
	} else if job != nil && metav1.IsControlledBy(job, fr) && job.DeletionTimestamp.IsZero() {
		if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
			return ctrl.Result{}, err
		}
	}
	waitPods := time.Duration(runner.GracePeriodSeconds(fr))*time.Second + podStopMargin
	if remaining, err := r.remainingPods(ctx, fr); err != nil {
		return ctrl.Result{}, err
	} else if remaining > 0 && since < waitPods {
		log.V(1).Info("Waiting for runner pods to stop", "pods", remaining)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	f, err := r.instance(ctx, fr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if f != nil && string(f.UID) == fr.Status.ForgejoUID {
		if err := r.unregisterAll(ctx, fr, f); err != nil {
			// The budget starts with the first failed attempt, not with the deletion: waiting
			// for the pods may take longer than the budget.
			first, _ := r.unregisterFailing.LoadOrStore(key, r.now())
			if r.now().Sub(first.(time.Time)) < unregisterGiveUp {
				log.Info("Could not unregister runners yet", "error", err.Error())
				return ctrl.Result{RequeueAfter: runnerBusyRequeue}, nil
			}
			r.event(fr, corev1.EventTypeWarning, reasonUnregisterFailed, "Unregister",
				"Gave up unregistering runners %s from Forgejo %s after %s: %v; delete them in Site administration > Actions > Runners",
				recordNames(fr.Status.Runners), f.Name, unregisterGiveUp, err)
		}
	}
	r.unregisterFailing.Delete(key)

	runner.ForgetMetrics(fr.Namespace, fr.Name)
	patch := client.MergeFrom(fr.DeepCopy())
	controllerutil.RemoveFinalizer(fr, forgejo.FinalizerName)
	return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, fr, patch))
}

// remainingPods counts the runner and registration pods that still exist.
func (r *ForgejoRunnerReconciler) remainingPods(ctx context.Context, fr *forgejov1.ForgejoRunner) (int, error) {
	n := 0
	for _, component := range []string{forgejo.ComponentRunner, forgejo.ComponentRunnerRegistration} {
		list := &corev1.PodList{}
		sel := client.MatchingLabels{forgejo.LabelName: forgejo.AppName, forgejo.LabelInstance: fr.Name, forgejo.LabelComponent: component}
		if err := r.reader().List(ctx, list, client.InNamespace(fr.Namespace), sel); err != nil {
			return 0, fmt.Errorf("listing pods: %w", err)
		}
		n += len(list.Items)
	}
	return n, nil
}

// unregisterAll deletes from Forgejo every recorded runner, and every runner whose UUID one of
// the ForgejoRunner's secrets gives (registered by a Job whose result was never recorded).
//
// Recorded runners are deleted by their id, which needs no list (Forgejo answers 404 for one
// that is gone). The others are found in Forgejo's list, which is read again after deleting
// until it shows none of them: a list of several pages can miss a runner.
func (r *ForgejoRunnerReconciler) unregisterAll(ctx context.Context, fr *forgejov1.ForgejoRunner, f *forgejov1.Forgejo) error {
	api, err := r.api(ctx, f)
	if err != nil {
		return err
	}
	ours := map[string]string{}
	for _, rec := range fr.Status.Runners {
		ours[rec.UUID] = rec.Name
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: fr.Namespace, Name: runner.TokensSecretName(fr)}, sec); err == nil {
		for key, tok := range sec.Data {
			if i, ok := runner.IndexOfKey(key); ok && runner.ValidToken(string(tok)) {
				if _, known := ours[runner.UUIDOf(string(tok))]; !known {
					ours[runner.UUIDOf(string(tok))] = runner.RunnerName(fr, i)
				}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	var errs []error
	var done []string
	unregister := func(name string, id int64) {
		deleted, err := api.DeleteRunner(ctx, id)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		case deleted:
			done = append(done, fmt.Sprintf("%s (id %d)", name, id))
			logf.FromContext(ctx).Info("Unregistered runner", "runner", name, "id", id)
		}
	}
	for _, rec := range fr.Status.Runners {
		if rec.ID != 0 && !rec.Missing {
			unregister(rec.Name, rec.ID)
		}
	}
	for round := 0; len(errs) == 0; round++ {
		if round == 3 {
			errs = append(errs, errors.New("runners of this ForgejoRunner were still listed by Forgejo after deleting them three times"))
			break
		}
		list, err := api.ListRunners(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		found := false
		for _, a := range list {
			if name, ok := ours[a.UUID]; ok {
				found = true
				unregister(name, a.ID)
			}
		}
		if !found {
			break
		}
	}
	if len(done) > 0 {
		slices.Sort(done)
		r.event(fr, corev1.EventTypeNormal, "Unregistered", "Unregister", "Unregistered runners %s from Forgejo %s",
			strings.Join(done, ", "), f.Name)
	}
	return errors.Join(errs...)
}

// runners is "1 runner" or "<n> runners".
func runners(n int) string {
	if n == 1 {
		return "1 runner"
	}
	return fmt.Sprintf("%d runners", n)
}

func mergeStringMaps(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// SetupWithManager sets up the controller with the Manager. StatefulSets, Jobs, Pods and
// Secrets are cached only with the operator's labels (forgejo.CacheScopes), which every
// object of a ForgejoRunner carries.
func (r *ForgejoRunnerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&forgejov1.ForgejoRunner{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			deletionStarted(),
		))).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Secret{}).
		// Pods: a runner pod that is gone lets its runner be unregistered after a scale-down.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapRunnerPod)).
		Watches(&forgejov1.Forgejo{}, handler.EnqueueRequestsFromMapFunc(r.mapForgejoToRunners),
			builder.WithPredicates(forgejoAvailabilityChanged())).
		WithOptions(controller.Options{RateLimiter: rateLimiter()}).
		Named("forgejorunner").
		Complete(r)
}

func deletionStarted() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func mapRunnerPod(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[forgejo.LabelComponent] != forgejo.ComponentRunner || l[forgejo.LabelRunner] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: l[forgejo.LabelRunner]}}}
}

func (r *ForgejoRunnerReconciler) mapForgejoToRunners(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &forgejov1.ForgejoRunnerList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, fr := range list.Items {
		if fr.Spec.ForgejoRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: fr.Namespace, Name: fr.Name}})
		}
	}
	return out
}

// forgejoAvailabilityChanged passes the changes of a Forgejo that matter to its runners:
// readiness (registration waits for it), the running image (the registration Job runs it),
// its spec and its deletion.
func forgejoAvailabilityChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*forgejov1.Forgejo)
			n, ok2 := e.ObjectNew.(*forgejov1.Forgejo)
			if !ok1 || !ok2 {
				return true
			}
			return o.Generation != n.Generation || o.Status.Image != n.Status.Image || o.Status.Phase != n.Status.Phase ||
				o.DeletionTimestamp.IsZero() != n.DeletionTimestamp.IsZero() ||
				meta.IsStatusConditionTrue(o.Status.Conditions, forgejov1.ConditionReady) != meta.IsStatusConditionTrue(n.Status.Conditions, forgejov1.ConditionReady)
		},
	}
}
