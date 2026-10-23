package backup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Backups carry a copy of the SSH host keys from the Secret the pods serve them from.
func TestBackupJobCopiesSSHHostKeys(t *testing.T) {
	s := testScheme(t)
	build := func(mutate func(f *forgejov1.Forgejo)) *corev1.PodSpec {
		t.Helper()
		f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"}}
		if mutate != nil {
			mutate(f)
		}
		b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns"}}
		in := JobInput{Scheme: s, Instance: f, InstanceName: f.Name, ForgejoImage: forgejov1.DefaultImage,
			PostgresImage: forgejov1.DefaultPostgresImage, DB: &forgejo.DBConnection{SecretName: "db"}, BackupPVC: "fj-backups"}
		job, err := BackupJob(in, b, true, metav1.Now())
		if err != nil {
			t.Fatal(err)
		}
		return &job.Spec.Template.Spec
	}
	secretOf := func(spec *corev1.PodSpec) string {
		for _, v := range spec.Volumes {
			if v.Name == volSSHHostKeys {
				if v.Secret.Optional == nil || !*v.Secret.Optional {
					t.Fatal("the Secret may not exist yet: the volume must be optional")
				}
				for _, m := range spec.Containers[0].VolumeMounts {
					if m.Name == volSSHHostKeys && m.ReadOnly && m.MountPath == sshHostKeysMountPath {
						return v.Secret.SecretName
					}
				}
				t.Fatal("archive container does not mount the host keys read-only")
			}
		}
		return ""
	}
	if got := secretOf(build(nil)); got != "fj-ssh-host-keys" {
		t.Errorf("default: %q", got)
	}
	if got := secretOf(build(func(f *forgejov1.Forgejo) {
		f.Spec.Server.SSH.HostKeys = &forgejov1.SSHHostKeysSpec{ExistingSecret: "mine"}
	})); got != "mine" {
		t.Errorf("existingSecret: %q", got)
	}
	if got := secretOf(build(func(f *forgejov1.Forgejo) { f.Spec.Server.SSH.Enabled = ptr.To(false) })); got != "" {
		t.Errorf("SSH disabled: %q", got)
	}
}

func TestArchiveScriptCopiesSSHHostKeys(t *testing.T) {
	for _, tool := range []string{"sh", "wc", "tr", "date"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	// "stat -c %s" as in the Forgejo image (busybox), also where stat is BSD's.
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "stat"), []byte("#!/bin/sh\nwc -c < \"$3\" | tr -d ' '\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "b1")
	keys := filepath.Join(dir, "keys")
	for _, d := range []string{backupDir, keys} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		filepath.Join(backupDir, DBDumpFile):        "PGDMP",
		filepath.Join(keys, "ssh_host_ed25519_key"): "KEY",
		filepath.Join(keys, "unrelated"):            "x",
	} {
		if err := os.WriteFile(name, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	term := filepath.Join(dir, "termination-log")
	script := strings.ReplaceAll(archiveScript(forgejo.LayoutForImage(forgejov1.DefaultImage)), "/dev/termination-log", term)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "BACKUP_DIR=" + backupDir, "BACKUP_NAME=b1", "INCLUDE_DATA=false",
		"DATA_EXCLUDES_JSON=[]", "NAMESPACE=ns", "FORGEJO_NAME=fj", "REASON=Manual", "FORGEJO_IMAGE=i", "POSTGRES_IMAGE=p",
		"DATABASE_NAME=forgejo", "STARTED_AT=now", "SSH_HOST_KEYS_DIR=" + keys, "SSH_HOST_KEYS_SECRET=fj-ssh-host-keys",
		"OBJECT_STORAGE_JSON=" + objectStorageManifest(objectStorageInstance())}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	fi, err := os.Stat(filepath.Join(backupDir, SSHHostKeysDir, "ssh_host_ed25519_key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("copied key: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, SSHHostKeysDir, "unrelated")); err == nil {
		t.Fatal("only host keys are copied")
	}
	b, err := os.ReadFile(filepath.Join(backupDir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		SSHHostKeys struct {
			Directory string   `json:"directory"`
			Secret    string   `json:"secret"`
			Files     []string `json:"files"`
		} `json:"sshHostKeys"`
		ObjectStorage *struct {
			Bucket     string   `json:"bucket"`
			Prefix     string   `json:"prefix"`
			Subsystems []string `json:"subsystems"`
		} `json:"objectStorage"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest: %v\n%s", err, b)
	}
	if m.SSHHostKeys.Directory != SSHHostKeysDir || m.SSHHostKeys.Secret != "fj-ssh-host-keys" ||
		len(m.SSHHostKeys.Files) != 1 || m.SSHHostKeys.Files[0] != "ssh_host_ed25519_key" {
		t.Fatalf("manifest sshHostKeys: %+v", m.SSHHostKeys)
	}
	if o := m.ObjectStorage; o == nil || o.Bucket != "forgejo" || o.Prefix != "fj/" || strings.Join(o.Subsystems, ",") !=
		"lfs,attachments,avatars,repo-avatars,repo-archive,packages,actions_log" {
		t.Fatalf("manifest objectStorage: %+v\n%s", o, b)
	}
	if got := objectStorageManifest(&forgejov1.Forgejo{}); got != "null" {
		t.Fatalf("without object storage: %s", got)
	}
}

func objectStorageInstance() *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{}
	f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{
		S3: forgejov1.S3Connection{Endpoint: "http://garage.s3-lab:3900", Bucket: "forgejo", Prefix: "fj",
			CredentialsSecret: forgejov1.S3CredentialsSecretRef{Name: "s3"}},
		Subsystems: forgejov1.ObjectStorageSubsystems{ActionsArtifacts: ptr.To(false)},
	}
	return f
}
