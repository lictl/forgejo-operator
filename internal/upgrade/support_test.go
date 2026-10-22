package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	img11 = "codeberg.org/forgejo/forgejo:11"
	img15 = "codeberg.org/forgejo/forgejo:15"
	img16 = "codeberg.org/forgejo/forgejo:16"
	img17 = "codeberg.org/forgejo/forgejo:17"
	img18 = "codeberg.org/forgejo/forgejo:18"
)

var supportNow = time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)

func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func withPrefix(evs []string, prefix string) []string {
	var out []string
	for _, e := range evs {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

func supportContext(f *forgejov1.Forgejo) (*forgejo.ReconcileContext, *events.FakeRecorder) {
	rec := events.NewFakeRecorder(100)
	return &forgejo.ReconcileContext{Instance: f, Recorder: rec, TargetImage: f.Status.Image}, rec
}

func TestReportVersionSupport(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "vs", Namespace: "ns"},
		Spec: forgejov1.ForgejoSpec{Image: img16}, Status: forgejov1.ForgejoStatus{Image: img16}}
	rc, rec := supportContext(f)
	cond := func() *metav1.Condition {
		return meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionVersionSupported)
	}
	metric := func(major, support string) float64 {
		return gaugeValue(t, forgejo.VersionSupported.WithLabelValues("ns", "vs", major, support))
	}

	// 16 is still maintained upstream but outside the policy: Deprecated, one Warning.
	ReportVersionSupport(rc, supportNow)
	c := cond()
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Deprecated" || !strings.Contains(c.Message, "Upgrade to 17 before 2026-10-29") {
		t.Fatalf("16: %+v", c)
	}
	if evs := withPrefix(drain(rec), "Warning UnsupportedVersion"); len(evs) != 1 {
		t.Fatalf("16: want one UnsupportedVersion warning, got %q", evs)
	}
	if metric("16", "Deprecated") != 0 || seriesCount(forgejo.VersionSupported) != 1 {
		t.Fatal("16: metric")
	}
	if eol := gaugeValue(t, forgejo.VersionEndOfLife.WithLabelValues("ns", "vs", "16")); eol != float64(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC).Unix()) {
		t.Fatalf("16: EOL gauge %v", eol)
	}
	ReportVersionSupport(rc, supportNow)
	if evs := drain(rec); len(evs) != 0 {
		t.Fatalf("unchanged classification re-emitted %q", evs)
	}
	// Forgejo reporting its exact version does not change the message either.
	f.Status.Version = "16.0.6+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	if evs := drain(rec); len(evs) != 0 {
		t.Fatalf("the reported patch re-emitted %q", evs)
	}
	// On its end-of-life day it becomes EndOfLife, with a new warning.
	ReportVersionSupport(rc, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if c := cond(); c.Reason != "EndOfLife" || len(withPrefix(drain(rec), "Warning UnsupportedVersion")) != 1 {
		t.Fatalf("16 after EOL: %+v", c)
	}

	// Upgraded to 17: supported again (Normal event), but 17.0.0 carries an advisory.
	f.Status.Image, f.Status.Version = img17, "17.0.0+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	c = cond()
	if c.Status != metav1.ConditionTrue || c.Reason != "Supported" || !strings.Contains(c.Message, "17.0.1 or newer") {
		t.Fatalf("17.0.0: %+v", c)
	}
	evs := drain(rec)
	if len(withPrefix(evs, "Warning VersionAdvisory")) != 1 {
		t.Fatalf("17.0.0: want a VersionAdvisory warning, got %q", evs)
	}
	if metric("17", "Supported") != 1 {
		t.Fatal("17: metric")
	}
	if n := seriesCount(forgejo.VersionSupported); n != 1 {
		t.Fatalf("one series per instance, got %d", n)
	}
	f.Status.Version = "17.0.1+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	if evs := drain(rec); len(evs) != 0 || strings.Contains(cond().Message, "or newer") {
		t.Fatalf("17.0.1: events %q, message %q", evs, cond().Message)
	}

	// From Deprecated back to Supported: a Normal event.
	f.Status.Image, f.Status.Version = img16, ""
	ReportVersionSupport(rc, supportNow)
	drain(rec)
	f.Status.Image = img15
	ReportVersionSupport(rc, supportNow)
	if evs := drain(rec); len(withPrefix(evs, "Normal SupportedVersion")) != 1 {
		t.Fatalf("back to supported: %q", evs)
	}

	// A digest: Unknown (no event) until Forgejo reports its version.
	f.Status.Image = "codeberg.org/forgejo/forgejo@sha256:0123"
	ReportVersionSupport(rc, supportNow)
	if c := cond(); c.Status != metav1.ConditionUnknown || c.Reason != "Unknown" || !strings.Contains(c.Message, "sha256:0123") {
		t.Fatalf("digest: %+v", c)
	}
	if evs := drain(rec); len(evs) != 0 {
		t.Fatalf("Unknown emitted %q", evs)
	}
	f.Status.Version = "15.0.10+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	if c := cond(); c.Reason != "Supported" {
		t.Fatalf("digest with reported version: %+v", c)
	}

	// Before the first rollout the image being deployed is classified.
	g := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "ns"}, Spec: forgejov1.ForgejoSpec{Image: img18}}
	grc, grec := supportContext(g)
	grc.TargetImage = img18
	ReportVersionSupport(grc, supportNow)
	if c := meta.FindStatusCondition(g.Status.Conditions, forgejov1.ConditionVersionSupported); c == nil || c.Reason != "Future" {
		t.Fatalf("fresh 18: %+v", c)
	}
	if evs := withPrefix(drain(grec), "Warning UnsupportedVersion"); len(evs) != 1 {
		t.Fatalf("fresh 18: %q", evs)
	}
	forgejo.DeleteInstanceMetrics("ns", "vs")
	forgejo.DeleteInstanceMetrics("ns", "new")
}

// gate runs the upgrade step for an instance that runs from and requests to.
func gate(t *testing.T, from, to string, mutate func(*forgejov1.Forgejo)) (*forgejov1.Forgejo, *forgejo.ReconcileContext, []string) {
	t.Helper()
	f := fixture(nil)
	f.Spec.Image, f.Status.Image = to, from
	if mutate != nil {
		mutate(f)
	}
	c := newClient(t, f)
	rec := events.NewFakeRecorder(100)
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Recorder: rec, Instance: f, TargetImage: from}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	return f, rc, drain(rec)
}

func TestUpgradeGateAcrossMajors(t *testing.T) {
	// 15 (LTS) -> 17 skips 16: supported upstream, so it is one orchestrated upgrade.
	f, rc, evs := gate(t, img15, img17, nil)
	if f.Status.Upgrade == nil || f.Status.Upgrade.Phase != forgejov1.UpgradePhasePending || rc.TargetImage != img15 {
		t.Fatalf("15 -> 17: upgrade %+v, target %s", f.Status.Upgrade, rc.TargetImage)
	}
	if len(withPrefix(evs, "Normal MajorUpgrade Upgrading across major versions from 15 to 17, skipping 16;")) != 1 {
		t.Fatalf("15 -> 17 events: %q", evs)
	}
	if len(withPrefix(evs, "Warning")) != 0 {
		t.Fatalf("15 -> 17 warned: %q", evs)
	}

	// 11 -> 17 skips the LTS 15: refused like a downgrade, the old image keeps running.
	f, rc, evs = gate(t, img11, img17, nil)
	deg := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded)
	up := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading)
	if f.Status.Upgrade != nil || rc.TargetImage != img11 || deg == nil || deg.Reason != ReasonUpgradePathUnsupported ||
		up == nil || up.Reason != ReasonUpgradePathUnsupported || !strings.Contains(up.Message, "Upgrade to Forgejo 15 first") {
		t.Fatalf("11 -> 17: upgrade %+v, target %s, degraded %+v, upgrading %+v", f.Status.Upgrade, rc.TargetImage, deg, up)
	}
	if len(withPrefix(evs, "Warning UpgradePathUnsupported")) != 1 {
		t.Fatalf("11 -> 17 events: %q", evs)
	}
	// Choosing the intermediate LTS starts the upgrade and clears Degraded.
	f.Spec.Image = img15
	c := newClient(t, f)
	rc = &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: img11}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Upgrade == nil || meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionDegraded) {
		t.Fatalf("11 -> 15: upgrade %+v, conditions %+v", f.Status.Upgrade, f.Status.Conditions)
	}

	// 17 -> 15 is a downgrade, refused.
	f, rc, _ = gate(t, img17, img15, nil)
	if f.Status.Upgrade != nil || rc.TargetImage != img17 ||
		meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading).Reason != ReasonDowngradeRefused {
		t.Fatalf("17 -> 15: %+v", f.Status.Conditions)
	}

	// A major newer than the table is allowed, with a warning.
	f, _, evs = gate(t, img17, img18, nil)
	if f.Status.Upgrade == nil || len(withPrefix(evs, "Warning UnsupportedVersion Upgrading to "+img18+" anyway")) != 1 {
		t.Fatalf("17 -> 18: upgrade %+v, events %q", f.Status.Upgrade, evs)
	}

	// 16 -> 17 with a duration value 17 refuses: warned, not blocked.
	f, _, evs = gate(t, img16, img17, func(f *forgejov1.Forgejo) {
		f.Spec.Config = map[string]map[string]string{"server": {"PER_WRITE_TIMEOUT": "-1"}}
	})
	if f.Status.Upgrade == nil || len(withPrefix(evs, "Warning ConfigIncompatible")) != 1 ||
		!strings.Contains(withPrefix(evs, "Warning ConfigIncompatible")[0], "[server] PER_WRITE_TIMEOUT") {
		t.Fatalf("16 -> 17 with -1: upgrade %+v, events %q", f.Status.Upgrade, evs)
	}
}

// An image without a version in its reference (a digest, or a custom tag) is gated by the
// version Forgejo reports (status.version).
func TestUpgradeGateUsesReportedVersion(t *testing.T) {
	const digest = "codeberg.org/forgejo/forgejo@sha256:0123"
	reports := func(v string) func(*forgejov1.Forgejo) {
		return func(f *forgejov1.Forgejo) { f.Status.Version = v }
	}
	reason := func(f *forgejov1.Forgejo) string {
		if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading); c != nil {
			return c.Reason
		}
		return ""
	}

	// 14 -> 17 skips the LTS 15: refused as with a 14 tag.
	f, rc, evs := gate(t, digest, img17, reports("14.0.5+gitea-1.22.0"))
	if f.Status.Upgrade != nil || rc.TargetImage != digest || reason(f) != ReasonUpgradePathUnsupported ||
		!meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionDegraded) {
		t.Fatalf("digest 14 -> 17: upgrade %+v, target %s, conditions %+v", f.Status.Upgrade, rc.TargetImage, f.Status.Conditions)
	}
	if evs := withPrefix(evs, "Warning UpgradePathUnsupported"); len(evs) != 1 || !strings.Contains(evs[0], "(14.0.5)") {
		t.Fatalf("digest 14 -> 17 events: %q", evs)
	}
	// The same through a custom tag of a mirror.
	f, _, _ = gate(t, "registry.example/forgejo:prod", img17, reports("14.0.5+gitea-1.22.0"))
	if f.Status.Upgrade != nil || reason(f) != ReasonUpgradePathUnsupported {
		t.Fatalf("custom tag 14 -> 17: upgrade %+v, conditions %+v", f.Status.Upgrade, f.Status.Conditions)
	}

	// 17 -> 15 is a downgrade, refused.
	f, rc, _ = gate(t, digest, img15, reports("17.0.0+gitea-1.22.0"))
	if f.Status.Upgrade != nil || rc.TargetImage != digest || reason(f) != ReasonDowngradeRefused {
		t.Fatalf("digest 17 -> 15: upgrade %+v, conditions %+v", f.Status.Upgrade, f.Status.Conditions)
	}

	// 16 -> 17 is an orchestrated major upgrade, and the duration check knows 16 is lenient.
	f, _, evs = gate(t, digest, img17, func(f *forgejov1.Forgejo) {
		f.Status.Version = "16.0.6+gitea-1.22.0"
		f.Spec.Config = map[string]map[string]string{"server": {"PER_WRITE_TIMEOUT": "-1"}}
	})
	if f.Status.Upgrade == nil || f.Status.Upgrade.Phase != forgejov1.UpgradePhasePending ||
		len(withPrefix(evs, "Normal MajorUpgrade Upgrading across major versions from 16 to 17;")) != 1 ||
		len(withPrefix(evs, "Warning ConfigIncompatible")) != 1 || len(withPrefix(evs, "Warning UnverifiedVersion")) != 0 {
		t.Fatalf("digest 16 -> 17: upgrade %+v, events %q", f.Status.Upgrade, evs)
	}

	// The reported version can lag behind an image change, so it never turns a change into
	// a plain rollout: the same version, or an allowed downgrade, is orchestrated as before.
	for name, mutate := range map[string]func(*forgejov1.Forgejo){
		"same version": reports("17.0.0+gitea-1.22.0"),
		"allowed downgrade": func(f *forgejov1.Forgejo) {
			f.Status.Version = "17.0.0+gitea-1.22.0"
			f.Spec.Image = img15
			f.Spec.Upgrade.AllowDowngrade = true
		},
	} {
		f, rc, evs = gate(t, digest, img17, mutate)
		if f.Status.Upgrade == nil || f.Status.Upgrade.Phase != forgejov1.UpgradePhasePending || rc.TargetImage != digest ||
			len(withPrefix(evs, "Warning UnverifiedVersion The version of "+digest+" is only known from what Forgejo reports (17.0.0)")) != 1 {
			t.Fatalf("%s: upgrade %+v, target %s, events %q", name, f.Status.Upgrade, rc.TargetImage, evs)
		}
	}

	// Without a reported version nothing can be compared: orchestrated, as before.
	f, _, evs = gate(t, digest, img15, nil)
	if f.Status.Upgrade == nil || len(withPrefix(evs, "Warning UnverifiedVersion Cannot compare")) != 1 {
		t.Fatalf("digest without status.version: upgrade %+v, events %q", f.Status.Upgrade, evs)
	}
}
