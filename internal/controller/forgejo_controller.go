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
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/autopilot"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/core"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/observability"
	"github.com/lictl/forgejo-operator/internal/upgrade"
)

const (
	stepSecrets          = "secrets"
	stepDatabase         = "database"
	stepHighAvailability = "high-availability"
	stepStorage          = "storage"
	stepSSHHostKeys      = "ssh-host-keys"
	stepTuning           = "tuning"
	stepConfig           = "config"
	stepWorkload         = "workload"
	stepNetwork          = "network"
	stepGateway          = "gateway"
	stepTLS              = "tls"
	stepMonitoring       = "monitoring"
	stepHealth           = "health"
	stepAutopilot        = "autopilot"
)

// defaultResync bounds how long the operator goes without re-checking an instance, which
// drives health probing, statistics sampling and autopilot decisions.
const defaultResync = 60 * time.Second

const (
	// restoreLiveCheckWindow is how long after a ForgejoRestore failed the restore guard
	// double-checks for its Job with a live read, in case the cache had not caught up with it.
	restoreLiveCheckWindow = 5 * time.Minute
	// restoreGuardRequeue is how often a held maintenance re-checks the restore workload.
	restoreGuardRequeue = 5 * time.Second
)

// ForgejoReconciler reconciles a Forgejo object by running an ordered list of steps,
// each owned by a feature package.
type ForgejoReconciler struct {
	client.Client
	// APIReader is an uncached reader (mgr.GetAPIReader()); it defaults to Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// WatchNamespaces restricts the informers this controller starts outside the manager's
	// cache (CNPG credential Secrets) like the manager's --watch-namespace; empty means all.
	WatchNamespaces []string

	// statusWrites maps an instance (types.NamespacedName) to the resourceVersion of this
	// controller's last status write, until the cache has caught up with it (see getInstance).
	statusWrites sync.Map
}

// namedStep is one pipeline step and the earlier steps it depends on.
type namedStep struct {
	name string
	fn   forgejo.Step
	// needs names the earlier steps whose outputs (on the ReconcileContext or in the
	// cluster) this step consumes. The step runs only when all of them completed in this
	// pass, i.e. returned neither an error nor Stop; otherwise it is skipped. A step without
	// needs runs on every pass, so a failure elsewhere cannot starve it.
	needs []string
}

// Step names referenced by the orchestrator itself.
const (
	stepMaintenance      = "maintenance"
	stepRestoreGuard     = "restore-guard"
	stepUpgrade          = "upgrade"
	stepBackupSchedule   = "backup-schedule"
	stepBackupOwnership  = "backup-ownership"
	stepStorageAutopilot = "storage-autopilot"
	stepObjectStorage    = "object-storage"
	stepRestoreIdentity  = "restore-identity"
	stepRestoreFrom      = "restore-from"
)

// steps run in order. maintenance → restore-guard → secrets → database → high-availability
// → storage → upgrade → tuning → config → config-guard → workload form a chain (which
// ssh-host-keys joins at config): each produces outputs the next ones consume, so a failure or
// Stop skips the rest of the chain
// (e.g. no app.ini without the database, no rollout onto an unready Valkey). The other steps
// only need part of the chain, or nothing, and keep running when an unrelated step fails, so
// that one rejected Ingress or a Valkey outage does not also stop scheduled backups, health
// checks, the restore handshake or volume expansion.
var steps = []namedStep{
	// Maintenance comes first: it only reads ForgejoRestores, and a restore must be able to
	// take the instance down while the database or Valkey is unavailable.
	{name: stepMaintenance, fn: backup.ReconcileMaintenance},
	{name: stepRestoreGuard, fn: reconcileRestoreGuard, needs: []string{stepMaintenance}},
	// spec.restoreFrom: the identity Secrets come from the backup, before the secrets and
	// ssh-host-keys steps would generate new ones (a no-op otherwise).
	{name: stepRestoreIdentity, fn: backup.ReconcileRestoreIdentity, needs: []string{stepRestoreGuard}},
	{name: stepSecrets, fn: core.ReconcileSecrets, needs: []string{stepRestoreIdentity}},
	// Owner references of the ForgejoBackups and the backup PVC follow
	// spec.storage.retainPolicy. It reads only the instance and its backups, so a database
	// or Valkey that is down must not delay it (the instance may be deleted meanwhile).
	{name: stepBackupOwnership, fn: backup.ReconcileOwnership},
	{name: stepDatabase, fn: core.ReconcileDatabase, needs: []string{stepSecrets}},
	{name: stepHighAvailability, fn: autopilot.ReconcileHA, needs: []string{stepDatabase}},
	{name: stepStorage, fn: core.ReconcileStorage, needs: []string{stepHighAvailability}},
	// spec.restoreFrom: once the identity is restored and the database and the data volume
	// exist, a ForgejoRestore restores them from the backup.
	{name: stepRestoreFrom, fn: backup.ReconcileBootstrap, needs: []string{stepStorage}},
	// The SSH host keys are identity, like the secrets: independent of the database, Valkey
	// and storage. The config step consumes them; the upgrade step waits while they are
	// unavailable (rc.SSHHostKeysUnavailable), so it runs first.
	{name: stepSSHHostKeys, fn: core.ReconcileSSHHostKeys, needs: []string{stepSecrets}},
	// spec.objectStorage: its Secrets (which pods reference) and the bucket check
	// (ObjectStorageReady). It stops only while the Secrets are unusable, which no pod could
	// start without; an unreachable bucket is only reported. No prerequisites, so the
	// condition stays current while the database is down.
	{name: stepObjectStorage, fn: core.ReconcileObjectStorage},
	{name: stepUpgrade, fn: upgrade.Reconcile, needs: []string{stepStorage, stepObjectStorage}},
	{name: stepTuning, fn: autopilot.ReconcileTuning, needs: []string{stepUpgrade}},
	{name: stepConfig, fn: core.ReconcileConfig, needs: []string{stepTuning, stepSSHHostKeys}},
	{name: "config-guard", fn: autopilot.ReconcileConfigGuard, needs: []string{stepConfig}},
	{name: stepWorkload, fn: core.ReconcileWorkload, needs: []string{"config-guard"}},
	// Spec-only objects.
	{name: stepNetwork, fn: core.ReconcileNetwork},
	// Optional integrations (Gateway API, cert-manager): created only when their CRDs exist;
	// their status becomes RoutesReady and TLSReady. Independent of everything else.
	{name: stepGateway, fn: core.ReconcileGateway},
	{name: stepTLS, fn: core.ReconcileTLS},
	// rc.Maintenance, rc.Upgrading, rc.HPAManaged and rc.ResourcesOverride.
	{name: "scaling", fn: autopilot.ReconcileScaling, needs: []string{stepTuning}},
	{name: stepMonitoring, fn: observability.ReconcileMonitoring},
	// Creates and prunes ForgejoBackup objects; the backup controller itself waits for the
	// database. rc.Maintenance comes from the guard; rc.Upgrading from the upgrade step, or
	// from status.upgrade when that step was skipped.
	{name: stepBackupSchedule, fn: backup.ReconcileSchedule, needs: []string{stepRestoreGuard}},
	// rc.Maintenance suspends probing during a restore.
	{name: stepHealth, fn: observability.ReconcileHealth, needs: []string{stepRestoreGuard}},
	// Self-healing, LKG bookkeeping and right-sizing must never act on a partial pass.
	{name: stepAutopilot, fn: autopilot.Reconcile, needs: []string{stepWorkload, stepHealth}},
	// Volume measurement, StorageHealthy and PVC auto-expansion. No needs: a full CNPG
	// volume is a common reason why the database step stops, and growing it must not wait
	// for that step. Last, so that it sees this pass's rc.Maintenance, rc.Upgrading and
	// rc.RestoreMigrating, and its file-system-resize restart follows the workload update.
	{name: stepStorageAutopilot, fn: autopilot.ReconcileStorage},
}

// pipelineResult is the outcome of one pass over the steps.
type pipelineResult struct {
	// failed maps each step that returned an error to it, in step order in failedOrder.
	failed      map[string]error
	failedOrder []string
	// stopped names the steps that returned Stop.
	stopped []string
	// completed holds the steps that returned neither an error nor Stop.
	completed map[string]bool
	// skipped maps each skipped step to the prerequisite that did not complete.
	skipped map[string]string
	// conflict is set when a step hit an optimistic-lock conflict; the pass was cut short.
	conflict bool
	// terminating is set when a step could not create an object because the namespace is
	// being deleted; the pass was cut short.
	terminating bool
}

// err joins the step errors, naming each step.
func (p *pipelineResult) err() error {
	errs := make([]error, 0, len(p.failedOrder))
	for _, name := range p.failedOrder {
		errs = append(errs, fmt.Errorf("step %q: %w", name, p.failed[name]))
	}
	return errors.Join(errs...)
}

// blockedBy explains why a step did not complete: its own error, or the earliest
// prerequisite (transitively) that failed or stopped.
func (p *pipelineResult) blockedBy(name string) string {
	for range steps {
		if err, ok := p.failed[name]; ok {
			return fmt.Sprintf("step %q failed: %v", name, err)
		}
		if slices.Contains(p.stopped, name) {
			return fmt.Sprintf("step %q is waiting (see its condition)", name)
		}
		dep, ok := p.skipped[name]
		if !ok {
			break
		}
		name = dep
	}
	return fmt.Sprintf("step %q did not run", name)
}

// runPipeline runs steps in order, skipping those whose prerequisites did not complete. A
// conflict ends the pass: the next one, a second later, starts from fresh reads. So does a
// create refused because the namespace is being deleted: every later create would fail the
// same way (see forgejo.IsNamespaceTerminating).
func runPipeline(ctx context.Context, rc *forgejo.ReconcileContext, steps []namedStep) *pipelineResult {
	log := logf.FromContext(ctx)
	p := &pipelineResult{failed: map[string]error{}, completed: map[string]bool{}, skipped: map[string]string{}}
	for _, s := range steps {
		if missing := firstMissing(s.needs, p.completed); missing != "" {
			p.skipped[s.name] = missing
			if s.name == stepUpgrade {
				inferUpgrading(rc)
			}
			continue
		}
		res, err := s.fn(ctx, rc)
		if apierrors.IsConflict(err) {
			// A child changed after our cached read; retry soon instead of reporting an error.
			log.V(1).Info("conflict updating a child, retrying", "step", s.name, "error", err.Error())
			p.conflict = true
			rc.Requeue(time.Second)
			return p
		}
		if forgejo.IsNamespaceTerminating(err) {
			log.V(1).Info("Stopped reconcile pass because the namespace is terminating", "step", s.name, "error", err.Error())
			p.terminating = true
			return p
		}
		if err != nil {
			p.failed[s.name] = err
			p.failedOrder = append(p.failedOrder, s.name)
			log.Error(err, "reconcile step failed", "step", s.name)
			if s.name == stepUpgrade {
				inferUpgrading(rc)
			}
			continue
		}
		rc.Requeue(res.RequeueAfter)
		if res.Stop {
			log.V(1).Info("step is waiting; the steps that depend on it are skipped", "step", s.name)
			p.stopped = append(p.stopped, s.name)
			continue
		}
		p.completed[s.name] = true
	}
	if len(p.skipped) > 0 {
		log.V(1).Info("steps skipped because a prerequisite did not complete", "skipped", p.skipped)
	}
	return p
}

// inferUpgrading stands in for the upgrade step when it did not run (or failed): the
// independent steps after it (backup schedule, status) must not take an instance with an
// upgrade in flight for an idle one, e.g. start a scheduled backup in the middle of it.
func inferUpgrading(rc *forgejo.ReconcileContext) {
	rc.Upgrading = rc.Upgrading || forgejo.UpgradeInFlight(rc.Instance)
}

func firstMissing(needs []string, completed map[string]bool) string {
	for _, n := range needs {
		if !completed[n] {
			return n
		}
	}
	return ""
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos/status,verbs=patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch

// Reconcile drives a Forgejo instance towards its spec.
func (r *ForgejoReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	instance, err := r.getInstance(ctx, req.NamespacedName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.statusWrites.Delete(req.NamespacedName)
			forgetInstance(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// No API call: the last-success gauge is back right after an operator restart, also
	// while the backup-schedule step is skipped or the instance is paused (the backup
	// staleness alerts depend on it).
	backup.ExportLastSuccess(instance)
	backup.ExportLastUpload(instance)

	original := instance.DeepCopy()
	rc := &forgejo.ReconcileContext{
		Client:     r.Client,
		APIReader:  r.reader(),
		Scheme:     r.Scheme,
		Recorder:   r.Recorder,
		RESTMapper: r.RESTMapper(),
		Instance:   instance,
	}
	rc.TargetImage = forgejo.Image(instance)
	if instance.Status.Image != "" {
		rc.TargetImage = instance.Status.Image
	}
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.ConfigSecretName = forgejo.ConfigSecretName(instance)

	if instance.Spec.Paused {
		instance.Status.Phase = forgejov1.PhasePaused
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionFalse, "Paused", "Reconciliation is paused by spec.paused")
		// ReconcileStatus, which normally exports these, does not run while paused. The
		// alerts' expected-down selector matches instance_info{phase="Paused"}, and a missing
		// instance_ready (e.g. after an operator restart) would fire
		// ForgejoOperatorMetricsMissing.
		forgejo.SetInstanceInfo(instance, rc.TargetImage)
		exportInstanceReady(instance)
		upgrade.ExportVersionSupport(instance, time.Now())
		return r.finish(ctx, original, instance, ctrl.Result{}, nil)
	}

	p := runPipeline(ctx, rc, steps)
	if p.terminating {
		// The namespace is being deleted, and the instance with it. As kube-controller-manager
		// does, this is not an error: no Degraded condition, Warning event (the API server
		// would reject it anyway), status write or backoff, and no requeue, since the
		// namespace never becomes active again. The operator does not look the Namespace up
		// before a pass: that would take a cluster-wide Namespace watch (or a read per pass)
		// and RBAC on namespaces, also under --watch-namespace, and a cached Terminating
		// phase can lag behind, so the refused create is the signal it relies on anyway.
		return ctrl.Result{}, nil
	}
	stepErr := p.err()
	if stepErr != nil {
		msg := make([]string, 0, len(p.failedOrder))
		for _, name := range p.failedOrder {
			msg = append(msg, fmt.Sprintf("step %q: %v", name, p.failed[name]))
			rc.Event(corev1.EventTypeWarning, "ReconcileError", "Reconcile", "step %s failed: %v", name, p.failed[name])
		}
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, "ReconcileError", strings.Join(msg, "; "))
	} else if c := meta.FindStatusCondition(instance.Status.Conditions, forgejov1.ConditionDegraded); !p.conflict && c != nil && c.Reason == "ReconcileError" {
		// Only clear our own Degraded; other features report Degraded with their own reasons.
		meta.RemoveStatusCondition(&instance.Status.Conditions, forgejov1.ConditionDegraded)
	}
	if !p.conflict && !p.completed[stepBackupSchedule] && instance.Spec.Backup.Enabled {
		// Without this, BackupHealthy would keep its last value (often True) while no
		// scheduled backup is created and retention does not run.
		rc.SetCondition(forgejov1.ConditionBackupHealthy, metav1.ConditionUnknown, "ReconcileBlocked",
			"Scheduled backups are not being created: "+p.blockedBy(stepBackupSchedule))
	}

	if _, err := core.ReconcileStatus(ctx, rc); err != nil {
		log.Error(err, "status step failed")
		stepErr = errors.Join(stepErr, fmt.Errorf("status: %w", err))
	}
	// After the status step, which verifies status.image.
	upgrade.ReportVersionSupport(rc, time.Now())
	instance.Status.ObservedGeneration = instance.Generation

	rc.Requeue(defaultResync)
	return r.finish(ctx, original, instance, ctrl.Result{RequeueAfter: rc.RequeueAfter}, stepErr)
}

// finish persists the status and returns the reconcile result: stepErr (which makes the
// controller back off) when the status was saved, a quick retry on a conflict, and nothing
// when the instance was deleted during the pass.
func (r *ForgejoReconciler) finish(ctx context.Context, original, instance *forgejov1.Forgejo,
	res ctrl.Result, stepErr error) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, original, instance); err != nil {
		switch {
		case apierrors.IsConflict(err):
			return ctrl.Result{Requeue: true}, nil
		case apierrors.IsNotFound(err):
			// Deleted while this pass ran (its children are garbage-collected); the delete
			// event's reconcile finds it gone and drops its metrics.
			logf.FromContext(ctx).V(1).Info("instance was deleted during the reconcile")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.statusWrites.Store(client.ObjectKeyFromObject(instance), instance.ResourceVersion)
	if stepErr != nil {
		return ctrl.Result{}, stepErr
	}
	return res, nil
}

// reader returns the live API reader (the cache in tests that do not set one).
func (r *ForgejoReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// getInstance reads the instance from the cache, or live while the cache has not caught up
// with this controller's last status write. A pass usually follows the previous one within
// milliseconds (the children that pass changed queue it), before the watch delivers that
// write. Starting from the older status, it would repeat the previous pass's decisions: the
// optimistic lock refuses its status patch, but the events it emitted for a status change
// (UnsupportedVersion, PreUpgradeBackupCompleted, UpgradeRollingOut, ...) would go out a
// second time. The cache is used once it holds that write, or whatever is live now.
func (r *ForgejoReconciler) getInstance(ctx context.Context, key types.NamespacedName) (*forgejov1.Forgejo, error) {
	cached := &forgejov1.Forgejo{}
	if err := r.Get(ctx, key, cached); err != nil {
		return nil, err
	}
	written, ok := r.statusWrites.Load(key)
	if !ok {
		return cached, nil
	}
	if written == cached.ResourceVersion {
		r.statusWrites.Delete(key)
		return cached, nil
	}
	live := &forgejov1.Forgejo{}
	if err := r.reader().Get(ctx, key, live); err != nil {
		return nil, err
	}
	if live.ResourceVersion == cached.ResourceVersion {
		// Written again by someone else since, and the cache already has that.
		r.statusWrites.Delete(key)
		return cached, nil
	}
	return live, nil
}

func (r *ForgejoReconciler) patchStatus(ctx context.Context, original, instance *forgejov1.Forgejo) error {
	// The optimistic lock keeps a reconcile working from a stale cache from overwriting
	// status written by a newer one; conflicts are requeued by the caller.
	return r.Status().Patch(ctx, instance, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

// forgetInstance drops every metric series and the in-memory bookkeeping of an instance
// that no longer exists.
func forgetInstance(namespace, name string) {
	forgejo.DeleteInstanceMetrics(namespace, name)
	backup.ForgetLastSuccess(namespace, name)
	backup.ForgetRemote(namespace, name)
	observability.ForgetInstance(namespace, name)
	core.ForgetInstance(namespace, name)
	core.ForgetObjectStorage(namespace, name)
}

// exportInstanceReady sets forgejo_operator_instance_ready from the Ready condition, as
// core.ReconcileStatus does when the pipeline runs.
func exportInstanceReady(f *forgejov1.Forgejo) {
	v := 0.0
	if meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionReady) {
		v = 1
	}
	forgejo.InstanceReady.WithLabelValues(f.Namespace, f.Name).Set(v)
}

// reconcileRestoreGuard keeps the instance in maintenance while a restore Job of this
// instance still runs although no ForgejoRestore holds maintenance any more, e.g. when the
// restore was marked failed while its Job kept running, or the Job was orphaned. Starting
// Forgejo then would run migrations and serve traffic on a database and volume that
// pg_restore and the archive extraction are still rewriting.
func reconcileRestoreGuard(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	if rc.Maintenance {
		return forgejo.StepResult{}, nil
	}
	f := rc.Instance
	running, err := runningRestoreWorkload(ctx, rc.Client, f)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if running == "" {
		// A restore that failed moments ago may have done so because the cache had not yet
		// seen its freshly created Job; confirm with a live read before scaling up.
		recent, err := restoreFailedRecently(ctx, rc.Client, f)
		if err != nil {
			return forgejo.StepResult{}, err
		}
		if recent {
			if running, err = runningRestoreWorkload(ctx, rc.APIReader, f); err != nil {
				return forgejo.StepResult{}, err
			}
		}
	}
	if running == "" {
		return forgejo.StepResult{}, nil
	}

	rc.Maintenance = true
	rc.MaintenanceReason = fmt.Sprintf("%s is still running although no ForgejoRestore is active; "+
		"Forgejo stays scaled down until it finishes", running)
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionProgressing); c == nil || c.Message != rc.MaintenanceReason {
		rc.Event(corev1.EventTypeWarning, "RestoreJobStillRunning", "Restore", "%s", rc.MaintenanceReason)
	}
	return forgejo.StepResult{RequeueAfter: restoreGuardRequeue}, nil
}

// runningRestoreWorkload names the first restore Job or pod of the instance that has not
// finished, or returns "" when there is none.
func runningRestoreWorkload(ctx context.Context, r client.Reader, f *forgejov1.Forgejo) (string, error) {
	sel := client.MatchingLabels{
		forgejo.LabelName:      forgejo.AppName,
		forgejo.LabelInstance:  f.Name,
		forgejo.LabelComponent: forgejo.ComponentRestore,
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(f.Namespace), sel); err != nil {
		return "", fmt.Errorf("listing restore jobs: %w", err)
	}
	for i := range jobs.Items {
		if !jobFinished(&jobs.Items[i]) {
			return "restore job " + jobs.Items[i].Name, nil
		}
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), sel); err != nil {
		return "", fmt.Errorf("listing restore pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			return "restore pod " + p.Name, nil
		}
	}
	return "", nil
}

func jobFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// restoreFailedRecently reports whether a ForgejoRestore of the instance failed within
// restoreLiveCheckWindow.
func restoreFailedRecently(ctx context.Context, c client.Client, f *forgejov1.Forgejo) (bool, error) {
	restores, err := backup.ListRestores(ctx, c, f.Namespace, f.Name)
	if err != nil {
		return false, fmt.Errorf("listing restores: %w", err)
	}
	for _, rs := range restores {
		if rs.Status.Phase == forgejov1.RestorePhaseFailed && rs.Status.CompletionTime != nil &&
			time.Since(rs.Status.CompletionTime.Time) < restoreLiveCheckWindow {
			return true, nil
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// Secrets, ConfigMaps, Pods, Jobs, Deployments and Services are cached only when they carry
// the operator's labels (forgejo.CacheScopes, see cmd/main.go), so the watches below see
// only the operator's own objects of these kinds.
func (r *ForgejoReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&forgejov1.Forgejo{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&forgejov1.ForgejoBackup{}, handler.EnqueueRequestsFromMapFunc(mapBackupToForgejo)).
		Watches(&forgejov1.ForgejoRestore{}, handler.EnqueueRequestsFromMapFunc(mapRestoreToForgejo)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapByInstanceLabel),
			builder.WithPredicates(managedByOperator())).
		WithOptions(controller.Options{RateLimiter: rateLimiter()}).
		Named("forgejo")

	// CloudNativePG is optional at startup; watch its Clusters and app secrets when installed.
	if _, err := mgr.GetRESTMapper().RESTMapping(forgejo.CNPGClusterGVK.GroupKind(), forgejo.CNPGClusterGVK.Version); err == nil {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
		// Map by label rather than owner: under retainPolicy Retain the cluster has no owner reference.
		b = b.Watches(cluster, handler.EnqueueRequestsFromMapFunc(mapByInstanceLabel), builder.WithPredicates(managedByOperator()))
	} else {
		mgr.GetLogger().Info("CloudNativePG CRDs not found; managed databases will be unavailable until the operator restarts")
	}

	// CNPG-generated credential secrets are owned by the CNPG Cluster and carry none of our
	// labels, so the manager's cache never holds them. A separate cache keeps just the
	// metadata of Secrets labelled cnpg.io/cluster, enough to map a password rotation back
	// to its instance.
	secrets, err := r.newCNPGSecretCache(mgr)
	if err != nil {
		return err
	}
	if err := mgr.Add(secrets); err != nil {
		return err
	}
	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	b = b.WatchesRawSource(source.Kind(secrets.Cache, client.Object(secret),
		handler.EnqueueRequestsFromMapFunc(mapCNPGSecretToForgejo)))

	return b.Complete(r)
}

// rateLimiter is controller-runtime's default with the per-instance backoff capped at
// defaultResync: a step that keeps failing must not push time-based work (scheduled backups,
// health checks, storage expansion) out by up to the default 1000s.
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, defaultResync),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// cacheRunnable lets the manager start a cache with its own caches, before leader election,
// and wait for it to sync.
type cacheRunnable struct{ cache.Cache }

func (c cacheRunnable) GetCache() cache.Cache { return c.Cache }

func (r *ForgejoReconciler) newCNPGSecretCache(mgr ctrl.Manager) (cacheRunnable, error) {
	hasCluster, err := labels.NewRequirement("cnpg.io/cluster", selection.Exists, nil)
	if err != nil {
		return cacheRunnable{}, err
	}
	opts := cache.Options{
		HTTPClient:           mgr.GetHTTPClient(),
		Scheme:               mgr.GetScheme(),
		Mapper:               mgr.GetRESTMapper(),
		DefaultLabelSelector: labels.NewSelector().Add(*hasCluster),
		DefaultTransform:     cache.TransformStripManagedFields(),
	}
	for _, ns := range r.WatchNamespaces {
		if opts.DefaultNamespaces == nil {
			opts.DefaultNamespaces = map[string]cache.Config{}
		}
		opts.DefaultNamespaces[ns] = cache.Config{}
	}
	c, err := cache.New(mgr.GetConfig(), opts)
	if err != nil {
		return cacheRunnable{}, fmt.Errorf("creating the CNPG secret cache: %w", err)
	}
	return cacheRunnable{c}, nil
}

func mapBackupToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*forgejov1.ForgejoBackup)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}}}
}

func mapRestoreToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	r, ok := obj.(*forgejov1.ForgejoRestore)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.Spec.ForgejoRef.Name}}}
}

func mapByInstanceLabel(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[forgejo.LabelInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// mapCNPGSecretToForgejo maps "<forgejo>-db-app" (labelled cnpg.io/cluster=<forgejo>-db)
// back to the Forgejo instance so password rotations re-render app.ini.
func mapCNPGSecretToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	cluster := obj.GetLabels()["cnpg.io/cluster"]
	if cluster == "" || !strings.HasSuffix(cluster, "-db") {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: strings.TrimSuffix(cluster, "-db")}}}
}

// managedByOperator selects the operator's objects that belong to a Forgejo instance. The
// pods and Jobs of a ForgejoRunner carry the runner's name as their instance label and are
// left to the ForgejoRunner controller.
func managedByOperator() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		switch l[forgejo.LabelComponent] {
		case forgejo.ComponentRunner, forgejo.ComponentRunnerRegistration:
			return false
		}
		return l[forgejo.LabelName] == forgejo.AppName && l[forgejo.LabelInstance] != ""
	})
}
