package forgejo

import (
	"slices"
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
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: supported %v, want %v", tc.now, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	const now = "2026-10-22"
	with18 := Versions.WithVerified(18)
	for _, tc := range []struct {
		table    VersionTable
		now      string
		version  string
		want     SupportLevel
		advisory bool
		msg      []string
	}{
		{Versions, now, "15", SupportSupported, false, []string{"current LTS", "2027-07-15"}},
		{Versions, now, "15.0.8", SupportSupported, true, []string{"The running patch release 15.0.8"}},
		{Versions, now, "15.0.10", SupportSupported, false, nil},
		{Versions, now, "17", SupportSupported, false, nil}, // floating: the patch is unknown
		{Versions, now, "17.0.0", SupportSupported, true, []string{"The running patch release 17.0.0"}},
		{Versions, now, "17.0.1", SupportSupported, false, []string{"latest release", "2027-01-28"}},
		// The advice names the minimum patch of a target with an advisory: a floating :17 may
		// still pull 17.0.0. An instance on 16 cannot go back to the LTS 15, so only 17 is
		// offered (the targets are listed oldest first).
		{Versions, now, "16.0.6", SupportDeprecated, false, []string{"2026-10-29", "Upgrade to 17 (17.0.1 or newer) before 2026-10-29"}},
		{Versions, "2026-10-29", "16", SupportEndOfLife, false, nil}, // its EOL day
		{Versions, now, "14", SupportEndOfLife, false, []string{"Upgrade to 15 (LTS, 15.0.10 or newer) or 17 (17.0.1 or newer, through 15 first) now"}},
		{Versions, now, "11.0.16", SupportEndOfLife, false, []string{"Upgrade to 15 (LTS, 15.0.10 or newer) or 17 (17.0.1 or newer, through 15 first) now"}},
		// 17 ended and nothing newer is verified: tell the user to update the operator.
		{Versions, "2027-02-01", "17", SupportEndOfLife, false, []string{"update the operator"}},
		{Versions, now, "18", SupportFuture, false, []string{"newest verified: 17", "2027-01-14"}},
		{Versions, now, "21", SupportFuture, false, []string{"update the operator"}},
		// Once 18 is verified and out, 17 is deprecated until its end of life.
		{with18, "2027-01-20", "17", SupportDeprecated, false, []string{"Upgrade to 18 before 2027-01-28"}},
		{with18, "2027-01-01", "18", SupportFuture, false, []string{"not released yet"}},
		{Versions, now, "latest", SupportUnknown, false, nil},
	} {
		ver, ok := ParseVersion(tc.version)
		s := tc.table.Classify(ver, ok, day(tc.now))
		if s.Level != tc.want || (s.Advisory != nil) != tc.advisory {
			t.Errorf("%s on %s: %s, advisory %v (%s); want %s, advisory %v",
				tc.version, tc.now, s.Level, s.Advisory != nil, s.Message, tc.want, tc.advisory)
		}
		for _, m := range tc.msg {
			if !strings.Contains(s.Message, m) {
				t.Errorf("%s on %s: message %q lacks %q", tc.version, tc.now, s.Message, m)
			}
		}
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
		// Majors beyond every row are held to the newest row's UpgradeFrom (19's: 15), so that
		// 14 -> 20 is refused like 14 -> 17.
		{14, 20, false, 15},
		{15, 20, true, 0},
		{17, 99, true, 0},
		{5, 7, true, 0},   // majors older than the table are not restricted
		{17, 15, true, 0}, // downgrades are the version gate's business
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
		// Not verified yet: the newest verified release's traits plus the prepared app.ini
		// traits of 18 (the per-pod socket), but not its CLI traits.
		{"codeberg.org/forgejo/forgejo:18", Traits{InternalToken: true, InternalSocket: true, StrictDurations: true}},
		{"codeberg.org/forgejo/forgejo:21-rootless", Traits{InternalToken: true, InternalSocket: true, StrictDurations: true}},
		{"codeberg.org/forgejo/forgejo:11", legacy},
		// Unknown version: what works on every verified and upcoming major.
		{digestImage, Traits{InternalToken: true, InternalSocket: true, StrictDurations: true}},
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
