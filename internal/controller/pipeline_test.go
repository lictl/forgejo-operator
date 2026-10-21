package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// stubSteps returns the real step table with every step replaced by a recorder. outcome
// decides a step's result by name; the returned slice collects the steps that ran.
func stubSteps(outcome func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error)) ([]namedStep, *[]string) {
	ran := &[]string{}
	out := make([]namedStep, len(steps))
	for i, s := range steps {
		s := s
		out[i] = namedStep{name: s.name, needs: s.needs, fn: func(_ context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
			*ran = append(*ran, s.name)
			if outcome == nil {
				return forgejo.StepResult{}, nil
			}
			return outcome(s.name, rc)
		}}
	}
	return out, ran
}

func newRC() *forgejo.ReconcileContext {
	return &forgejo.ReconcileContext{Instance: &forgejov1.Forgejo{}}
}

func TestStepTableNeedsEarlierSteps(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range steps {
		for _, n := range s.needs {
			if !seen[n] {
				t.Errorf("step %q needs %q, which does not run before it", s.name, n)
			}
		}
		if seen[s.name] {
			t.Errorf("duplicate step %q", s.name)
		}
		seen[s.name] = true
	}
}

func TestPipelineAllSucceed(t *testing.T) {
	st, ran := stubSteps(nil)
	p := runPipeline(context.Background(), newRC(), st)
	if len(*ran) != len(steps) || p.err() != nil || p.conflict {
		t.Fatalf("ran %v, err %v, conflict %v", *ran, p.err(), p.conflict)
	}
}

// F45/F26: a failing network step must not stop scheduled backups, health or autopilot.
func TestPipelineIndependentStepsRunAfterError(t *testing.T) {
	boom := errors.New("admission webhook denied the Ingress")
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == "network" {
			return forgejo.StepResult{}, boom
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	for _, want := range []string{"scaling", "monitoring", stepBackupSchedule, "health", "autopilot"} {
		if !slices.Contains(*ran, want) {
			t.Errorf("%s did not run after the network step failed; ran %v", want, *ran)
		}
	}
	if !errors.Is(p.err(), boom) || !strings.Contains(p.err().Error(), `step "network"`) {
		t.Errorf("err = %v", p.err())
	}
}

// F57: a Valkey that is not ready blocks config and workload, not the restore handshake,
// backups or health.
func TestPipelineValkeyStop(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == "high-availability" {
			return forgejo.StepResult{Stop: true, RequeueAfter: 5 * time.Second}, nil
		}
		return forgejo.StepResult{RequeueAfter: time.Minute}, nil
	})
	rc := newRC()
	p := runPipeline(context.Background(), rc, st)
	want := []string{stepMaintenance, stepRestoreGuard, "secrets", "database", "high-availability",
		"network", "monitoring", stepBackupSchedule, "health"}
	if !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	if p.err() != nil {
		t.Errorf("err = %v", p.err())
	}
	if rc.RequeueAfter != 5*time.Second {
		t.Errorf("requeue = %v, want the Valkey wait", rc.RequeueAfter)
	}
	if got := p.blockedBy("autopilot"); !strings.Contains(got, `"high-availability" is waiting`) {
		t.Errorf("blockedBy(autopilot) = %q", got)
	}
}

// A database that is not ready skips the upgrade step; an upgrade in flight must still be
// visible to the backup schedule.
func TestPipelineInfersUpgradingWhenUpgradeSkipped(t *testing.T) {
	var upgradingAtSchedule bool
	st, ran := stubSteps(func(name string, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		switch name {
		case "database":
			return forgejo.StepResult{Stop: true}, nil
		case stepBackupSchedule:
			upgradingAtSchedule = rc.Upgrading
		}
		return forgejo.StepResult{}, nil
	})
	rc := newRC()
	rc.Instance.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseRollingOut}
	runPipeline(context.Background(), rc, st)
	if slices.Contains(*ran, stepUpgrade) || !slices.Contains(*ran, stepBackupSchedule) {
		t.Fatalf("ran %v", *ran)
	}
	if !upgradingAtSchedule {
		t.Error("rc.Upgrading was false for the backup schedule while status.upgrade is RollingOut")
	}

	rc = newRC()
	rc.Instance.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseSucceeded}
	runPipeline(context.Background(), rc, st)
	if upgradingAtSchedule {
		t.Error("rc.Upgrading was true after a finished upgrade")
	}
}

func TestPipelineConflictEndsPass(t *testing.T) {
	st, ran := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == "config" {
			return forgejo.StepResult{}, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New("stale"))
		}
		return forgejo.StepResult{}, nil
	})
	rc := newRC()
	p := runPipeline(context.Background(), rc, st)
	if !p.conflict || p.err() != nil || (*ran)[len(*ran)-1] != "config" || rc.RequeueAfter != time.Second {
		t.Errorf("conflict %v err %v ran %v requeue %v", p.conflict, p.err(), *ran, rc.RequeueAfter)
	}
}

// A failure inside the chain skips its dependants but keeps the failure as the reason.
func TestPipelineBlockedByNamesRootCause(t *testing.T) {
	st, _ := stubSteps(func(name string, _ *forgejo.ReconcileContext) (forgejo.StepResult, error) {
		if name == stepMaintenance {
			return forgejo.StepResult{}, errors.New("listing restores: forbidden")
		}
		return forgejo.StepResult{}, nil
	})
	p := runPipeline(context.Background(), newRC(), st)
	if p.completed[stepBackupSchedule] {
		t.Fatal("backup schedule ran without maintenance")
	}
	if got := p.blockedBy(stepBackupSchedule); !strings.Contains(got, `step "maintenance" failed: listing restores: forbidden`) {
		t.Errorf("blockedBy = %q", got)
	}
}
