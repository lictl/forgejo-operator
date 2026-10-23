package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

const (
	testAppIniSecret = "git-config"
	testForgejoName  = "git"
	testPool         = "git"
	testOther        = "other"
	testImage17      = "codeberg.org/forgejo/forgejo:17"
	testSlotName     = "ci-0"
	testTaintKey     = "dedicated"
)

func testRunner(mutate ...func(*forgejov1.ForgejoRunner)) *forgejov1.ForgejoRunner {
	r := &forgejov1.ForgejoRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "ns"},
		Spec: forgejov1.ForgejoRunnerSpec{
			ForgejoRef: forgejov1.ForgejoRef{Name: testForgejoName},
			Replicas:   ptr.To[int32](2),
			Capacity:   1,
			Executor:   forgejov1.RunnerExecutorDocker,
			Image:      forgejov1.DefaultRunnerImage,
		},
	}
	for _, m := range mutate {
		m(r)
	}
	return r
}

func testForgejo() *forgejov1.Forgejo {
	return &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: testForgejoName, Namespace: "ns", UID: "fj-uid"}}
}

// Observed on Forgejo 17.0.0: "forgejo-cli actions register" printed this UUID for a secret
// starting with these 16 characters.
func TestUUIDOfMatchesForgejo(t *testing.T) {
	token := "e1688e40c638c928" + strings.Repeat("0", 24)
	if got, want := UUIDOf(token), "65313638-3865-3430-6336-333863393238"; got != want {
		t.Fatalf("UUIDOf = %s, want %s", got, want)
	}
	if UUIDOf("short") != "" {
		t.Fatal("UUIDOf of a short token is not empty")
	}
}

func TestNewTokenIsValidAndRandom(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if !ValidToken(a) || !ValidToken(b) || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 41)} {
		if ValidToken(bad) {
			t.Errorf("ValidToken(%q) = true", bad)
		}
	}
}

func TestIndexOfKey(t *testing.T) {
	for key, want := range map[string]int32{"token-0": 0, "token-12": 12, "config-3.yaml": 3} {
		if got, ok := IndexOfKey(key); !ok || got != want {
			t.Errorf("IndexOfKey(%q) = %d, %v", key, got, ok)
		}
	}
	for _, key := range []string{"token-", "token-01", "token--1", "config-1", testOther, "token-x"} {
		if _, ok := IndexOfKey(key); ok {
			t.Errorf("IndexOfKey(%q) accepted", key)
		}
	}
}

func TestScope(t *testing.T) {
	if s := Scope(testRunner()); s != "" {
		t.Errorf("instance scope %q", s)
	}
	if s := Scope(testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Scope.Organization = testOrg })); s != testOrg {
		t.Errorf("organization scope %q", s)
	}
	if s := Scope(testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Scope.Repository = testRepo })); s != testRepo {
		t.Errorf("repository scope %q", s)
	}
}

func renderMap(t *testing.T, r *forgejov1.ForgejoRunner, i int32, uuid string) map[string]any {
	t.Helper()
	b, err := RenderConfig(r, testForgejo(), i, uuid)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func path(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func TestRenderConfigConnection(t *testing.T) {
	r := testRunner()
	m := renderMap(t, r, 1, "uuid-1")
	conn := path(m, "server", "connections", ConnectionName).(map[string]any)
	if conn["url"] != "http://git-http.ns.svc.cluster.local:3000/" {
		t.Errorf("url %v", conn["url"])
	}
	if conn["uuid"] != "uuid-1" || conn["token_url"] != "file:"+TokensDir+"/token-1" {
		t.Errorf("connection %v", conn)
	}
	if _, inline := conn["token"]; inline {
		t.Error("the configuration carries the secret inline")
	}
	if got := path(m, "runner", "labels").([]any); len(got) != 1 || got[0] != "docker:docker://"+forgejov1.DefaultRunnerJobImage {
		t.Errorf("default labels %v", got)
	}
	if path(m, "container", "docker_host") != "-" {
		t.Errorf("docker_host without mountSocket: %v", path(m, "container", "docker_host"))
	}
	if path(m, "cache", "enabled") != true || path(m, "cache", "dir") != CacheDir {
		t.Errorf("cache %v", m["cache"])
	}
	if path(m, "runner", "shutdown_timeout") != "10m0s" {
		t.Errorf("shutdown_timeout %v", path(m, "runner", "shutdown_timeout"))
	}
}

func TestRenderConfigUserConfigMergedOperatorKeysWin(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) {
		r.Spec.Docker.MountSocket = true
		r.Spec.Labels = []string{"ubuntu:docker://ubuntu:24.04"}
		r.Spec.Capacity = 3
		r.Spec.Config = &runtime.RawExtension{Raw: []byte(`{
			"runner": {"envs": {"A": "b"}, "capacity": 9, "labels": ["evil:host"], "timeout": "1h"},
			"container": {"options": "--memory=1g"},
			"server": {"connections": {"other": {"url": "https://evil.example/"}}},
			"log": {"level": "debug"}
		}`)}
	})
	m := renderMap(t, r, 0, "u")
	if path(m, "runner", "envs", "A") != "b" || path(m, "runner", "timeout") != "1h" || path(m, "log", "level") != "debug" ||
		path(m, "log", "job_level") != logLevelInfo || path(m, "container", "options") != "--memory=1g" {
		t.Errorf("user keys not merged: %v", m)
	}
	if path(m, "container", "docker_host") != "automount" {
		t.Errorf("docker_host with mountSocket: %v", path(m, "container", "docker_host"))
	}
	if c := path(m, "runner", "capacity"); c != float64(3) {
		t.Errorf("capacity %v", c)
	}
	if l := path(m, "runner", "labels").([]any); len(l) != 1 || l[0] != "ubuntu:docker://ubuntu:24.04" {
		t.Errorf("labels %v", l)
	}
	conns := path(m, "server", "connections").(map[string]any)
	if len(conns) != 1 || conns[ConnectionName] == nil {
		t.Errorf("connections %v", conns)
	}
}

func TestRenderConfigHostHasNoContainerSection(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Executor = forgejov1.RunnerExecutorHost })
	m := renderMap(t, r, 0, "u")
	if m["container"] != nil {
		t.Errorf("container section %v", m["container"])
	}
	if l := path(m, "runner", "labels").([]any); len(l) != 1 || l[0] != labelHost {
		t.Errorf("default host labels %v", l)
	}
}

func TestRenderConfigInvalid(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Config = &runtime.RawExtension{Raw: []byte(`[1]`)} })
	if _, err := RenderConfig(r, testForgejo(), 0, "u"); err == nil {
		t.Fatal("a non-object spec.config was accepted")
	}
}

func TestConfigHashIgnoresRunnerIdentity(t *testing.T) {
	r := testRunner()
	h1, err := ConfigHash(r, testForgejo())
	if err != nil {
		t.Fatal(err)
	}
	r.Spec.Replicas = ptr.To[int32](5)
	h2, _ := ConfigHash(r, testForgejo())
	if h1 != h2 {
		t.Error("the hash changes with replicas, so scaling would roll every pod")
	}
	r.Spec.Labels = []string{"x:docker://alpine"}
	if h3, _ := ConfigHash(r, testForgejo()); h3 == h1 {
		t.Error("the hash ignores the labels")
	}
}

func TestLabelNames(t *testing.T) {
	got := LabelNames([]string{"docker:docker://node:22", labelHost, "bare"})
	if strings.Join(got, ",") != "docker,host,bare" {
		t.Fatalf("LabelNames = %v", got)
	}
}

// restrictedViolations checks the parts of the "restricted" Pod Security Standard a pod spec
// can violate.
func restrictedViolations(spec corev1.PodSpec) []string {
	var out []string
	podNonRoot := spec.SecurityContext != nil && spec.SecurityContext.RunAsNonRoot != nil && *spec.SecurityContext.RunAsNonRoot
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil ||
		spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		out = append(out, "seccomp")
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		out = append(out, "host namespaces")
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			out = append(out, "hostPath "+v.Name)
		}
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		sc := c.SecurityContext
		switch {
		case sc == nil:
			out = append(out, c.Name+": no securityContext")
			continue
		case sc.Privileged != nil && *sc.Privileged:
			out = append(out, c.Name+": privileged")
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			out = append(out, c.Name+": allowPrivilegeEscalation")
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != capDropAll || len(sc.Capabilities.Add) > 0 {
			out = append(out, c.Name+": capabilities")
		}
		if !podNonRoot && (sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot) {
			out = append(out, c.Name+": runAsNonRoot")
		}
	}
	return out
}

func TestHostPodPassesRestricted(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Executor = forgejov1.RunnerExecutorHost })
	spec := PodSpec(r)
	if v := restrictedViolations(spec); len(v) > 0 {
		t.Fatalf("host pod violates restricted: %v", v)
	}
	if spec.Containers[0].Image != forgejov1.DefaultRunnerToolchainImage {
		t.Errorf("host runner image %s", spec.Containers[0].Image)
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Image != forgejov1.DefaultRunnerImage {
		t.Errorf("install container %+v", spec.InitContainers)
	}
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Error("job processes are not reaped (shareProcessNamespace)")
	}
	if got := spec.Containers[0].Command; got[0] != runnerBinDir+"/forgejo-runner" || got[len(got)-1] != ConfigDir+"/config-$(POD_INDEX).yaml" {
		t.Errorf("command %v", got)
	}
}

// The privileged Docker daemon is rendered only for executor Docker, never for a missing one.
func TestNoPrivilegedPodWithoutDockerExecutor(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Executor = "" })
	if got := Executor(r); got != forgejov1.RunnerExecutorHost {
		t.Fatalf("Executor of an empty spec.executor = %s, want Host", got)
	}
	if v := restrictedViolations(PodSpec(r)); len(v) > 0 {
		t.Fatalf("pod of an empty spec.executor violates restricted: %v", v)
	}
}

func TestDockerPod(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Docker.Args = []string{"--mtu=1400"} })
	spec := PodSpec(r)
	if len(spec.InitContainers) != 1 {
		t.Fatalf("init containers %v", spec.InitContainers)
	}
	d := spec.InitContainers[0]
	if d.RestartPolicy == nil || *d.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("the Docker daemon is not a native sidecar")
	}
	if d.SecurityContext == nil || d.SecurityContext.Privileged == nil || !*d.SecurityContext.Privileged {
		t.Error("the Docker daemon is not privileged")
	}
	cmd := strings.Join(d.Command, " ")
	if !strings.Contains(cmd, "--host=unix://"+DockerSocket) || strings.Contains(cmd, "tcp://") || !strings.HasSuffix(cmd, "--mtu=1400") {
		t.Errorf("dockerd command %q", cmd)
	}
	if d.StartupProbe == nil {
		t.Error("the runner may start before the daemon")
	}
	runner := spec.Containers[0]
	if v := restrictedViolations(corev1.PodSpec{SecurityContext: spec.SecurityContext, Containers: []corev1.Container{runner}}); len(v) > 0 {
		t.Errorf("runner container: %v", v)
	}
	if runner.SecurityContext.ReadOnlyRootFilesystem == nil || !*runner.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("runner root file system is writable")
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range runner.Env {
		env[e.Name] = e
	}
	if env["DOCKER_HOST"].Value != "unix://"+DockerSocket || env["POD_INDEX"].ValueFrom == nil {
		t.Errorf("env %v", runner.Env)
	}
	if got := GracePeriodSeconds(r); *spec.TerminationGracePeriodSeconds != got || got != 630 {
		t.Errorf("grace period %d", *spec.TerminationGracePeriodSeconds)
	}
	if *spec.AutomountServiceAccountToken {
		t.Error("service account token mounted")
	}
}

func TestRegisterJobKeepsSecretsOutOfTheSpec(t *testing.T) {
	r := testRunner(func(r *forgejov1.ForgejoRunner) { r.Spec.Scope.Organization = testOrg })
	f := testForgejo()
	t0, _ := NewToken()
	t1, _ := NewToken()
	job := RegisterJob(RegisterJobInput{
		Runner: r, Forgejo: f, Image: testImage17, AppIniSecret: testAppIniSecret,
		Slots: []Slot{{Index: 0, Name: testSlotName, Token: t0, Version: "13.2.0"}, {Index: 1, Name: "ci-1", Token: t1, Version: "1.0 $(id)"}},
	})
	b, _ := json.Marshal(job)
	for _, tok := range []string{t0, t1, t0[16:], t1[16:]} {
		if strings.Contains(string(b), tok) {
			t.Fatal("a runner secret is in the Job")
		}
	}
	c := job.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	// The listed version is passed on; one that is not a plain version string is not.
	if env["RUNNERS"] != "0:13.2.0:ci-0 1::ci-1" || env["RUNNER_SCOPE"] != testOrg {
		t.Errorf("env %v", env)
	}
	if c.Image != testImage17 || *c.SecurityContext.RunAsUser != 1000 || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("container %+v", c)
	}
	var tokensVol, configVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		v := &job.Spec.Template.Spec.Volumes[i]
		switch v.Name {
		case volTokens:
			tokensVol = v
		case volConfig:
			configVol = v
		}
	}
	if configVol == nil || configVol.Secret.SecretName != testAppIniSecret {
		t.Errorf("app.ini volume %+v", configVol)
	}
	if tokensVol == nil || len(tokensVol.Secret.Items) != 2 || tokensVol.Secret.Items[1].Key != "token-1" {
		t.Errorf("tokens volume %+v", tokensVol)
	}
	got := ParseRegistrationAnnotation(job.Annotations[AnnotationRegistrationRunners])
	if got[0] != UUIDOf(t0) || got[1] != UUIDOf(t1) || job.Annotations[AnnotationRegistrationScope] != testOrg {
		t.Errorf("annotations %v", job.Annotations)
	}
	if v := restrictedViolations(job.Spec.Template.Spec); len(v) > 0 {
		t.Errorf("registration pod violates restricted: %v", v)
	}
	// The controller deletes the Job once it is recorded: a TTL could remove a succeeded Job,
	// the only trace of its registrations, before that.
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("registration Job has a TTL of %d s", *job.Spec.TTLSecondsAfterFinished)
	}
}

func TestSafeVersion(t *testing.T) {
	for _, v := range []string{"13.2.0", "v12.7.0-rc1", "1.0+dev_2", "dev"} {
		if SafeVersion(v) != v {
			t.Errorf("SafeVersion(%q) = %q", v, SafeVersion(v))
		}
	}
	for _, v := range []string{"", "1.0 2", "1:2", "$(id)", "-1", ".1", "1;2", "a\nb", strings.Repeat("1", 65)} {
		if got := SafeVersion(v); got != "" {
			t.Errorf("SafeVersion(%q) = %q, want \"\"", v, got)
		}
	}
}

// The registration pod is placed like the instance's pods (node selector, tolerations, node
// affinity, priority class, service account for image pulls), without the instance's pod
// (anti-)affinity and without a service account token.
func TestRegisterJobInstancePlacement(t *testing.T) {
	f := testForgejo()
	nodeAffinity := &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"arm64"},
		}}}},
	}}
	f.Spec.PodTemplate = forgejov1.PodTemplateSpec{
		NodeSelector:       map[string]string{"pool": testPool},
		Tolerations:        []corev1.Toleration{{Key: testTaintKey, Value: testPool, Effect: corev1.TaintEffectNoSchedule}},
		PriorityClassName:  "git-critical",
		ServiceAccountName: "git-pull",
		Affinity: &corev1.Affinity{
			NodeAffinity: nodeAffinity,
			PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "forgejo"}},
			}}},
			PodAffinity: &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": "git-db"}},
			}}},
		},
	}
	tok, _ := NewToken()
	job := RegisterJob(RegisterJobInput{Runner: testRunner(), Forgejo: f, Image: testImage17,
		AppIniSecret: testAppIniSecret, Slots: []Slot{{Index: 0, Name: testSlotName, Token: tok}}})
	spec := job.Spec.Template.Spec
	if spec.NodeSelector["pool"] != testPool || len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != testTaintKey ||
		spec.PriorityClassName != "git-critical" || spec.ServiceAccountName != "git-pull" {
		t.Errorf("placement: nodeSelector %v, tolerations %v, priorityClass %q, serviceAccount %q",
			spec.NodeSelector, spec.Tolerations, spec.PriorityClassName, spec.ServiceAccountName)
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != "arm64" {
		t.Errorf("node affinity %+v", spec.Affinity)
	}
	if spec.Affinity != nil && (spec.Affinity.PodAffinity != nil || spec.Affinity.PodAntiAffinity != nil) {
		t.Errorf("the instance's pod (anti-)affinity was copied: %+v", spec.Affinity)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the service account token is mounted")
	}
	// Copies, not aliases of the Forgejo's spec.
	spec.NodeSelector["pool"] = testOther
	spec.Tolerations[0].Key = testOther
	if f.Spec.PodTemplate.NodeSelector["pool"] != testPool || f.Spec.PodTemplate.Tolerations[0].Key != testTaintKey {
		t.Error("the Job shares the Forgejo's node selector or tolerations")
	}

	// Without a pod template: no constraints, the namespace's default service account.
	job = RegisterJob(RegisterJobInput{Runner: testRunner(), Forgejo: testForgejo(), Image: testImage17,
		AppIniSecret: testAppIniSecret, Slots: []Slot{{Index: 0, Name: testSlotName, Token: tok}}})
	if spec := job.Spec.Template.Spec; spec.NodeSelector != nil || spec.Tolerations != nil || spec.Affinity != nil ||
		spec.ServiceAccountName != "" || spec.PriorityClassName != "" {
		t.Errorf("placement without a pod template: %+v", spec)
	}
}

// The registration script runs with a fake Forgejo CLI: it registers every runner, and when
// Forgejo prints a runner record with its secret (models/actions.RegisterRunner on a failed
// update), neither the logs nor the termination message show the secret.
func TestRegisterScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "0123456789abcdef0123456789abcdef01234567"
	for _, i := range []string{"0", "1"} {
		if err := os.WriteFile(filepath.Join(tokens, "token-"+i), []byte(secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(dir, "forgejo")
	fake := `#!/bin/sh
# args: --config C forgejo-cli actions register --secret-file F --scope S --name N --version V --keep-labels
name=""; file=""; version="unset"
while [ $# -gt 0 ]; do case "$1" in --name) name=$2; shift;; --secret-file) file=$2; shift;; --version) version=$2; shift;; esac; shift; done
if [ "$name" = "fail-1" ]; then echo "error while registering runner: can't update the runner {Token:$(cat "$file") Name:$name}" >&2; exit 1; fi
printf "uuid-of-%s@%s" "$name" "$version"
`
	if err := os.WriteFile(cli, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	termLog := filepath.Join(dir, "termination-log")
	run := func(runners string) (string, string, error) {
		script := strings.ReplaceAll(registerScriptFor(cli, tokens), "/dev/termination-log", termLog)
		script = strings.ReplaceAll(script, "/tmp/register.err", filepath.Join(dir, "register.err"))
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "RUNNERS="+runners, "RUNNER_SCOPE=", "GITEA_APP_INI=/dev/null")
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	// Each runner is registered with its own secret file, name and version (empty: none).
	out, _, err := run("0:13.2.0:ok-0 1::ok-1")
	if err != nil || !strings.Contains(out, "Registered runner ok-0 (uuid-of-ok-0@13.2.0)") || !strings.Contains(out, "Registered runner ok-1 (uuid-of-ok-1@)") {
		t.Fatalf("success run: %v %q", err, out)
	}
	out, errOut, err := run("0::ok-0 1::fail-1")
	if err == nil {
		t.Fatal("a failed registration exited 0")
	}
	msg, _ := os.ReadFile(termLog)
	for name, s := range map[string]string{"stdout": out, "stderr": errOut, "termination message": string(msg)} {
		if strings.Contains(s, secret) {
			t.Errorf("%s shows the secret: %q", name, s)
		}
	}
	if !strings.Contains(string(msg), "Could not register runner fail-1") || !strings.Contains(errOut, "<redacted>") {
		t.Errorf("termination message %q, stderr %q", msg, errOut)
	}
}
