package forgejo

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func namespaceTerminating() error {
	return NewNamespaceTerminatingError("persistentvolumeclaims", "fj-data", "ns")
}

func TestIsNamespaceTerminating(t *testing.T) {
	terminating := namespaceTerminating()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"admission error":        {terminating, true},
		"wrapped twice":          {fmt.Errorf("step %q: %w", "storage", fmt.Errorf("creating PVC: %w", terminating)), true},
		"joined":                 {errors.Join(fmt.Errorf("ServiceMonitor fj: %w", terminating)), true},
		"nil":                    {nil, false},
		"plain error":            {errors.New("namespace is being terminated"), false},
		"wrapped without %w":     {fmt.Errorf("creating PVC: %v", terminating), false},
		"RBAC forbidden":         {apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "x", errors.New("no permission")), false},
		"other cause":            {apierrors.NewInvalid(schema.GroupKind{Kind: "Job"}, "x", nil), false},
		"conflict":               {apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New("stale")), false},
		"not found in namespace": {apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "ns"), false},
	} {
		if got := IsNamespaceTerminating(tc.err); got != tc.want {
			t.Errorf("%s: IsNamespaceTerminating(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

// capture returns a logger writing to lines at verbosity v, with the caller of each line.
func capture(v int) (logr.Logger, *[]string) {
	lines := &[]string{}
	l := funcr.New(func(prefix, args string) { *lines = append(*lines, prefix+" "+args) },
		funcr.Options{Verbosity: v, LogCaller: funcr.All})
	return l, lines
}

func TestQuietTerminatingNamespaces(t *testing.T) {
	base, lines := capture(1)
	l := QuietTerminatingNamespaces(base).WithName("events").WithValues("k", "v")

	l.Error(fmt.Errorf("wrapped: %w", namespaceTerminating()), "Server rejected event (will not retry!)")
	l.Error(errors.New("boom"), "Reconciler error")
	l.Info("hello")
	if len(*lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(*lines), *lines)
	}
	rejected, other, info := (*lines)[0], (*lines)[1], (*lines)[2]
	if !strings.Contains(rejected, `"level"=1`) || strings.Contains(rejected, `"error"=null`) ||
		!strings.Contains(rejected, "being terminated") || !strings.Contains(rejected, `"k"="v"`) ||
		!strings.HasPrefix(rejected, "events ") {
		t.Errorf("terminating-namespace error not written as a V(1) info line: %s", rejected)
	}
	if !strings.Contains(other, `"error"="boom"`) || strings.Contains(other, `"level"`) {
		t.Errorf("other error was changed: %s", other)
	}
	if !strings.Contains(info, `"level"=0`) || !strings.Contains(info, `"msg"="hello"`) {
		t.Errorf("info line was changed: %s", info)
	}
	// The wrapper adds a stack frame; the reported caller must still be this file.
	for _, line := range *lines {
		if !strings.Contains(line, "terminating_test.go") {
			t.Errorf("caller is not the logging call site: %s", line)
		}
	}

	// At the default verbosity the downgraded line is not written at all.
	base, lines = capture(0)
	QuietTerminatingNamespaces(base).Error(namespaceTerminating(), "Server rejected event (will not retry!)")
	if len(*lines) != 0 {
		t.Errorf("V(1) line written at verbosity 0: %q", *lines)
	}
	if l := QuietTerminatingNamespaces(logr.Discard()); l.GetSink() != nil {
		t.Error("discard logger got a sink")
	}
}
