package autopilot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	// postgresDefaultMaxConnections is PostgreSQL's (and CNPG's) default max_connections.
	postgresDefaultMaxConnections = 100
	// reservedConnections are kept free for superuser/CNPG, backups and init containers.
	reservedConnections = 10
	maxOpenConnsCap     = 50
	// connMaxLifetime recycles connections so pools follow a CNPG switchover quickly.
	connMaxLifetime = "5m"
	// gomemlimitPercent of the memory limit leaves headroom for non-heap memory (stacks,
	// cgo, git child processes run in the same cgroup).
	gomemlimitPercent = 85
)

// ReconcileTuning computes rc.Tuning, rc.TuningEnv and rc.ResourcesOverride. It also decides
// rc.HPAManaged here because the workload step, which honors it, runs before the scaling step.
func ReconcileTuning(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	planVertical(rc)
	rc.HPAManaged = hpaWanted(rc)

	if !forgejo.AutoTuningEnabled(f) {
		if ap := f.Status.AutoPilot; ap != nil {
			ap.Tuning = nil
		}
		return forgejo.StepResult{}, nil
	}

	res := effectiveResources(rc)
	tuning := map[string]map[string]string{}
	mirror := map[string]string{}
	set := func(section, key, value string) {
		if userConfigured(f, section, key) {
			return
		}
		if tuning[section] == nil {
			tuning[section] = map[string]string{}
		}
		tuning[section][key] = value
		mirror[section+"."+key] = value
	}

	open := dbMaxOpenConns(f)
	set("database", "MAX_OPEN_CONNS", strconv.Itoa(open))
	set("database", "MAX_IDLE_CONNS", strconv.Itoa(max(2, open/2)))
	set("database", "CONN_MAX_LIFETIME", connMaxLifetime)

	// Forgejo defaults [queue] MAX_WORKERS to NumCPU/2 clamped to 1..10, where NumCPU is the
	// node's CPU count rather than the container's budget.
	if cpu := cpuBudget(res); cpu > 0 {
		set("queue", "MAX_WORKERS", strconv.Itoa(min(10, max(1, cpu/2000))))
	}

	// Go 1.25+ derives GOMAXPROCS from the CPU limit, but nothing derives GOMEMLIMIT.
	if lim := res.Limits.Memory(); !lim.IsZero() && !userEnv(f, "GOMEMLIMIT") {
		mib := lim.Value() * gomemlimitPercent / 100 >> 20
		v := fmt.Sprintf("%dMiB", max(mib, 16))
		rc.TuningEnv = append(rc.TuningEnv, corev1.EnvVar{Name: "GOMEMLIMIT", Value: v})
		mirror["env.GOMEMLIMIT"] = v
	}

	if len(tuning) > 0 {
		rc.Tuning = tuning
	}
	autoPilotStatus(rc).Tuning = mirror
	return forgejo.StepResult{}, nil
}

// dbMaxOpenConns splits PostgreSQL's connection budget across the largest number of
// Forgejo pods that can run at once. It depends only on the spec, so transient states
// (upgrades, restores, HPA decisions) never change app.ini.
func dbMaxOpenConns(f *forgejov1.Forgejo) int {
	maxConns := postgresDefaultMaxConnections
	if forgejo.DatabaseType(f) == forgejov1.DatabaseTypeCNPG {
		if v, err := strconv.Atoi(strings.TrimSpace(f.Spec.Database.CNPG.Parameters["max_connections"])); err == nil && v > 0 {
			maxConns = v
		}
	}
	pods := int(maxPods(f))
	if f.Spec.HighAvailability.Enabled {
		pods++ // rolling updates surge one extra pod
	}
	return min(maxOpenConnsCap, max(2, (maxConns-reservedConnections)/pods))
}

// cpuBudget returns the container's CPU limit (or request) in millicores, 0 when unset.
func cpuBudget(res corev1.ResourceRequirements) int {
	if q := res.Limits.Cpu(); !q.IsZero() {
		return int(q.MilliValue())
	}
	if q := res.Requests.Cpu(); !q.IsZero() {
		return int(q.MilliValue())
	}
	return 0
}

func userConfigured(f *forgejov1.Forgejo, section, key string) bool {
	for s, kv := range f.Spec.Config {
		if !strings.EqualFold(s, section) {
			continue
		}
		for k := range kv {
			if strings.EqualFold(k, key) {
				return true
			}
		}
	}
	for _, c := range f.Spec.ConfigFrom {
		if strings.EqualFold(c.Section, section) && strings.EqualFold(c.Key, key) {
			return true
		}
	}
	return false
}

func userEnv(f *forgejov1.Forgejo, name string) bool {
	for _, e := range f.Spec.Env {
		if e.Name == name {
			return true
		}
	}
	return false
}
