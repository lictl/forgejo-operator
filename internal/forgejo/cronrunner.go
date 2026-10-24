package forgejo

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// CronRunner returns the instance's cron runner, StatefulSet "<name>-cron", or nil when it
// does not exist. An object of that name that the instance does not control is not its cron
// runner: it is neither scaled, counted nor waited for. (The API refuses ForgejoRunner names
// that end in "-cron", whose StatefulSet would have that name.)
func CronRunner(ctx context.Context, r client.Reader, f *forgejov1.Forgejo) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: CronRunnerName(f)}, sts); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(sts, f) {
		return nil, nil
	}
	return sts, nil
}
