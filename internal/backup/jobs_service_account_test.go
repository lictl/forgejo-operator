package backup

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// Kubernetes admits registry credentials from the selected service account when
// imagePullSecrets is empty. Every recovery Job must select the instance's account
// without mounting its API token; explicit image pull secrets remain authoritative.
func TestRecoveryJobsKeepServiceAccountPullCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, serviceAccount string
		pullSecrets          []corev1.LocalObjectReference
	}{
		{name: "service account credentials", serviceAccount: "private-registry"},
		{name: "explicit credentials", serviceAccount: "private-registry",
			pullSecrets: []corev1.LocalObjectReference{{Name: "forgejo-registry"}, {Name: "rclone-registry"}}},
		{name: "default account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, b, target, in := remoteJobFixture(t)
			f.Spec.PodTemplate.ServiceAccountName = tc.serviceAccount
			f.Spec.ImagePullSecrets = tc.pullSecrets
			r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: f.Namespace}}
			source := RemoteSource{RemoteTarget: target, Files: []forgejov1.RemoteBackupFile{
				{Name: DBDumpFile, SHA256: "database"}, {Name: DataArchiveFile, SHA256: "data"},
			}}
			builders := map[string]func() (*batchv1.Job, error){
				"backup":        func() (*batchv1.Job, error) { return BackupJob(in, b, true, metav1.Now()) },
				"cleanup":       func() (*batchv1.Job, error) { return CleanupJob(in, b) },
				"local restore": func() (*batchv1.Job, error) { return RestoreJob(in, r, b, true, true) },
				"remote upload": func() (*batchv1.Job, error) {
					return UploadJob(in, b, target, IdentitySecretName(b), `{"formatVersion":1`)
				},
				"remote restore": func() (*batchv1.Job, error) { return RemoteRestoreJob(in, r, source, true, true) },
			}
			for name, build := range builders {
				t.Run(name, func(t *testing.T) {
					job, err := build()
					if err != nil {
						t.Fatal(err)
					}
					pod := job.Spec.Template.Spec
					if pod.ServiceAccountName != tc.serviceAccount {
						t.Errorf("service account = %q, want %q", pod.ServiceAccountName, tc.serviceAccount)
					}
					if !slices.Equal(pod.ImagePullSecrets, tc.pullSecrets) {
						t.Errorf("imagePullSecrets = %v, want %v", pod.ImagePullSecrets, tc.pullSecrets)
					}
					if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
						t.Error("recovery Jobs must explicitly disable service account token mounting")
					}
				})
			}
		})
	}
}

func TestCleanupWithoutInstanceDefaultsStayTokenless(t *testing.T) {
	_, b, _, in := remoteJobFixture(t)
	in.Instance = nil
	in.ForgejoImage = "private.example/forgejo:15"
	job, err := CleanupJob(in, b)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if pod.Containers[0].Image != in.ForgejoImage {
		t.Errorf("orphan cleanup image = %q, want recorded image %q", pod.Containers[0].Image, in.ForgejoImage)
	}
	if pod.ServiceAccountName != "" || len(pod.ImagePullSecrets) != 0 {
		t.Error("cleanup without an instance must leave registry credentials to the namespace default")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("orphan cleanup must explicitly disable service account token mounting")
	}
}
