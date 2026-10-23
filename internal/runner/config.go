package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"sigs.k8s.io/yaml"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	labelHost    = "host:host"
	logLevelInfo = "info"
	configRunner = "runner"
)

// Paths inside the runner container.
const (
	// ConfigDir holds the configuration files of all runners of the StatefulSet; pod i reads
	// ConfigKey(i).
	ConfigDir = "/etc/forgejo-runner/config"
	// TokensDir holds the runners' secrets (TokenKey(i)), referenced with token_url.
	TokensDir = "/etc/forgejo-runner/tokens"
	// DataDir is $HOME and the working directory (emptyDir).
	DataDir = "/data"
	// CacheDir holds the built-in cache server's data (emptyDir).
	CacheDir = "/data/cache"
	// DockerSocketDir holds the socket of the Docker executor's daemon (emptyDir shared
	// with the runner; dockerd runs with --group 1000 so that the runner can use it).
	DockerSocketDir = "/var/run/dind"
	// DockerSocket is the daemon's socket.
	DockerSocket = DockerSocketDir + "/docker.sock"
	// ConnectionName is the key of the runner's only entry in server.connections.
	ConnectionName = "forgejo"
)

// DefaultShutdownTimeout is spec.shutdownTimeout when unset.
const DefaultShutdownTimeout = 10 * time.Minute

// Executor returns spec.executor. The API server requires it; anything but Docker (an object
// that never passed validation) is taken as Host, so that the privileged Docker daemon is only
// rendered when it is asked for.
func Executor(r *forgejov1.ForgejoRunner) forgejov1.RunnerExecutor {
	if r.Spec.Executor == forgejov1.RunnerExecutorDocker {
		return forgejov1.RunnerExecutorDocker
	}
	return forgejov1.RunnerExecutorHost
}

// DesiredReplicas returns spec.replicas with its default (1).
func DesiredReplicas(r *forgejov1.ForgejoRunner) int32 {
	if r.Spec.Replicas == nil {
		return 1
	}
	return max(*r.Spec.Replicas, 0)
}

// Capacity returns spec.capacity with its default (1).
func Capacity(r *forgejov1.ForgejoRunner) int32 {
	return max(r.Spec.Capacity, 1)
}

// EffectiveLabels returns spec.labels, or the executor's default label.
func EffectiveLabels(r *forgejov1.ForgejoRunner) []string {
	if len(r.Spec.Labels) > 0 {
		return r.Spec.Labels
	}
	if Executor(r) == forgejov1.RunnerExecutorHost {
		return []string{labelHost}
	}
	return []string{"docker:docker://" + forgejov1.DefaultRunnerJobImage}
}

// LabelNames are the names of the labels ("docker" of "docker:docker://node:22"), as Forgejo
// matches them against runs-on.
func LabelNames(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		name := l
		for i := 0; i < len(l); i++ {
			if l[i] == ':' {
				name = l[:i]
				break
			}
		}
		out = append(out, name)
	}
	return out
}

// ShutdownTimeout returns spec.shutdownTimeout with its default.
func ShutdownTimeout(r *forgejov1.ForgejoRunner) time.Duration {
	return forgejo.DurationOr(r.Spec.ShutdownTimeout, DefaultShutdownTimeout)
}

// CacheEnabled returns spec.cache.enabled with its default (true).
func CacheEnabled(r *forgejov1.ForgejoRunner) bool { return forgejo.BoolOr(r.Spec.Cache.Enabled, true) }

// ServerURL is the URL the runners connect to: the instance's in-cluster HTTP Service.
func ServerURL(f *forgejov1.Forgejo) string { return forgejo.InternalHTTPURL(f) + "/" }

// RenderConfig returns the configuration file of runner i (uuid is its UUID in Forgejo).
//
// It is built in three layers: the operator's defaults, spec.config on top (deep merge), and
// the keys the operator owns on top of that: server (the connection to the instance, whose
// secret is read from a file and never written into the configuration), runner.labels,
// runner.capacity and runner.shutdown_timeout (the pod's grace period is derived from it).
func RenderConfig(r *forgejov1.ForgejoRunner, f *forgejov1.Forgejo, i int32, uuid string) ([]byte, error) {
	return render(r, f, uuid, TokensDir+"/"+TokenKey(i))
}

// ConfigHash is a digest of the configuration shared by all runners of r (everything but the
// UUID and the path of the secret). It is put on the pod template, so that a change rolls
// the pods, while adding a runner does not.
func ConfigHash(r *forgejov1.ForgejoRunner, f *forgejov1.Forgejo) (string, error) {
	b, err := render(r, f, "<uuid>", TokensDir+"/<token>")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

func render(r *forgejov1.ForgejoRunner, f *forgejov1.Forgejo, uuid, tokenFile string) ([]byte, error) {
	cfg := map[string]any{
		"log": map[string]any{"level": logLevelInfo, "job_level": logLevelInfo},
		"cache": map[string]any{
			"enabled": CacheEnabled(r),
			"dir":     CacheDir,
		},
	}
	if Executor(r) == forgejov1.RunnerExecutorDocker {
		dockerHost := "-"
		if r.Spec.Docker.MountSocket {
			dockerHost = "automount"
		}
		cfg["container"] = map[string]any{"docker_host": dockerHost}
	}
	if r.Spec.Config != nil && len(r.Spec.Config.Raw) > 0 {
		user := map[string]any{}
		if err := json.Unmarshal(r.Spec.Config.Raw, &user); err != nil {
			return nil, fmt.Errorf("spec.config: %w", err)
		}
		cfg = merge(cfg, user)
	}
	labels := EffectiveLabels(r)
	cfg = merge(cfg, map[string]any{
		configRunner: map[string]any{
			"capacity":         Capacity(r),
			"labels":           labels,
			"shutdown_timeout": ShutdownTimeout(r).String(),
		},
	})
	cfg["server"] = map[string]any{
		"connections": map[string]any{
			ConnectionName: map[string]any{
				"url":       ServerURL(f),
				"uuid":      uuid,
				"token_url": "file:" + tokenFile,
				"labels":    labels,
			},
		},
	}
	return yaml.Marshal(cfg)
}

// merge returns base with over deep-merged into it: maps are merged key by key, any other
// value of over replaces the one of base.
func merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	maps.Copy(out, base)
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = merge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}
