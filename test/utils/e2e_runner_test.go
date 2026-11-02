package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests mock commands, not Kubernetes behavior: they prove the E2E runner
// refuses existing clusters and never passes the caller's kubeconfig to tests.
func TestE2ERunnerIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, existing, createExit, flattenExit, testExit, deleteExit string
		wantRun, wantDelete, wantRecovery                             bool
		wantExit                                                      int
	}{
		{name: "existing cluster", existing: "release-proof", wantExit: 1},
		{name: "prefix is not the same cluster", existing: "release-proof-other", wantRun: true, wantDelete: true},
		{name: "partial create failed", createExit: "1", wantDelete: true, wantExit: 1},
		{name: "flatten failed", flattenExit: "1", wantDelete: true, wantExit: 1},
		{name: "test failed", testExit: "42", wantRun: true, wantDelete: true, wantExit: 42},
		{name: "cleanup failed", deleteExit: "7", wantRun: true, wantDelete: true, wantExit: 7, wantRecovery: true},
		{name: "test and cleanup failed", testExit: "42", deleteExit: "7",
			wantRun: true, wantDelete: true, wantExit: 42, wantRecovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeExecutable(t, dir, "kind", `#!/bin/bash
set -eu
echo "kind $*" >> "$CALLS"
case "$1" in
get) printf '%s\n' "$EXISTING" ;;
create)
  [[ "$KUBECONFIG" != "$ORIGINAL_CONFIG" ]]
  touch "$RESIDUAL_NODE"
  printf '%s' "$KUBECONFIG" > "$RECOVERY_CONFIG_PATH"
  exit "${CREATE_EXIT:-0}"
  ;;
export|delete)
  # Model Kind touching kubeconfig during cleanup; a runner regression damages
  # the sentinel so the test checks the actual side effect, not only argv.
  printf cleanup > "$KUBECONFIG"
  [[ "$KUBECONFIG" != "$ORIGINAL_CONFIG" ]]
  if [[ "$1" == delete ]]; then
    if [[ "${DELETE_EXIT:-0}" != 0 ]]; then exit "$DELETE_EXIT"; fi
    if [[ -e "$RESIDUAL_NODE" ]]; then unlink "$RESIDUAL_NODE"; fi
  fi
  ;;
esac
`)
			writeExecutable(t, dir, "kubectl", `#!/bin/bash
set -eu
[[ "$KUBECONFIG" != "$ORIGINAL_CONFIG" ]]
echo isolated
exit "${FLATTEN_EXIT:-0}"
`)
			writeExecutable(t, dir, "go", `#!/bin/bash
set -eu
[[ "$KUBECONFIG" != "$ORIGINAL_CONFIG" ]]
[[ "$(cat "$KUBECONFIG")" == isolated ]]
[[ "$FORGEJO_E2E_ISOLATED_CLUSTER" == release-proof ]]
echo go >> "$CALLS"
exit "${TEST_EXIT:-0}"
`)
			original := filepath.Join(dir, "original")
			if err := os.WriteFile(original, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			cmd := exec.Command("bash", "../../hack/test-e2e.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "KIND="+filepath.Join(dir, "kind"),
				"KIND_CLUSTER=release-proof", "EXISTING="+tc.existing, "CREATE_EXIT="+tc.createExit,
				"FLATTEN_EXIT="+tc.flattenExit, "TEST_EXIT="+tc.testExit, "DELETE_EXIT="+tc.deleteExit,
				"RESIDUAL_NODE="+filepath.Join(dir, "node"), "RECOVERY_CONFIG_PATH="+filepath.Join(dir, "config-path"),
				"CALLS="+calls, "KUBECONFIG="+original, "ORIGINAL_CONFIG="+original, "TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatalf("could not start runner: %v", err)
			}
			if code := cmd.ProcessState.ExitCode(); code != tc.wantExit {
				t.Fatalf("exit=%d, expected %d: %s", code, tc.wantExit, out)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), "\ngo\n") != tc.wantRun {
				t.Fatalf("unexpected test execution: %s", got)
			}
			if strings.Contains(string(got), "kind delete cluster --name release-proof") != tc.wantDelete {
				t.Fatalf("unexpected deletion: %s", got)
			}
			_, nodeErr := os.Stat(filepath.Join(dir, "node"))
			if (nodeErr == nil) != tc.wantRecovery {
				t.Fatalf("unexpected residual node: %v", nodeErr)
			}
			configPath, _ := os.ReadFile(filepath.Join(dir, "config-path"))
			_, configErr := os.Stat(string(configPath))
			if (configErr == nil) != tc.wantRecovery {
				t.Fatalf("unexpected recovery kubeconfig: %v", configErr)
			}
			if strings.Contains(string(out), "Retry cleanup: KUBECONFIG=") != tc.wantRecovery {
				t.Fatalf("unexpected recovery instructions: %s", out)
			}
			got, err = os.ReadFile(original)
			if err != nil || string(got) != "untouched" {
				t.Fatalf("caller kubeconfig changed: %q, %v", got, err)
			}
		})
	}
}

func writeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
