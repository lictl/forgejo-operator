package forgejo

import (
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// IsNamespaceTerminating reports whether err, or an error it wraps (fmt.Errorf %w,
// errors.Join), is the API server refusing to create an object because its namespace is
// being deleted: the NamespaceLifecycle admission plugin's 403 Forbidden with cause
// NamespaceTerminating. Updates and deletes are still allowed then, so only creates fail.
//
// A namespace never leaves Terminating, every later create fails the same way, and the
// namespace's deletion removes the operator's objects in it. Like the kube-controller-manager
// controllers that create objects (ReplicaSet, Job, Deployment, Endpoints, ...), the operator
// treats it as an end state, not a failure: no error return, backoff, Warning event or
// Degraded condition.
func IsNamespaceTerminating(err error) bool {
	return apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)
}

// NewNamespaceTerminatingError returns the error the NamespaceLifecycle admission plugin
// returns for a create of name in namespace while that namespace is being deleted. Tests use
// it to stand in for the API server.
func NewNamespaceTerminatingError(resource, name, namespace string) *apierrors.StatusError {
	msg := fmt.Sprintf("namespace %s is being terminated", namespace)
	err := apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name,
		fmt.Errorf("unable to create new content in namespace %s because it is being terminated", namespace))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type: corev1.NamespaceTerminatingCause, Message: msg, Field: "metadata.namespace",
	})
	return err
}

// QuietTerminatingNamespaces returns l with its errors caused by a terminating namespace
// (IsNamespaceTerminating) written as V(1) info lines.
//
// The controllers do not report these errors themselves, but the events.k8s.io broadcaster
// behind mgr.GetEventRecorder logs every event the API server rejects at error level
// ("Server rejected event (will not retry!)"), and an event emitted while a namespace is being
// deleted is such a rejection. The legacy core/v1 recorder logs them at V(5) (client-go
// PR 114849); controller-runtime gives no way to replace the newer broadcaster, but it logs
// through the manager's logger, i.e. this one. Cilium wraps its sink for the same reason.
// The rejected event is lost either way; events in other namespaces are untouched.
func QuietTerminatingNamespaces(l logr.Logger) logr.Logger {
	sink := l.GetSink()
	if sink == nil {
		return l
	}
	if cd, ok := sink.(logr.CallDepthLogSink); ok {
		sink = cd.WithCallDepth(1) // terminatingSink adds one frame
	}
	return l.WithSink(terminatingSink{sink})
}

// terminatingSink downgrades errors caused by a terminating namespace to V(1) info lines.
// Info and Error are implemented explicitly so that both add exactly one stack frame.
type terminatingSink struct{ logr.LogSink }

func (s terminatingSink) Info(level int, msg string, kv ...any) {
	s.LogSink.Info(level, msg, kv...)
}

func (s terminatingSink) Error(err error, msg string, kv ...any) {
	if !IsNamespaceTerminating(err) {
		s.LogSink.Error(err, msg, kv...)
		return
	}
	if s.Enabled(1) {
		s.LogSink.Info(1, msg, append(kv[:len(kv):len(kv)], "error", err.Error())...)
	}
}

func (s terminatingSink) WithValues(kv ...any) logr.LogSink {
	return terminatingSink{s.LogSink.WithValues(kv...)}
}

func (s terminatingSink) WithName(name string) logr.LogSink {
	return terminatingSink{s.LogSink.WithName(name)}
}

func (s terminatingSink) WithCallDepth(depth int) logr.LogSink {
	if cd, ok := s.LogSink.(logr.CallDepthLogSink); ok {
		return terminatingSink{cd.WithCallDepth(depth)}
	}
	return s
}
