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
	testSectionServer      = "server"
	testVersion170         = "17.0.0+gitea-1.22.0"
	testVersion171         = "17.0.1+gitea-1.22.0"
	testKeyPerWriteTimeout = "PER_WRITE_TIMEOUT"
)

const (
	img11 = "codeberg.org/forgejo/forgejo:11"
	img15 = "codeberg.org/forgejo/forgejo:15"
	img16 = "codeberg.org/forgejo/forgejo:16"
	img17 = "codeberg.org/forgejo/forgejo:17"
	img18 = "codeberg.org/forgejo/forgejo:18"
)

var supportNow = time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)

func withPrefix(evs []string, prefix string) []string {
	var out []string
	for _, e := range evs {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// metricValue reads a counter or a gauge.
func metricValue(t *testing.T, c prometheus.Metric) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	if m.Counter != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

func supportContext(f *forgejov1.Forgejo) *forgejo.ReconcileContext {
	return &forgejo.ReconcileContext{Instance: f, Recorder: events.NewFakeRecorder(100), TargetImage: f.Status.Image}
}

func TestReportVersionSupport(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "vs", Namespace: "ns"},
		Spec: forgejov1.ForgejoSpec{Image: img16}, Status: forgejov1.ForgejoStatus{Image: img16}}
	rc := supportContext(f)
	cond := func() *metav1.Condition {
		return meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionVersionSupported)
	}
	metric := func(major, support string) float64 {
		return metricValue(t, forgejo.VersionSupported.WithLabelValues("ns", "vs", major, support))
	}

	// 16 is still maintained upstream but outside the policy: Deprecated, one Warning.
	ReportVersionSupport(rc, supportNow)
	c := cond()
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Deprecated" || !strings.Contains(c.Message, "Upgrade to 17 (17.0.1 or newer) before 2026-10-29") {
		t.Fatalf("16: %+v", c)
	}
	if evs := withPrefix(recorded(rc), "Warning UnsupportedVersion"); len(evs) != 1 {
		t.Fatalf("16: want one UnsupportedVersion warning, got %q", evs)
	}
	if metric("16", "Deprecated") != 0 || seriesCount(forgejo.VersionSupported) != 1 {
		t.Fatal("16: metric")
	}
	if eol := metricValue(t, forgejo.VersionEndOfLife.WithLabelValues("ns", "vs", "16")); eol != float64(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC).Unix()) {
		t.Fatalf("16: EOL gauge %v", eol)
	}
	ReportVersionSupport(rc, supportNow)
	if evs := recorded(rc); len(evs) != 0 {
		t.Fatalf("unchanged classification re-emitted %q", evs)
	}
	// Forgejo reporting its exact version does not change the message either.
	f.Status.Version = "16.0.6+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	if evs := recorded(rc); len(evs) != 0 {
		t.Fatalf("the reported patch re-emitted %q", evs)
	}
	// On its end-of-life day it becomes EndOfLife, with a new warning.
	ReportVersionSupport(rc, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if c := cond(); c.Reason != "EndOfLife" || len(withPrefix(recorded(rc), "Warning UnsupportedVersion")) != 1 {
		t.Fatalf("16 after EOL: %+v", c)
	}

	// Upgraded to 17: supported again (Normal event), but 17.0.0 carries an advisory.
	f.Status.Image, f.Status.Version = img17, testVersion170
	ReportVersionSupport(rc, supportNow)
	c = cond()
	if c.Status != metav1.ConditionTrue || c.Reason != "Supported" || !strings.Contains(c.Message, "17.0.1 or newer") {
		t.Fatalf("17.0.0: %+v", c)
	}
	evs := recorded(rc)
	if len(withPrefix(evs, "Warning VersionAdvisory")) != 1 {
		t.Fatalf("17.0.0: want a VersionAdvisory warning, got %q", evs)
	}
	if metric("17", "Supported") != 1 {
		t.Fatal("17: metric")
	}
	if n := seriesCount(forgejo.VersionSupported); n != 1 {
		t.Fatalf("one series per instance, got %d", n)
	}
	f.Status.Version = testVersion171
	ReportVersionSupport(rc, supportNow)
	if evs := recorded(rc); len(evs) != 0 || strings.Contains(cond().Message, "or newer") {
		t.Fatalf("17.0.1: events %q, message %q", evs, cond().Message)
	}

	// From Deprecated back to Supported: a Normal event.
	f.Status.Image, f.Status.Version = img16, ""
	ReportVersionSupport(rc, supportNow)
	recorded(rc)
	f.Status.Image = img15
	ReportVersionSupport(rc, supportNow)
	if evs := recorded(rc); len(withPrefix(evs, "Normal SupportedVersion")) != 1 {
		t.Fatalf("back to supported: %q", evs)
	}

	// A digest: Unknown (no event) until Forgejo reports its version.
	f.Status.Image = "codeberg.org/forgejo/forgejo@sha256:0123"
	ReportVersionSupport(rc, supportNow)
	if c := cond(); c.Status != metav1.ConditionUnknown || c.Reason != "Unknown" || !strings.Contains(c.Message, "sha256:0123") {
		t.Fatalf("digest: %+v", c)
	}
	if evs := recorded(rc); len(evs) != 0 {
		t.Fatalf("Unknown emitted %q", evs)
	}
	f.Status.Version = "15.0.10+gitea-1.22.0"
	ReportVersionSupport(rc, supportNow)
	if c := cond(); c.Reason != "Supported" {
		t.Fatalf("digest with reported version: %+v", c)
	}

	// Before the first rollout the image being deployed is classified.
	g := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "ns"}, Spec: forgejov1.ForgejoSpec{Image: img18}}
	grc := supportContext(g)
	grc.TargetImage = img18
	ReportVersionSupport(grc, supportNow)
	if c := meta.FindStatusCondition(g.Status.Conditions, forgejov1.ConditionVersionSupported); c == nil || c.Reason != "Future" {
		t.Fatalf("fresh 18: %+v", c)
	}
	if evs := withPrefix(recorded(grc), "Warning UnsupportedVersion"); len(evs) != 1 {
		t.Fatalf("fresh 18: %q", evs)
	}
	forgejo.DeleteInstanceMetrics("ns", "vs")
	forgejo.DeleteInstanceMetrics("ns", "new")
	ForgetInstance("ns", "vs")
	ForgetInstance("ns", "new")
}

// forgejo_operator_version_advisory is 1 while the running patch release has an advisory, 0
// for a later patch, absent while the version is unknown, and deleted with the instance.
func TestVersionAdvisoryGauge(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "adv", Namespace: "ns", UID: "uid-adv"},
		Spec: forgejov1.ForgejoSpec{Image: img17}, Status: forgejov1.ForgejoStatus{Image: img17, Version: testVersion170}}
	rc := supportContext(f)
	t.Cleanup(func() { ForgetInstance("ns", "adv") })
	advisory := func() float64 { return metricValue(t, forgejo.VersionAdvisory.WithLabelValues("ns", "adv", "17")) }
	ReportVersionSupport(rc, supportNow)
	if advisory() != 1 || seriesCount(forgejo.VersionAdvisory) != 1 {
		t.Fatal("17.0.0: want 1")
	}
	f.Status.Version = testVersion171
	ReportVersionSupport(rc, supportNow)
	if advisory() != 0 || seriesCount(forgejo.VersionAdvisory) != 1 {
		t.Fatal("17.0.1: want 0")
	}
	f.Status.Image, f.Status.Version = "codeberg.org/forgejo/forgejo@sha256:0123", ""
	ReportVersionSupport(rc, supportNow)
	if seriesCount(forgejo.VersionAdvisory) != 0 {
		t.Fatal("unknown version: want no series")
	}
	f.Status.Image, f.Status.Version = img15, "15.0.7+gitea-1.22.0"
	ExportVersionSupport(f, supportNow)
	if metricValue(t, forgejo.VersionAdvisory.WithLabelValues("ns", "adv", "15")) != 1 {
		t.Fatal("15.0.7 (paused instance): want 1")
	}
	forgejo.DeleteInstanceMetrics("ns", "adv")
	if seriesCount(forgejo.VersionAdvisory) != 0 {
		t.Fatal("not deleted with the instance")
	}
}

// An HA instance on a floating tag whose nodes hold different patch releases: the probe
// through the Service makes status.version alternate, and with it the advisory text. The
// condition follows every probe, but VersionAdvisory is emitted once per image and patch.
func TestVersionAdvisoryIsNotRepeatedByAlternatingPatches(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "flip", Namespace: "ns", UID: "uid-flip"},
		Spec: forgejov1.ForgejoSpec{Image: img17}, Status: forgejov1.ForgejoStatus{Image: img17}}
	rc := supportContext(f)
	t.Cleanup(func() {
		forgejo.DeleteInstanceMetrics("ns", "flip")
		ForgetInstance("ns", "flip")
	})
	var advisories int
	for i := range 6 {
		f.Status.Version = []string{testVersion170, testVersion171}[i%2]
		ReportVersionSupport(rc, supportNow.Add(time.Duration(i)*time.Minute))
		advisories += len(withPrefix(recorded(rc), "Warning VersionAdvisory"))
		if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionVersionSupported); (i%2 == 0) != strings.Contains(c.Message, "17.0.1 or newer") {
			t.Fatalf("probe %d: the condition does not follow %s: %s", i, f.Status.Version, c.Message)
		}
	}
	if advisories != 1 {
		t.Fatalf("want one VersionAdvisory, got %d", advisories)
	}
	// A day later the advisory is repeated once, and a new image is reported at once.
	f.Status.Version = testVersion171
	ReportVersionSupport(rc, supportNow.Add(25*time.Hour))
	f.Status.Version = testVersion170
	ReportVersionSupport(rc, supportNow.Add(25*time.Hour))
	if evs := withPrefix(recorded(rc), "Warning VersionAdvisory"); len(evs) != 1 {
		t.Fatalf("after a day: %q", evs)
	}
	f.Status.Image = "codeberg.org/forgejo/forgejo:17-rootless"
	f.Status.Version = testVersion171
	ReportVersionSupport(rc, supportNow.Add(26*time.Hour))
	f.Status.Version = testVersion170
	ReportVersionSupport(rc, supportNow.Add(26*time.Hour))
	if evs := withPrefix(recorded(rc), "Warning VersionAdvisory"); len(evs) != 1 {
		t.Fatalf("new image: %q", evs)
	}
	// Forgetting the instance (deleted) reports it again.
	ForgetInstance("ns", "flip")
	if !adviseOnce(f, f.Status.Image, forgejo.Version{Major: 17, Minor: 0, Patch: 0}, supportNow.Add(26*time.Hour)) {
		t.Fatal("ForgetInstance kept the advisory")
	}
}

// The upgrade gate: which image changes start an orchestrated upgrade, with which events, and
// which are refused while the old image keeps running. An image without a version in its
// reference (a digest, or a custom tag) is gated by the version Forgejo reports (status.version).
func TestUpgradeGate(t *testing.T) {
	const (
		img14  = "codeberg.org/forgejo/forgejo:14"
		img20  = "codeberg.org/forgejo/forgejo:20"
		digest = "codeberg.org/forgejo/forgejo@sha256:0123"
		custom = "registry.example/forgejo:prod"
	)
	// badDuration is a value 17 refuses to start with.
	badDuration := func(f *forgejov1.Forgejo) {
		f.Spec.Config = map[string]map[string]string{testSectionServer: {testKeyPerWriteTimeout: "-1"}}
	}
	reportedOnly := "Warning UnverifiedVersion The version of " + digest + " is only known from what Forgejo reports (17.0.0)"
	for _, tt := range []struct {
		name, from, version, to string
		mutate                  func(*forgejov1.Forgejo)
		// refused is the reason (Upgrading and Degraded) of a refusal and msg a part of its
		// message; "" for an upgrade that starts.
		refused, msg string
		// events are prefixes of events emitted once each, none of events never emitted.
		events, none []string
	}{
		// 15 (LTS) -> 17 skips 16: supported upstream, so it is one orchestrated upgrade.
		{name: "15 to 17", from: img15, to: img17, none: []string{"Warning"},
			events: []string{"Normal MajorUpgrade Upgrading across major versions from 15 to 17, skipping 16;"}},
		// Skipping the LTS 15 is refused like a downgrade.
		{name: "11 to 17", from: img11, to: img17, refused: ReasonUpgradePathUnsupported, msg: "Upgrade to Forgejo 15 first",
			events: []string{"Warning UpgradePathUnsupported"}},
		// Choosing the intermediate LTS starts the upgrade and clears Degraded.
		{name: "11 to 15 after the refusal of 17", from: img11, to: img15, mutate: func(f *forgejov1.Forgejo) {
			meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionDegraded, Status: metav1.ConditionTrue,
				Reason: ReasonUpgradePathUnsupported})
		}},
		{name: "downgrade", from: img17, to: img15, refused: ReasonDowngradeRefused, events: []string{"Warning DowngradeRefused"}},
		// A major newer than the table is allowed, with a warning.
		{name: "17 to 18", from: img17, to: img18, events: []string{"Warning UnsupportedVersion Upgrading to " + img18 + " anyway"}},
		// Beyond every row of the table the newest row's UpgradeFrom holds.
		{name: "14 to 20", from: img14, to: img20, refused: ReasonUpgradePathUnsupported, msg: "Upgrade to Forgejo 15 first",
			events: []string{"Warning UpgradePathUnsupported"}},
		{name: "15 to 20", from: img15, to: img20, events: []string{"Warning UnsupportedVersion Upgrading to " + img20 + " anyway"}},
		// Warned, not blocked.
		{name: "16 to 17 with a duration 17 refuses", from: img16, to: img17, mutate: badDuration,
			events: []string{"Warning ConfigIncompatible " + img17 + " refuses to start with [server] PER_WRITE_TIMEOUT"}},

		{name: "digest of 14 to 17", from: digest, version: "14.0.5+gitea-1.22.0", to: img17, refused: ReasonUpgradePathUnsupported,
			events: []string{"Warning UpgradePathUnsupported Refusing to upgrade from " + digest + " (14.0.5)"}},
		{name: "custom tag of 14 to 17", from: custom, version: "14.0.5+gitea-1.22.0", to: img17, refused: ReasonUpgradePathUnsupported},
		{name: "digest of 17 to 15", from: digest, version: testVersion170, to: img15, refused: ReasonDowngradeRefused},
		// The duration check knows 16 is lenient.
		{name: "digest of 16 to 17", from: digest, version: "16.0.6+gitea-1.22.0", to: img17, mutate: badDuration,
			events: []string{"Normal MajorUpgrade Upgrading across major versions from 16 to 17;", "Warning ConfigIncompatible"},
			none:   []string{"Warning UnverifiedVersion"}},
		// The reported version can lag behind an image change, so it never turns a change into a
		// plain rollout: the same version, or an allowed downgrade, is orchestrated as before.
		{name: "digest of the same version", from: digest, version: testVersion170, to: img17, events: []string{reportedOnly}},
		{name: "digest of 17 to 15, allowed", from: digest, version: testVersion170, to: img15,
			mutate: func(f *forgejov1.Forgejo) { f.Spec.Upgrade.AllowDowngrade = true }, events: []string{reportedOnly}},
		// Without a reported version nothing can be compared: orchestrated, as before.
		{name: "digest without status.version", from: digest, to: img15, events: []string{"Warning UnverifiedVersion Cannot compare"}},
		// An unknown running version is not assumed to be strict about durations (it may be 15 or
		// 16), so the upgrade to 17 warns about the values 17 refuses.
		{name: "digest without status.version to 17", from: digest, to: img17, mutate: badDuration,
			events: []string{"Warning ConfigIncompatible"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := fixture(nil)
			f.Spec.Image, f.Status.Image, f.Status.Version = tt.to, tt.from, tt.version
			if tt.mutate != nil {
				tt.mutate(f)
			}
			c := newClient(t)
			rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Recorder: events.NewFakeRecorder(100),
				Instance: f, TargetImage: tt.from}
			if _, err := Reconcile(context.Background(), rc); err != nil {
				t.Fatal(err)
			}
			evs := recorded(rc)
			up, deg := condition(f, forgejov1.ConditionUpgrading), condition(f, forgejov1.ConditionDegraded)
			if rc.TargetImage != tt.from {
				t.Errorf("target %s", rc.TargetImage)
			}
			if tt.refused != "" {
				if f.Status.Upgrade != nil || up == nil || up.Reason != tt.refused || !strings.Contains(up.Message, tt.msg) ||
					deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != tt.refused {
					t.Errorf("upgrade %+v, Upgrading %+v, Degraded %+v", f.Status.Upgrade, up, deg)
				}
			} else if u := f.Status.Upgrade; u == nil || u.Phase != forgejov1.UpgradePhasePending ||
				meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionDegraded) {
				t.Errorf("upgrade %+v, Degraded %+v", u, deg)
			}
			for _, e := range tt.events {
				if n := len(withPrefix(evs, e)); n != 1 {
					t.Errorf("%d events %q: %q", n, e, evs)
				}
			}
			for _, e := range tt.none {
				if n := len(withPrefix(evs, e)); n != 0 {
					t.Errorf("events %q: %q", e, evs)
				}
			}
		})
	}
}
