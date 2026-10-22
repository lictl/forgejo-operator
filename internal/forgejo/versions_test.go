package forgejo

import (
	"strings"
	"testing"
	"time"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

const digestImage = "codeberg.org/forgejo/forgejo@sha256:0123"

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func majors(rs []Release) []int {
	out := make([]int, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Major)
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The table must stay well-formed when it is edited for a new release.
func TestVersionTableIsWellFormed(t *testing.T) {
	all := append(append([]Release{}, Versions.Releases...), Versions.Upcoming...)
	if len(Versions.Releases) == 0 {
		t.Fatal("no verified release")
	}
	for i, r := range all {
		if i > 0 && r.Major != all[i-1].Major+1 {
			t.Errorf("majors must be consecutive and ascending: %d after %d", r.Major, all[i-1].Major)
		}
		if !r.Released.Before(r.EOL) {
			t.Errorf("%d: released %s is not before EOL %s", r.Major, Day(r.Released), Day(r.EOL))
		}
		if i > 0 && !all[i-1].Released.Before(r.Released) {
			t.Errorf("%d is released before %d", r.Major, all[i-1].Major)
		}
		if r.UpgradeFrom >= r.Major {
			t.Errorf("%d: UpgradeFrom %d must be older", r.Major, r.UpgradeFrom)
		}
		if a := r.Advisory; a != nil && (a.FixedIn <= 0 || a.Note == "") {
			t.Errorf("%d: incomplete advisory %+v", r.Major, a)
		}
		// Every app.ini needs either the token (<= 17) or the socket (>= 18).
		if !r.Traits.InternalToken && !r.Traits.InternalSocket {
			t.Errorf("%d: neither InternalToken nor InternalSocket", r.Major)
		}
	}
	// Majors older than the table run with LegacyTraits, which only fit Forgejo 17 and older:
	// the oldest row must still render the legacy app.ini (rows are never dropped).
	if p := LegacyTraits.ConfigProfile(); p != LegacyConfigProfile {
		t.Errorf("LegacyTraits have profile %q, want %q", p, LegacyConfigProfile)
	}
	if o := Versions.Oldest(); o.Traits.ConfigProfile() != LegacyConfigProfile || o.Traits.PasswordFromStdin {
		t.Errorf("the oldest row %d must run like the majors before it (LegacyTraits): %+v", o.Major, o.Traits)
	}
}

// The default image must be the current LTS release, so that new instances start on it.
func TestDefaultImageIsCurrentLTS(t *testing.T) {
	v, ok := ImageVersion(forgejov1.DefaultImage)
	if !ok {
		t.Fatalf("default image %s has no version", forgejov1.DefaultImage)
	}
	// Checked at the newest release date of the table: when a new LTS is added, the default
	// moves with it. Today it is 15.
	lts := Versions.CurrentLTS(Versions.Newest().Released)
	if lts == nil || lts.Major != v.Major {
		t.Fatalf("default image %s is not the current LTS %v", forgejov1.DefaultImage, lts)
	}
}

func TestSupportPolicyFollowsTheCalendar(t *testing.T) {
	full := Versions.WithVerified(19)
	for _, tc := range []struct {
		table VersionTable
		now   string
		want  []int
	}{
		{Versions, "2026-10-22", []int{15, 17}},
		{Versions, "2026-10-14", []int{15, 16}},                  // before 17 was out
		{Versions, "2027-01-29", []int{15}},                      // 17 ended, 18 not verified
		{full, "2027-01-14", []int{15, 18}},                      // 18 out: 17 is deprecated
		{full, "2027-04-15", []int{18, 19}},                      // 19 LTS out: 15 is deprecated until July
		{full, "2027-04-29", []int{19}},                          // 18 ended; next non-LTS not in the table
		{Versions.WithVerified(18), "2026-12-01", []int{15, 17}}, // 18 verified early, not released yet
	} {
		got := majors(tc.table.Supported(day(tc.now)))
		if !sameInts(got, tc.want) {
			t.Errorf("%s: supported %v, want %v", tc.now, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	now := day("2026-10-22")
	v := func(s string) (Version, bool) { return ParseVersion(s) }
	for _, tc := range []struct {
		version string
		want    SupportLevel
		msg     []string
	}{
		{"15", SupportSupported, []string{"current LTS", "2027-07-15"}},
		{"15.0.10", SupportSupported, nil},
		{"17.0.1", SupportSupported, []string{"latest release", "2027-01-28"}},
		{"16.0.6", SupportDeprecated, []string{"2026-10-29", "Upgrade to 17 before 2026-10-29"}},
		{"14", SupportEndOfLife, []string{"Upgrade to 15 (LTS) or 17 (through 15 first) now"}},
		{"11.0.16", SupportEndOfLife, []string{"Upgrade to 15 (LTS) or 17 (through 15 first) now"}},
		{"18", SupportFuture, []string{"newest verified: 17", "2027-01-14"}},
		{"21", SupportFuture, []string{"update the operator"}},
		{"latest", SupportUnknown, nil},
	} {
		ver, ok := v(tc.version)
		s := Versions.Classify(ver, ok, now)
		if s.Level != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.version, s.Level, s.Message, tc.want)
		}
		for _, m := range tc.msg {
			if !strings.Contains(s.Message, m) {
				t.Errorf("%s: message %q lacks %q", tc.version, s.Message, m)
			}
		}
	}
	// An instance on 16 cannot go back to the LTS 15, so only 17 is offered.
	if s := Versions.Classify(Version{Major: 16, Minor: -1, Patch: -1}, true, now); strings.Contains(s.Message, "Upgrade to 15") {
		t.Errorf("16: offered a downgrade to 15: %s", s.Message)
	}

	// 16 reaches its end of life on 2026-10-29.
	if s := Versions.Classify(Version{Major: 16, Minor: -1, Patch: -1}, true, day("2026-10-29")); s.Level != SupportEndOfLife {
		t.Errorf("16 on its EOL day: %s", s.Level)
	}
	// Once 18 is verified and out, 17 is deprecated until its end of life.
	full := Versions.WithVerified(18)
	if s := full.Classify(Version{Major: 17, Minor: 0, Patch: 1}, true, day("2027-01-20")); s.Level != SupportDeprecated ||
		!strings.Contains(s.Message, "Upgrade to 18 before 2027-01-28") {
		t.Errorf("17 after 18: %s %s", s.Level, s.Message)
	}
	if s := full.Classify(Version{Major: 18, Minor: -1, Patch: -1}, true, day("2027-01-01")); s.Level != SupportFuture ||
		!strings.Contains(s.Message, "not released yet") {
		t.Errorf("verified 18 before its release: %s %s", s.Level, s.Message)
	}
	// 17 ended and nothing newer is verified: tell the user to update the operator.
	if s := Versions.Classify(Version{Major: 17, Minor: -1, Patch: -1}, true, day("2027-02-01")); s.Level != SupportEndOfLife ||
		!strings.Contains(s.Message, "update the operator") {
		t.Errorf("17 after EOL: %s %s", s.Level, s.Message)
	}
}

// The message names the major only, so that it does not change (and is not reported again)
// when the exact version becomes known.
func TestClassifyMessageIsStableAcrossPatches(t *testing.T) {
	now := day("2026-10-22")
	floating := Versions.Classify(Version{16, -1, -1}, true, now)
	exact := Versions.Classify(Version{16, 0, 6}, true, now)
	if floating.Message != exact.Message || !strings.HasPrefix(exact.Message, "Forgejo 16 is outside") {
		t.Fatalf("%q != %q", floating.Message, exact.Message)
	}
}

func TestClassifyAdvisories(t *testing.T) {
	now := day("2026-10-22")
	for _, tc := range []struct {
		v        Version
		advisory bool
	}{
		{Version{17, 0, 0}, true},
		{Version{17, 0, 1}, false},
		{Version{17, -1, -1}, false}, // floating: the patch is unknown
		{Version{15, 0, 8}, true},
		{Version{15, 0, 10}, false},
	} {
		s := Versions.Classify(tc.v, true, now)
		if (s.Advisory != nil) != tc.advisory {
			t.Errorf("%s: advisory %v, want %v (%s)", tc.v, s.Advisory != nil, tc.advisory, s.Message)
		}
		if tc.advisory && (s.Level != SupportSupported || !strings.Contains(s.Message, "The running patch release "+tc.v.String())) {
			t.Errorf("%s: %s %s", tc.v, s.Level, s.Message)
		}
	}
}

func TestDirectUpgrade(t *testing.T) {
	for _, tc := range []struct {
		from, to int
		ok       bool
		via      int
	}{
		{15, 17, true, 0}, // skipping 16 is supported upstream and tested
		{16, 17, true, 0},
		{15, 16, true, 0},
		{14, 17, false, 15},
		{11, 15, true, 0},
		{7, 15, false, 11},
		{11, 18, false, 15}, // upcoming rows count too
		{17, 99, true, 0},   // unknown targets are not restricted
		{17, 15, true, 0},   // downgrades are the version gate's business
	} {
		ok, via := Versions.DirectUpgrade(tc.from, tc.to)
		if ok != tc.ok || via != tc.via {
			t.Errorf("%d -> %d: %v via %d, want %v via %d", tc.from, tc.to, ok, via, tc.ok, tc.via)
		}
	}
}

func TestTraits(t *testing.T) {
	legacy := Traits{InternalToken: true}
	for _, tc := range []struct {
		image string
		want  Traits
	}{
		{"codeberg.org/forgejo/forgejo:15", legacy},
		{"codeberg.org/forgejo/forgejo:16-rootless", legacy},
		{"codeberg.org/forgejo/forgejo:17.0.0", Traits{InternalToken: true, StrictDurations: true}},
		// Not verified yet: runs like the newest verified release.
		{"codeberg.org/forgejo/forgejo:18", Traits{InternalToken: true, StrictDurations: true}},
		{"codeberg.org/forgejo/forgejo:11", legacy},
		// Unknown version: what works on every verified release.
		{digestImage, Traits{InternalToken: true, StrictDurations: true}},
	} {
		if got := LayoutForImage(tc.image).Traits; got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.image, got, tc.want)
		}
	}

	// Verifying 18 activates its prepared traits, and unknown versions then get both the
	// token and the socket, which work on 17 and older as well as on 18.
	full := Versions.WithVerified(18)
	v18 := full.Traits(Version{18, -1, -1}, true)
	if v18.InternalToken || !v18.InternalSocket || !v18.PasswordFromStdin || !v18.StrictDurations {
		t.Errorf("18: %+v", v18)
	}
	if c := full.CompatTraits(); !c.InternalToken || !c.InternalSocket || c.PasswordFromStdin {
		t.Errorf("compat with 18: %+v", c)
	}
	if got := full.Traits(Version{17, 0, 0}, true); !got.InternalToken || got.InternalSocket {
		t.Errorf("17 with 18 verified: %+v", got)
	}
	if p := v18.ConfigProfile(); p != "internal-socket" {
		t.Errorf("18 profile %q", p)
	}
	if p := legacy.ConfigProfile(); p != LegacyConfigProfile {
		t.Errorf("legacy profile %q", p)
	}

	// Majors older than the table keep the legacy app.ini whatever its oldest row is: even a
	// table that started at 18 would still render INTERNAL_TOKEN for 17, by version and for
	// images of unknown version.
	from18 := VersionTable{Releases: full.Releases[len(full.Releases)-1:]}
	if from18.Oldest().Major != 18 {
		t.Fatalf("oldest %d", from18.Oldest().Major)
	}
	if got := from18.Traits(Version{17, 0, 3}, true); got != LegacyTraits {
		t.Errorf("17 below a table from 18: %+v", got)
	}
	if c := from18.CompatTraits(); !c.InternalToken || !c.InternalSocket || c.PasswordFromStdin {
		t.Errorf("compat of a table from 18: %+v", c)
	}
}

func TestInstanceVersion(t *testing.T) {
	for _, tc := range []struct {
		image, reported, want string
		ok                    bool
	}{
		{"codeberg.org/forgejo/forgejo:17", "17.0.0+gitea-1.22.0", "17.0.0", true},
		{"codeberg.org/forgejo/forgejo:17", "16.0.6+gitea-1.22.0", "17", true}, // stale report
		{"codeberg.org/forgejo/forgejo:17.0.1", "17.0.0+gitea-1.22.0", "17.0.1", true},
		{digestImage, "15.0.10+gitea-1.22.0", "15.0.10", true},
		{digestImage, "", "", false},
		{"example.com/forgejo:latest", "", "", false},
	} {
		v, ok := InstanceVersion(tc.image, tc.reported)
		if ok != tc.ok || (ok && v.String() != tc.want) {
			t.Errorf("%s + %q: %s %v, want %s %v", tc.image, tc.reported, v, ok, tc.want, tc.ok)
		}
	}
}

func TestInvalidDurations(t *testing.T) {
	cfg := map[string]map[string]string{
		"server":      {"PER_WRITE_TIMEOUT": "-1", "PER_WRITE_PER_KB_TIMEOUT": "10s", "STATIC_CACHE_TIME": "7d", "LFS_HTTP_AUTH_EXPIRY": " "},
		"Database":    {"CONN_MAX_LIFETIME": "5m", "CONN_MAX_IDLETIME": "30", "DB_RETRY_BACKOFF": "0"},
		"indexer":     {keyStartupTimeout: "1h30m"},
		"git.timeout": {"DEFAULT": "360"}, // seconds, not a duration
		"actions":     {"ZOMBIE_TASK_TIMEOUT": "-1s", "ENDLESS_TASK_TIMEOUT": "3 hours"},
	}
	got := strings.Join(InvalidDurations(cfg), ", ")
	want := "[actions] ENDLESS_TASK_TIMEOUT, [database] CONN_MAX_IDLETIME, [server] PER_WRITE_TIMEOUT"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
