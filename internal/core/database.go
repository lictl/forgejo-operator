package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	cnpgHealthyPhase = "Cluster in healthy state"
	// annotationManagedPGParams remembers which postgresql.parameters came from the spec,
	// so that removed ones are dropped without touching parameters set by others.
	annotationManagedPGParams = "forgejo.lucas.icu/managed-pg-parameters"
	dbNotReadyRequeue         = 10 * time.Second
	// cnpgDegradedRequeue re-checks a cluster that serves from its primary while replicas
	// are missing (the Cluster watch reports most changes sooner).
	cnpgDegradedRequeue   = 30 * time.Second
	reasonClusterDegraded = "ClusterDegraded"
)

// ReconcileDatabase ensures the CNPG Cluster (or validates the external DB secret) and
// sets rc.DB / rc.DatabaseReady and status.database. Stops the reconcile until ready.
func ReconcileDatabase(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	if forgejo.DatabaseType(rc.Instance) == forgejov1.DatabaseTypeExternal {
		return reconcileExternalDB(ctx, rc)
	}
	return reconcileCNPG(ctx, rc)
}

func dbNotReady(rc *forgejo.ReconcileContext, reason, msg string, requeue time.Duration) (forgejo.StepResult, error) {
	rc.DatabaseReady = false
	rc.SetCondition(forgejov1.ConditionDatabaseReady, metav1.ConditionFalse, reason, msg)
	return forgejo.StepResult{Stop: true, RequeueAfter: requeue}, nil
}

func reconcileExternalDB(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	st := &forgejov1.DatabaseStatus{Type: forgejov1.DatabaseTypeExternal}
	if ext := f.Spec.Database.External; ext != nil {
		st.Host = ext.Host
		st.SecretName = ext.CredentialsSecret.Name
	}
	f.Status.Database = st

	db, err := forgejo.ResolveDB(ctx, rc.Client, f)
	if err != nil {
		return dbNotReady(rc, "CredentialsUnavailable", err.Error(), 30*time.Second)
	}
	// Forgejo resolves the host from its own namespace; the operator may run elsewhere.
	// db.Host itself goes into app.ini and the backup jobs unchanged.
	port := strconv.Itoa(int(db.Port))
	var errs []string
	addr := ""
	for _, host := range probeHosts(db.Host, f.Namespace) {
		a := net.JoinHostPort(host, port)
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", a)
		if err != nil {
			errs = append(errs, err.Error())
			// Only a name that does not exist moves on; a name that resolved is the target.
			if dnsErr := (*net.DNSError)(nil); errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				continue
			}
			break
		}
		_ = conn.Close()
		addr = a
		break
	}
	if addr == "" {
		st.Phase = "Unreachable"
		return dbNotReady(rc, "Unreachable", fmt.Sprintf("cannot connect to %s: %s",
			net.JoinHostPort(db.Host, port), strings.Join(errs, "; ")), 30*time.Second)
	}

	st.Phase = "Reachable"
	rc.DB = db
	rc.DatabaseReady = true
	rc.SetCondition(forgejov1.ConditionDatabaseReady, metav1.ConditionTrue, "Reachable", "External PostgreSQL at "+addr+" is reachable")
	return forgejo.StepResult{}, nil
}

// probeHosts lists the names to probe for an external database host, in the order a pod in
// namespace resolves them: a single-label name is a Service in that namespace first
// ("<host>.<ns>.svc" resolves through the pod's search path whatever the cluster domain;
// the cluster.local form serves an operator running outside the cluster), then the name as
// given (node or corporate search domains).
func probeHosts(host, namespace string) []string {
	if net.ParseIP(host) != nil || strings.Contains(host, ".") || strings.EqualFold(host, "localhost") {
		return []string{host}
	}
	svc := host + "." + namespace + ".svc"
	return []string{svc, svc + ".cluster.local", host}
}

func reconcileCNPG(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	name := forgejo.DatabaseClusterName(f)
	st := &forgejov1.DatabaseStatus{
		Type:        forgejov1.DatabaseTypeCNPG,
		ClusterName: name,
		Host:        forgejo.CNPGReadWriteService(f) + "." + f.Namespace + ".svc.cluster.local",
		SecretName:  forgejo.CNPGAppSecretName(f),
		Instances:   cnpgInstances(f),
	}
	f.Status.Database = st

	if !rc.HasAPI(forgejo.CNPGClusterGVK) {
		st.Phase = "CNPGNotInstalled"
		return dbNotReady(rc, "CNPGNotInstalled",
			"CloudNativePG (postgresql.cnpg.io/v1 Cluster) is not installed; install CNPG or use spec.database.type: External", time.Minute)
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	cluster.SetName(name)
	cluster.SetNamespace(f.Namespace)
	if err := apply(ctx, rc, cluster, rc.OwnerRefsEnabled(), func() error {
		return mutateCNPGCluster(f, cluster)
	}); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("CNPG cluster %s: %w", name, err)
	}

	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	current, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	target, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	st.ReadyInstances = int32(ready)
	st.Phase = phase
	if phase == "" {
		phase = "Creating"
	}
	summary := fmt.Sprintf("CNPG cluster %s: %s (%d/%d instances ready)", name, phase, ready, st.Instances)
	healthy := phase == cnpgHealthyPhase && int32(ready) >= st.Instances
	if !healthy && f.Status.Image == "" {
		// First install: wait for the whole cluster, as CNPG reports it.
		return dbNotReady(rc, "ClusterNotReady", summary, dbNotReadyRequeue)
	}
	if !healthy {
		// A lost replica must not stop the pipeline (and with it backups and restores) while
		// the primary serves Forgejo; only an unavailable primary does.
		if why, err := cnpgPrimaryUnavailable(ctx, rc, phase, current, target, ready); err != nil {
			return forgejo.StepResult{}, err
		} else if why != "" {
			return dbNotReady(rc, "ClusterNotReady", summary+": "+why, dbNotReadyRequeue)
		}
	}

	db, err := forgejo.ResolveDB(ctx, rc.Client, f)
	if err != nil {
		return dbNotReady(rc, "CredentialsUnavailable", err.Error(), dbNotReadyRequeue)
	}
	rc.DB = db
	rc.DatabaseReady = true
	if healthy {
		rc.SetCondition(forgejov1.ConditionDatabaseReady, metav1.ConditionTrue, "ClusterHealthy",
			fmt.Sprintf("CNPG cluster %s is healthy (%d/%d instances ready)", name, ready, st.Instances))
		return forgejo.StepResult{}, nil
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseReady); c == nil || c.Reason != reasonClusterDegraded {
		rc.Event(corev1.EventTypeWarning, "DatabaseDegraded", "Database",
			"%s; the primary %s serves Forgejo, backups and restores", summary, current)
	}
	rc.SetCondition(forgejov1.ConditionDatabaseReady, metav1.ConditionTrue, reasonClusterDegraded,
		summary+"; primary "+current+" is available")
	return forgejo.StepResult{RequeueAfter: cnpgDegradedRequeue}, nil
}

// cnpgPrimaryBlockedPhases are CNPG phases during which the primary does not accept
// connections (or is about to change).
var cnpgPrimaryBlockedPhases = map[string]bool{
	"Setting up primary":                                       true,
	"Failing over":                                             true,
	"Switchover in progress":                                   true,
	"Primary instance is being restarted in-place":             true,
	"Primary instance is being restarted without a switchover": true,
	"Upgrading Postgres major version":                         true,
}

// cnpgPrimaryUnavailable explains why the primary of a CNPG cluster that is not fully
// healthy cannot serve Forgejo, or returns "" when it can.
func cnpgPrimaryUnavailable(ctx context.Context, rc *forgejo.ReconcileContext, phase, current, target string, ready int64) (string, error) {
	switch {
	case current == "":
		return "no primary yet", nil
	case target != current:
		return fmt.Sprintf("primary changing from %s to %s", current, target), nil
	case cnpgPrimaryBlockedPhases[phase]:
		return "primary unavailable", nil
	case ready < 1:
		return "no instance ready", nil
	}
	pod := &corev1.Pod{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: rc.Instance.Namespace, Name: current}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "primary pod " + current + " not found", nil
		}
		return "", fmt.Errorf("primary pod %s: %w", current, err)
	}
	if !pod.DeletionTimestamp.IsZero() || !podReady(pod) {
		return "primary pod " + current + " is not ready", nil
	}
	return "", nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func cnpgInstances(f *forgejov1.Forgejo) int32 {
	if n := f.Spec.Database.CNPG.Instances; n > 0 {
		return n
	}
	return 1
}

// mutateCNPGCluster sets only the fields the operator manages, leaving CNPG's defaults alone.
func mutateCNPGCluster(f *forgejov1.Forgejo, c *unstructured.Unstructured) error {
	spec := f.Spec.Database.CNPG
	creating := c.GetResourceVersion() == ""
	c.SetLabels(mergeMaps(c.GetLabels(), forgejo.Labels(f, forgejo.ComponentDatabase)))
	obj := c.Object

	set := func(v any, path ...string) error {
		return unstructured.SetNestedField(obj, v, append([]string{"spec"}, path...)...)
	}
	if err := set(int64(cnpgInstances(f)), "instances"); err != nil {
		return err
	}
	if err := set(forgejo.PostgresImage(f), "imageName"); err != nil {
		return err
	}

	size := forgejo.QuantityOr(spec.Storage.Size, "5Gi")
	if cur, found, _ := unstructured.NestedString(obj, "spec", "storage", "size"); !found || quantityLess(cur, size.String()) {
		if err := set(size.String(), "storage", "size"); err != nil {
			return err
		}
	}

	if creating {
		// Bootstrap and storage class are immutable after creation.
		if spec.Storage.StorageClassName != nil {
			if err := set(*spec.Storage.StorageClassName, "storage", "storageClass"); err != nil {
				return err
			}
		}
		if err := set(map[string]any{
			"initdb": map[string]any{"database": forgejo.CNPGDatabaseName, "owner": forgejo.CNPGOwnerName},
		}, "bootstrap"); err != nil {
			return err
		}
	}

	res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec.Resources)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		unstructured.RemoveNestedField(obj, "spec", "resources")
	} else if err := set(res, "resources"); err != nil {
		return err
	}

	if err := mergePGParameters(c, spec.Parameters); err != nil {
		return err
	}

	if spec.EnablePodMonitor {
		return set(true, "monitoring", "enablePodMonitor")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj, "spec", "monitoring", "enablePodMonitor"); found {
		return set(false, "monitoring", "enablePodMonitor")
	}
	return nil
}

// mergePGParameters applies spec parameters on top of spec.postgresql.parameters and
// removes parameters the operator set earlier that are no longer in the spec.
func mergePGParameters(c *unstructured.Unstructured, want map[string]string) error {
	params, _, err := unstructured.NestedStringMap(c.Object, "spec", "postgresql", "parameters")
	if err != nil {
		return err
	}
	if params == nil {
		params = map[string]string{}
	}
	annotations := c.GetAnnotations()
	for _, k := range strings.Split(annotations[annotationManagedPGParams], ",") {
		if _, keep := want[k]; k != "" && !keep {
			delete(params, k)
		}
	}
	keys := make([]string, 0, len(want))
	for k, v := range want {
		params[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		annotations = mergeMaps(annotations, map[string]string{annotationManagedPGParams: strings.Join(keys, ",")})
	} else {
		delete(annotations, annotationManagedPGParams)
	}
	c.SetAnnotations(annotations)
	if len(params) == 0 {
		unstructured.RemoveNestedField(c.Object, "spec", "postgresql", "parameters")
		return nil
	}
	return unstructured.SetNestedStringMap(c.Object, params, "spec", "postgresql", "parameters")
}

// quantityLess reports a < b for two quantity strings (unparsable a counts as smaller).
func quantityLess(a, b string) bool {
	qa, err := resource.ParseQuantity(a)
	if err != nil {
		return true
	}
	qb, err := resource.ParseQuantity(b)
	if err != nil {
		return false
	}
	return qa.Cmp(qb) < 0
}
