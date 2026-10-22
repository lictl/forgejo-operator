package upgrade

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Event reasons of the version support check.
const (
	reasonUnsupportedVersion = "UnsupportedVersion"
	reasonSupportedVersion   = "SupportedVersion"
	reasonVersionAdvisory    = "VersionAdvisory"
)

// instanceSupport classifies the Forgejo version the instance runs: status.image (the image
// verified running), or the image being deployed before the first rollout, refined by the
// version Forgejo reports (status.version).
func instanceSupport(f *forgejov1.Forgejo, targetImage string, now time.Time) (string, forgejo.Support) {
	image := f.Status.Image
	if image == "" {
		image = targetImage
	}
	if image == "" {
		image = forgejo.Image(f)
	}
	v, ok := forgejo.InstanceVersion(image, f.Status.Version)
	return image, forgejo.Versions.Classify(v, ok, now)
}

// ReportVersionSupport sets the VersionSupported condition and the version support metrics,
// and emits an event when the classification changes: a Warning when the instance runs a
// Deprecated, EndOfLife or Future version or a patch release with an advisory, a Normal
// event when it is supported again after one of those. It never blocks anything. It runs
// after core's status step, so that it sees this pass's status.image.
func ReportVersionSupport(rc *forgejo.ReconcileContext, now time.Time) {
	f := rc.Instance
	image, s := instanceSupport(f, rc.TargetImage, now)
	status := metav1.ConditionFalse
	switch s.Level {
	case forgejo.SupportSupported:
		status = metav1.ConditionTrue
	case forgejo.SupportUnknown:
		status = metav1.ConditionUnknown
	}
	msg := s.Message
	if s.Level == forgejo.SupportUnknown {
		msg = "Image " + image + ": " + msg
	}

	prev := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionVersionSupported)
	if prev == nil || prev.Reason != string(s.Level) || prev.Message != msg {
		switch {
		case s.Level == forgejo.SupportDeprecated || s.Level == forgejo.SupportEndOfLife || s.Level == forgejo.SupportFuture:
			rc.Event(corev1.EventTypeWarning, reasonUnsupportedVersion, eventAction, "%s", msg)
		case s.Advisory != nil:
			rc.Event(corev1.EventTypeWarning, reasonVersionAdvisory, eventAction, "%s", msg)
		case s.Level == forgejo.SupportSupported && prev != nil && prev.Status == metav1.ConditionFalse:
			rc.Event(corev1.EventTypeNormal, reasonSupportedVersion, eventAction, "%s", msg)
		}
	}
	rc.SetCondition(forgejov1.ConditionVersionSupported, status, string(s.Level), msg)
	exportSupport(f, s)
}

// ExportVersionSupport only exports the metrics, for a paused instance whose status is not
// reconciled.
func ExportVersionSupport(f *forgejov1.Forgejo, now time.Time) {
	_, s := instanceSupport(f, "", now)
	exportSupport(f, s)
}

func exportSupport(f *forgejov1.Forgejo, s forgejo.Support) {
	l := prometheus.Labels{"namespace": f.Namespace, "name": f.Name}
	forgejo.VersionSupported.DeletePartialMatch(l)
	forgejo.VersionEndOfLife.DeletePartialMatch(l)
	major := ""
	if s.Version.Major >= 0 {
		major = strconv.Itoa(s.Version.Major)
	}
	v := 0.0
	if s.Level == forgejo.SupportSupported {
		v = 1
	}
	forgejo.VersionSupported.WithLabelValues(f.Namespace, f.Name, major, string(s.Level)).Set(v)
	if s.Release != nil {
		forgejo.VersionEndOfLife.WithLabelValues(f.Namespace, f.Name, major).Set(float64(s.Release.EOL.Unix()))
	}
}
