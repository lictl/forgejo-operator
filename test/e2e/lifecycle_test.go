//go:build e2e

//nolint:goconst // Keep Kubernetes JSON field names literal in API fixtures.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

const (
	lifecycleNS  = "e2e-lifecycle"
	instanceName = "forge"
	oldImage     = "codeberg.org/forgejo/forgejo:15"
	newImage     = "codeberg.org/forgejo/forgejo:17"
	// A valid newer version with an intentionally unavailable registry exercises the
	// timeout and persisted rollback path without depending on an upstream outage.
	failedImage      = "unavailable.invalid/forgejo:18"
	postgresImage    = "postgres:17-alpine"
	lifecycleTimeout = 8 * time.Minute
)

func runLifecycle() {
	By("creating a disposable external PostgreSQL database without optional operators")
	_, err := kubectl("create", "namespace", lifecycleNS)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		// Exclude Secrets and rendered configuration from failure diagnostics.
		for _, resource := range []string{"forgejos,forgejobackups,forgejorestores", "pods,jobs,pvc", "events"} {
			out, _ := kubectl("-n", lifecycleNS, "get", resource, "-o", "wide")
			_, _ = fmt.Fprintln(GinkgoWriter, out)
		}
		out, _ := kubectl("-n", namespace, "logs", "deployment/forgejo-operator-controller-manager", "--tail=150")
		_, _ = fmt.Fprintln(GinkgoWriter, out)
	})
	createPostgres()
	applyJSON(map[string]any{
		"apiVersion": "forgejo.lucas.icu/v1alpha1", "kind": "Forgejo",
		"metadata": map[string]any{"name": instanceName, "namespace": lifecycleNS},
		"spec": map[string]any{
			"image": oldImage,
			"database": map[string]any{"type": "External", "cnpg": map[string]any{"imageName": postgresImage},
				"external": map[string]any{"host": "postgres", "database": "forgejo", "sslMode": "disable",
					"credentialsSecret": map[string]any{"name": "postgres"}}},
			"storage": map[string]any{"size": "1Gi", "retainPolicy": "Retain"},
			"backup":  map[string]any{"storage": map[string]any{"size": "1Gi"}},
			"upgrade": map[string]any{"timeout": "2m"},
		},
	})
	waitForgejo("initial install", func(f forgejov1.Forgejo) bool { return ready(f) && f.Status.Image == oldImage })
	identity := getObject[corev1.Secret]("secret", "forge-secrets")
	admin := getObject[corev1.Secret]("secret", "forge-admin")
	data := getObject[corev1.PersistentVolumeClaim]("pvc", "forge-data")

	By("writing a repository commit and an issue to exercise both the volume and database")
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.request("POST", "/user/repos", map[string]any{"name": "release-proof", "auto_init": true}, http.StatusCreated)
		api.writeFile("before-backup\n", "")
		api.request("POST", api.repo()+"/issues", map[string]any{"title": "keep this issue"}, http.StatusCreated)
		api.checkData("before-backup\n", 1)
		api.cloneAndCheck()
	})

	By("taking a manual backup, changing both data stores, and restoring the original contents")
	createBackup("snapshot", "Manual")
	waitBackup("snapshot")
	withForgejoAPI(admin, func(api *forgejoAPI) {
		file := api.file()
		api.writeFile("after-backup\n", file["sha"].(string))
		api.request("POST", api.repo()+"/issues", map[string]any{"title": "discard this issue"}, http.StatusCreated)
		api.checkData("after-backup\n", 2)
	})
	applyJSON(map[string]any{
		"apiVersion": "forgejo.lucas.icu/v1alpha1", "kind": "ForgejoRestore",
		"metadata": map[string]any{"name": "restore-proof", "namespace": lifecycleNS},
		"spec":     map[string]any{"forgejoRef": map[string]any{"name": instanceName}, "backupName": "snapshot"},
	})
	waitRestore("restore-proof")
	waitForgejo("restored instance", ready)
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.checkData("before-backup\n", 1)
		api.cloneAndCheck()
	})

	By("upgrading a real database across Forgejo majors with a completed safety backup")
	patchSpec(map[string]any{"image": newImage})
	upgraded := waitForgejo("successful major upgrade", func(f forgejov1.Forgejo) bool {
		return ready(f) && f.Status.Image == newImage && f.Status.Upgrade != nil &&
			f.Status.Upgrade.Phase == forgejov1.UpgradePhaseSucceeded
	})
	waitBackup(upgraded.Status.Upgrade.BackupName)
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.checkData("before-backup\n", 1)
		var version map[string]string
		Expect(json.Unmarshal(api.request("GET", "/version", nil, http.StatusOK), &version)).To(Succeed())
		Expect(version["version"]).To(HavePrefix("17."))
	})

	By("restarting the operator during a failing upgrade and verifying rollback resumes from its backup")
	patchSpec(map[string]any{"image": failedImage})
	rolling := waitForgejo("failed image rollout", func(f forgejov1.Forgejo) bool {
		return f.Status.Upgrade != nil && f.Status.Upgrade.ToImage == failedImage &&
			f.Status.Upgrade.Phase == forgejov1.UpgradePhaseRollingOut
	})
	waitBackup(rolling.Status.Upgrade.BackupName)
	_, err = kubectl("-n", namespace, "rollout", "restart", "deployment/forgejo-operator-controller-manager")
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectl("-n", namespace, "rollout", "status",
		"deployment/forgejo-operator-controller-manager", "--timeout=60s")
	Expect(err).NotTo(HaveOccurred())
	rolledBack := waitForgejo("automatic rollback after manager restart", func(f forgejov1.Forgejo) bool {
		return ready(f) && f.Status.Image == newImage && f.Status.Upgrade != nil &&
			f.Status.Upgrade.Phase == forgejov1.UpgradePhaseRolledBack
	})
	Expect(rolledBack.Status.Upgrade.BackupName).To(Equal(rolling.Status.Upgrade.BackupName))
	Expect(rolledBack.Status.Upgrade.StartTime).To(Equal(rolling.Status.Upgrade.StartTime))
	waitRestore(rolledBack.Status.Upgrade.RestoreName)
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.checkData("before-backup\n", 1)
		api.cloneAndCheck()
	})
	patchSpec(map[string]any{"image": newImage})
	waitForgejo("cleared failed image", ready)

	By("pruning only scheduled backups and retaining manual and pre-upgrade backups")
	for _, name := range []string{"scheduled-one", "scheduled-two"} {
		createBackup(name, "Scheduled")
		waitBackup(name)
	}
	patchSpec(map[string]any{"backup": map[string]any{"enabled": true, "schedule": "@every 24h", "retention": 1}})
	Eventually(func(g Gomega) {
		out, e := kubectl("-n", lifecycleNS, "get", "forgejobackup", "scheduled-one", "--ignore-not-found", "-o", "name")
		g.Expect(e).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(BeEmpty())
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
	waitBackup("scheduled-two")
	waitBackup("snapshot")
	waitBackup(upgraded.Status.Upgrade.BackupName)
	waitBackup(rolling.Status.Upgrade.BackupName)

	By("deleting and recreating a retained instance without replacing its identity or data")
	retained := getObject[forgejov1.Forgejo]("forgejo", instanceName)
	backupVolume := getObject[corev1.PersistentVolumeClaim]("pvc", "forge-backups")
	_, err = kubectl("-n", lifecycleNS, "delete", "forgejo", instanceName, "--wait=true", "--timeout=60s")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		out, e := kubectl("-n", lifecycleNS, "get", "deployment", instanceName, "--ignore-not-found", "-o", "name")
		g.Expect(e).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(BeEmpty())
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
	checkRetainedResources(identity, admin, data, backupVolume)
	waitBackup("snapshot")
	applyJSON(map[string]any{"apiVersion": "forgejo.lucas.icu/v1alpha1", "kind": "Forgejo",
		"metadata": map[string]any{"name": instanceName, "namespace": lifecycleNS}, "spec": retained.Spec})
	waitForgejo("recreated retained instance", ready)
	checkRetainedResources(identity, admin, data, backupVolume)
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.checkData("before-backup\n", 1)
		api.cloneAndCheck()
		api.writeFile("after-recreation\n", api.file()["sha"].(string))
		api.request("POST", api.repo()+"/issues", map[string]any{"title": "discard after recreation"}, http.StatusCreated)
		api.checkData("after-recreation\n", 2)
	})
	By("restoring the retained archive after instance recreation")
	applyJSON(map[string]any{
		"apiVersion": "forgejo.lucas.icu/v1alpha1", "kind": "ForgejoRestore",
		"metadata": map[string]any{"name": "retained-restore-proof", "namespace": lifecycleNS},
		"spec":     map[string]any{"forgejoRef": map[string]any{"name": instanceName}, "backupName": "snapshot"},
	})
	waitRestore("retained-restore-proof")
	waitForgejo("restored retained archive", ready)
	checkRetainedResources(identity, admin, data, backupVolume)
	withForgejoAPI(admin, func(api *forgejoAPI) {
		api.checkData("before-backup\n", 1)
		api.cloneAndCheck()
	})
}

func checkRetainedResources(identity, admin corev1.Secret, data, backups corev1.PersistentVolumeClaim) {
	for _, original := range []corev1.Secret{identity, admin} {
		current := getObject[corev1.Secret]("secret", original.Name)
		Expect(current.UID).To(Equal(original.UID))
		// A failed assertion must not print the secret values.
		Expect(reflect.DeepEqual(current.Data, original.Data)).To(BeTrue(), original.Name)
	}
	for _, original := range []corev1.PersistentVolumeClaim{data, backups} {
		Expect(getObject[corev1.PersistentVolumeClaim]("pvc", original.Name).UID).To(Equal(original.UID))
	}
}

func kubectl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl: %w: %s", err, out)
	}
	return string(out), nil
}

func applyJSON(object any) {
	data, err := json.Marshal(object)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

func getObject[T any](kind, name string) T {
	out, err := kubectl("-n", lifecycleNS, "get", kind, name, "-o", "json")
	Expect(err).NotTo(HaveOccurred())
	var result T
	Expect(json.Unmarshal([]byte(out), &result)).To(Succeed())
	return result
}

func patchSpec(spec map[string]any) {
	data, err := json.Marshal(map[string]any{"spec": spec})
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectl("-n", lifecycleNS, "patch", "forgejo", instanceName, "--type=merge", "-p", string(data))
	Expect(err).NotTo(HaveOccurred())
}

func ready(f forgejov1.Forgejo) bool {
	for _, c := range f.Status.Conditions {
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue && c.ObservedGeneration == f.Generation {
			return true
		}
	}
	return false
}

func waitForgejo(description string, predicate func(forgejov1.Forgejo) bool) forgejov1.Forgejo {
	var f forgejov1.Forgejo
	Eventually(func(g Gomega) {
		f = getObject[forgejov1.Forgejo]("forgejo", instanceName)
		g.Expect(predicate(f)).To(BeTrue(), "%s: phase=%s upgrade=%+v conditions=%+v",
			description, f.Status.Phase, f.Status.Upgrade, f.Status.Conditions)
	}, lifecycleTimeout, 3*time.Second).Should(Succeed())
	return f
}

func createBackup(name, reason string) {
	applyJSON(map[string]any{"apiVersion": "forgejo.lucas.icu/v1alpha1", "kind": "ForgejoBackup",
		"metadata": map[string]any{"name": name, "namespace": lifecycleNS},
		"spec":     map[string]any{"forgejoRef": map[string]any{"name": instanceName}, "reason": reason}})
}

func waitBackup(name string) {
	Expect(name).NotTo(BeEmpty())
	Eventually(func(g Gomega) {
		b := getObject[forgejov1.ForgejoBackup]("forgejobackup", name)
		g.Expect(b.Status.Phase).To(Equal(forgejov1.BackupPhaseCompleted), b.Status.Message)
		g.Expect(b.Status.DatabaseSizeBytes).To(BeNumerically(">", 0))
		g.Expect(b.Status.DataSizeBytes).To(BeNumerically(">", 0))
	}, lifecycleTimeout, 3*time.Second).Should(Succeed())
}

func waitRestore(name string) {
	Expect(name).NotTo(BeEmpty())
	Eventually(func(g Gomega) {
		r := getObject[forgejov1.ForgejoRestore]("forgejorestore", name)
		g.Expect(r.Status.Phase).To(Equal(forgejov1.RestorePhaseCompleted), r.Status.Message)
	}, lifecycleTimeout, 3*time.Second).Should(Succeed())
}

func createPostgres() {
	secret := make([]byte, 24)
	_, err := rand.Read(secret)
	Expect(err).NotTo(HaveOccurred())
	applyJSON(corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: lifecycleNS},
		StringData: map[string]string{"username": "forgejo", "password": hex.EncodeToString(secret)},
	})
	_, err = kubectl("apply", "-n", lifecycleNS, "-f", "test/e2e/testdata/postgres.yaml")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		g.Expect(getObject[appsv1.Deployment]("deployment", "postgres").Status.AvailableReplicas).To(Equal(int32(1)))
	}, 3*time.Minute).Should(Succeed())
}

type forgejoAPI struct{ url, username, password string }

func withForgejoAPI(admin corev1.Secret, run func(*forgejoAPI)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "-n", lifecycleNS, "port-forward",
		"service/forge-http", ":3000", "--address=127.0.0.1")
	stdout, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	Expect(cmd.Start()).To(Succeed())
	defer func() { cancel(); _ = cmd.Wait() }()
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if after, ok := strings.CutPrefix(scanner.Text(), "Forwarding from "); ok {
				address <- strings.Fields(after)[0]
				return
			}
		}
		address <- ""
	}()
	var host string
	select {
	case host = <-address:
	case <-time.After(30 * time.Second):
		Fail("port-forward did not become ready")
	}
	Expect(host).NotTo(BeEmpty())
	run(&forgejoAPI{url: "http://" + host,
		username: string(admin.Data["username"]), password: string(admin.Data["password"])})
}

func (api *forgejoAPI) request(method, path string, body any, status int) []byte {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
	}
	req, err := http.NewRequest(method, api.url+"/api/v1"+path, bytes.NewReader(data))
	Expect(err).NotTo(HaveOccurred())
	req.SetBasicAuth(api.username, api.password)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = response.Body.Close() }()
	result, err := io.ReadAll(response.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.StatusCode).To(Equal(status), "%s %s", method, path)
	return result
}

func (api *forgejoAPI) repo() string { return "/repos/" + api.username + "/release-proof" }
func (api *forgejoAPI) file() map[string]any {
	var file map[string]any
	data := api.request("GET", api.repo()+"/contents/sentinel.txt", nil, http.StatusOK)
	Expect(json.Unmarshal(data, &file)).To(Succeed())
	return file
}
func (api *forgejoAPI) writeFile(contents, sha string) {
	method, status := "POST", http.StatusCreated
	body := map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(contents)), "message": "release proof"}
	if sha != "" {
		method, status = "PUT", http.StatusOK
		body["sha"] = sha
	}
	api.request(method, api.repo()+"/contents/sentinel.txt", body, status)
}
func (api *forgejoAPI) checkData(contents string, issues int) {
	file := api.file()
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file["content"].(string), "\n", ""))
	Expect(err).NotTo(HaveOccurred())
	Expect(string(decoded)).To(Equal(contents))
	var list []map[string]any
	Expect(json.Unmarshal(api.request("GET", api.repo()+"/issues?state=all", nil, http.StatusOK), &list)).To(Succeed())
	Expect(list).To(HaveLen(issues))
	titles := make([]string, 0, len(list))
	for _, issue := range list {
		titles = append(titles, issue["title"].(string))
	}
	Expect(titles).To(ContainElement("keep this issue"))
}
func (api *forgejoAPI) cloneAndCheck() {
	dir, err := os.MkdirTemp("", "forgejo-e2e-git-")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", api.url+"/"+api.username+"/release-proof.git", dir)
	// Keep credentials out of argv, URLs and test output.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte(api.username+":"+api.password)))
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
	data, err := os.ReadFile(filepath.Join(dir, "sentinel.txt"))
	Expect(err).NotTo(HaveOccurred())
	Expect(string(data)).To(Equal("before-backup\n"))
}
