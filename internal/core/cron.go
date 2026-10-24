package core

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Forgejo's cron tasks (mirror updates, scheduled Actions workflows, cleanups, ...) have no
// leader election or lock: every Forgejo process runs each of them, and several do not
// tolerate running twice at once (scheduled workflow runs are created twice, a mirror is
// fetched twice). While forgejo.CronRunnerEnabled, the server pods therefore run an app.ini
// without cron tasks (app-web.ini), and StatefulSet "<name>-cron" runs the one pod that has
// them ("<name>-cron-0"): the same pod with the full app.ini, not behind the Services.
//
// It is a StatefulSet, not a Deployment, because a StatefulSet creates its pod only once the
// previous pod object is gone: after the kubelet confirmed that its containers stopped, or
// after a force delete. A ReplicaSet replaces a pod as soon as its deletion starts, so a pod
// being deleted on an unreachable node, which may still run and reach the database, and its
// replacement would both run the cron tasks. The tasks pause instead until the old pod is
// gone; runs missed meanwhile catch up (CronRunnerStuck tells the administrator).

// cronStopRequeue is how often the workload step looks again while it waits for the cron
// runner to stop before a new image is rolled out.
const cronStopRequeue = 3 * time.Second

// cronStuckAfter is how long a cron runner pod may still exist after its deletion deadline
// (deletionTimestamp: the deletion request plus the grace period) before CronRunnerStuck
// warns that its node is probably unreachable.
const cronStuckAfter = 2 * time.Minute

// cronReplicas is 1, or 0 whenever the server pods do not run freely: in maintenance (a
// restore), during an upgrade or while a failed upgrade's image is held, before the first
// rollout has completed, while a restored database waits for its first pod to migrate it,
// and while the server is scaled to zero. "forgejo migrate" takes no lock, so its single
// pod must be the only Forgejo pod.
func cronReplicas(rc *forgejo.ReconcileContext, webReplicas int32, migrationHold bool) int32 {
	f := rc.Instance
	if rc.Maintenance || rc.ReplicasOverride != nil || rc.Upgrading || forgejo.UpgradeHoldsWorkload(f) ||
		f.Status.Image == "" || migrationHold || webReplicas == 0 {
		return 0
	}
	return 1
}

// holdForCronRunner keeps the server Deployment as it is while a new image would be rolled
// out but cron runner pods (of the old image) still exist: the first pod of the new image
// migrates the database, which no other Forgejo pod may use meanwhile. It scales the cron
// runner to zero and reports whether the rollout has to wait for its pods to stop. An
// upgrade with a pre-upgrade backup has stopped every pod before; one without a backup
// starts the rollout right away. A pod stuck on an unreachable node keeps the rollout
// waiting until it is gone (CronRunnerStuck, see reportCronRunner).
func holdForCronRunner(ctx context.Context, rc *forgejo.ReconcileContext) (bool, error) {
	f := rc.Instance
	web := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, web); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if deployedImage(web) == "" || deployedImage(web) == rc.TargetImage {
		return false, nil
	}
	cron, err := forgejo.CronRunner(ctx, rc.Client, f)
	if err != nil {
		return false, err
	}
	pods, err := cronPods(ctx, rc)
	if err != nil {
		return false, err
	}
	wanted := cron != nil && ptr.Deref(cron.Spec.Replicas, 1) != 0
	if len(pods) == 0 && !wanted {
		return false, nil
	}
	if wanted {
		cron.Spec.Replicas = ptr.To[int32](0)
		if err := rc.Client.Update(ctx, cron); err != nil {
			return false, err
		}
		rc.Event(corev1.EventTypeNormal, "WaitingForCronRunner", "Rollout",
			"scaled the cron runner %s to zero: %s is rolled out once its pod has stopped", cron.Name, rc.TargetImage)
	}
	return true, nil
}

// deployedImage is the image of the forgejo container of a Deployment's template.
func deployedImage(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			return c.Image
		}
	}
	return ""
}

// cronPods lists the cron runner's pods that have not terminated, terminating ones included.
func cronPods(ctx context.Context, rc *forgejo.ReconcileContext) ([]corev1.Pod, error) {
	all, err := allCronPods(ctx, rc)
	if err != nil {
		return nil, err
	}
	return liveCronPods(all), nil
}

// allCronPods lists every pod with the cron runner's labels, terminated ones included.
func allCronPods(ctx context.Context, rc *forgejo.ReconcileContext) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.CronSelectorLabels(rc.Instance))); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func liveCronPods(all []corev1.Pod) []corev1.Pod {
	var pods []corev1.Pod
	for _, p := range all {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			pods = append(pods, p)
		}
	}
	return pods
}

// reconcileCronRunner ensures StatefulSet "<name>-cron" while forgejo.CronRunnerEnabled,
// with the server pods' template (same image, volumes, resources, config hash and restarts)
// and the full app.ini, and deletes it otherwise.
func reconcileCronRunner(ctx context.Context, rc *forgejo.ReconcileContext, replicas int32, restartedAt, objectStorageDigest string) error {
	f := rc.Instance
	if !forgejo.CronRunnerEnabled(f) {
		owned, err := forgejo.CronRunner(ctx, rc.Client, f)
		if err != nil || owned == nil || owned.DeletionTimestamp != nil {
			return err
		}
		if err := rc.Client.Delete(ctx, owned, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
		rc.Event(corev1.EventTypeNormal, "CronRunnerDeleted", "HighAvailability",
			"removed StatefulSet %s: without highAvailability the server pod runs the cron tasks", owned.Name)
		return nil
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace}}
	return apply(ctx, rc, sts, true, func() error {
		exists := sts.ResourceVersion != ""
		sts.Labels = mergeMaps(sts.Labels, podLabelsFor(rc, forgejo.ComponentCron, nil))
		sts.Spec.Replicas = ptr.To(replicas)
		if !exists {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: forgejo.CronSelectorLabels(f)}
			// Parallel: a template change replaces the pod even while it is not Ready (a
			// crash-looping runner gets the fixed template); OrderedReady would wait for it.
			sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		}
		sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
			Type:          appsv1.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
		}
		sts.Spec.RevisionHistoryLimit = ptr.To[int32](revisionHistory)
		// The API server's default; the pod mounts the data claim, it has no claim templates.
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		sts.Spec.Template.ObjectMeta = metav1.ObjectMeta{
			Labels:      podLabelsFor(rc, forgejo.ComponentCron, f.Spec.PodTemplate.Labels),
			Annotations: podAnnotations(rc, restartedAt, objectStorageDigest),
		}
		sts.Spec.Template.Spec = podSpecFor(rc, forgejo.ComponentCron)
		return nil
	})
}

// reportCronRunner sets status.highAvailability.cronRunner(Ready) and the
// forgejo_operator_cron_runner_ready gauge, which is only exported while a runner should run,
// and warns (CronRunnerStuck) about a runner pod that outlives its deletion deadline.
func reportCronRunner(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	if !forgejo.CronRunnerEnabled(f) {
		forgejo.CronRunnerReady.DeleteLabelValues(f.Namespace, f.Name)
		if ha := f.Status.HighAvailability; ha != nil {
			ha.CronRunner, ha.CronRunnerReady = "", false
		}
		return nil
	}
	cron, err := forgejo.CronRunner(ctx, rc.Client, f)
	if err != nil {
		return err
	}
	wanted := cron != nil && ptr.Deref(cron.Spec.Replicas, 1) > 0
	all, err := allCronPods(ctx, rc)
	if err != nil {
		return err
	}
	// A pod object that outlives its deletion keeps the StatefulSet from starting another,
	// whatever its phase.
	for i := range all {
		if all[i].DeletionTimestamp != nil {
			warnStuckCronPod(rc, &all[i])
		}
	}
	pods := liveCronPods(all)
	if f.Status.HighAvailability == nil {
		f.Status.HighAvailability = &forgejov1.HighAvailabilityStatus{}
	}
	ha := f.Status.HighAvailability
	ha.CronRunner, ha.CronRunnerReady = "", false
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		ready := podReady(p)
		if ha.CronRunner == "" || (ready && !ha.CronRunnerReady) {
			ha.CronRunner, ha.CronRunnerReady = p.Name, ready
		}
	}
	if !wanted {
		forgejo.CronRunnerReady.DeleteLabelValues(f.Namespace, f.Name)
		return nil
	}
	v := 0.0
	if ha.CronRunnerReady {
		v = 1
	}
	forgejo.CronRunnerReady.WithLabelValues(f.Namespace, f.Name).Set(v)
	return nil
}

// warnStuckCronPod warns (hourly) about a cron runner pod that still exists cronStuckAfter
// past its deletion deadline. Usually its kubelet does not confirm that it stopped because the
// node is down or cut off (the pod then still counts as running: a new image's rollout waits
// too); otherwise a finalizer holds it. The StatefulSet starts no other pod while it exists.
func warnStuckCronPod(rc *forgejo.ReconcileContext, p *corev1.Pod) {
	if time.Since(p.DeletionTimestamp.Time) < cronStuckAfter {
		return
	}
	warnEvery(rc, "cron-runner-stuck/"+string(p.UID), "CronRunnerStuck", "HighAvailability",
		"cron runner pod %s on node %s still exists %s after its deletion deadline (phase %s): its node is probably unreachable, "+
			"or a finalizer holds it. No other cron runner starts until it is gone, so cron tasks pause. Once the node is confirmed "+
			"down, or its containers stopped, remove it: kubectl -n %s delete pod %s --force --grace-period=0",
		p.Name, p.Spec.NodeName, time.Since(p.DeletionTimestamp.Time).Round(time.Second), p.Status.Phase, p.Namespace, p.Name)
}
