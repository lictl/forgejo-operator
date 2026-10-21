package forgejo

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// DBConnection is the resolved PostgreSQL connection used to render app.ini and
// to run backup/restore jobs.
type DBConnection struct {
	Host     string
	Port     int32
	Name     string
	User     string
	Password string
	SSLMode  string
	// SecretName holds the credentials (CNPG "<cluster>-app" secret or the external secret).
	SecretName  string
	UsernameKey string
	PasswordKey string
}

// ReconcileContext is shared by all reconcile steps of one Forgejo reconciliation.
// Steps read the outputs of earlier steps and record their own outputs here. The
// orchestrator persists Instance.Status once, after all steps have run.
type ReconcileContext struct {
	Client client.Client
	// APIReader reads straight from the API server, bypassing the informer cache. Use it
	// only where acting on a stale cache would be unsafe; it is never nil.
	APIReader  client.Reader
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
	RESTMapper meta.RESTMapper

	// Instance is the Forgejo being reconciled. Steps mutate Instance.Status only.
	Instance *forgejov1.Forgejo
	// Layout of the image currently being deployed (TargetImage).
	Layout ImageLayout

	// --- outputs of earlier steps ---

	// DB is set by the database step once credentials are known.
	DB *DBConnection
	// DatabaseReady is set by the database step.
	DatabaseReady bool
	// ValkeyURL (redis://host:port) is set by the HA step when highAvailability is enabled.
	ValkeyURL string
	// Maintenance is set by the maintenance step while a ForgejoRestore is active;
	// the workload is scaled to zero.
	Maintenance       bool
	MaintenanceReason string
	// RestoreMigrating is set by the maintenance step while a restored database waits for
	// its first pod to migrate it (restore in ScalingUp). The workload runs a single pod with
	// the Recreate strategy and the HPA is held back until that pod is available.
	RestoreMigrating bool
	// TargetImage is the image the workload step must deploy. Set by the upgrade step;
	// defaults to Status.Image (if set) or the spec image.
	TargetImage string
	// Upgrading is true while the upgrade step is orchestrating an image change.
	// The workload uses the Recreate strategy and a single replica while it is set.
	Upgrading bool
	// Tuning holds app.ini overrides from auto-tuning as section -> key -> value. User
	// spec.config wins over these.
	Tuning map[string]map[string]string
	// TuningEnv are extra env vars from auto-tuning (e.g. GOMEMLIMIT).
	TuningEnv []corev1.EnvVar
	// ResourcesOverride replaces spec.resources when vertical autoscaling is active.
	ResourcesOverride *corev1.ResourceRequirements
	// ConfigSecretName is the Secret mounted as app.ini (normally "<name>-config"; the
	// self-healing guard may switch it to the last-known-good copy).
	ConfigSecretName string
	// ConfigHash of the app.ini that will be deployed.
	ConfigHash string
	// DesiredConfigHash is the hash rendered from the current spec (may differ from
	// ConfigHash while a rollback is active).
	DesiredConfigHash string
	// ReplicasOverride, when set, replaces the computed replica count.
	ReplicasOverride *int32
	// HPAManaged is true when an HPA owns the replica count.
	HPAManaged bool

	// RequeueAfter is the shortest requeue any step asked for.
	RequeueAfter time.Duration
}

// StepResult tells the orchestrator how to continue.
type StepResult struct {
	// Stop ends this reconciliation after status is persisted (e.g. waiting for a dependency).
	Stop bool
	// RequeueAfter asks for another reconciliation.
	RequeueAfter time.Duration
}

// Step is one unit of reconciliation for a Forgejo instance.
type Step func(ctx context.Context, rc *ReconcileContext) (StepResult, error)

// Requeue records a requeue request, keeping the shortest one.
func (rc *ReconcileContext) Requeue(d time.Duration) {
	if d <= 0 {
		return
	}
	if rc.RequeueAfter == 0 || d < rc.RequeueAfter {
		rc.RequeueAfter = d
	}
}

// SetCondition sets a status condition on the instance.
func (rc *ReconcileContext) SetCondition(condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rc.Instance.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rc.Instance.Generation,
	})
}

// Event records a Kubernetes event on the instance.
func (rc *ReconcileContext) Event(eventType, reason, action, note string, args ...any) {
	if rc.Recorder == nil {
		return
	}
	rc.Recorder.Eventf(rc.Instance, nil, eventType, reason, action, note, args...)
}

// RecordAutoPilotAction appends to status.autoPilot.recentActions (newest first, max 10),
// emits an event and increments the actions metric.
func (rc *ReconcileContext) RecordAutoPilotAction(actionType, message string) {
	st := &rc.Instance.Status
	if st.AutoPilot == nil {
		st.AutoPilot = &forgejov1.AutoPilotStatus{}
	}
	act := forgejov1.AutoPilotAction{Time: metav1.Now(), Type: actionType, Message: message}
	st.AutoPilot.RecentActions = append([]forgejov1.AutoPilotAction{act}, st.AutoPilot.RecentActions...)
	if len(st.AutoPilot.RecentActions) > 10 {
		st.AutoPilot.RecentActions = st.AutoPilot.RecentActions[:10]
	}
	rc.Event(corev1.EventTypeNormal, actionType, "AutoPilot", "%s", message)
	AutoPilotActions.WithLabelValues(rc.Instance.Namespace, rc.Instance.Name, actionType).Inc()
}

// OwnerRefsEnabled reports whether data-bearing children (PVCs, CNPG cluster) should be
// garbage-collected with the instance.
func (rc *ReconcileContext) OwnerRefsEnabled() bool { return !RetainData(rc.Instance) }
