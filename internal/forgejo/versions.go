package forgejo

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Versions is the operator's Forgejo version table, the single source of truth for which
// Forgejo releases it knows, supports and how it runs each of them. Edit it (and only it)
// when Forgejo publishes or retires a release: docs/versions.md, "When a new Forgejo version
// is released", lists the steps, and `make forgejo-versions` reports what changed upstream.
//
// Dates come from https://codeberg.org/forgejo/docs/raw/branch/next/release-schedule.json.
var Versions = VersionTable{
	Releases: []Release{
		{
			Major: 15, LTS: true, Released: date(2026, time.April, 16), EOL: date(2027, time.July, 15),
			UpgradeFrom: 11,
			Advisory: &Advisory{FixedIn: 10, Note: "15.0.8 and 15.0.9 fix critical security issues and 15.0.10 " +
				"authorization issues"},
			Traits: Traits{InternalToken: true},
		},
		{
			Major: 16, Released: date(2026, time.July, 16), EOL: date(2026, time.October, 29),
			UpgradeFrom: 15,
			Advisory: &Advisory{FixedIn: 6, Note: "16.0.4 and 16.0.5 fix critical security issues and 16.0.6 " +
				"authorization issues"},
			Traits: Traits{InternalToken: true},
		},
		{
			Major: 17, Released: date(2026, time.October, 15), EOL: date(2027, time.January, 28),
			UpgradeFrom: 15,
			Advisory: &Advisory{FixedIn: 1, Note: "upstream advises against 17.0.0, which can fail pushes, merges " +
				"and other git operations in some repositories (forgejo#14866)"},
			Traits: Traits{InternalToken: true, StrictDurations: true},
		},
	},
	Upcoming: []Release{
		{
			// Prepared from the merged 18 changes (forgejo#14148, #14791, #14164) and smoke-tested
			// with forgejo-experimental 18.0-test (18.0.0-dev-128, 2026-10-22): migrate, admin
			// create, change-password from stdin, hooks of an HTTP push over the socket. Moving
			// this row into Releases activates them for Forgejo 18 images.
			Major: 18, Released: date(2027, time.January, 14), EOL: date(2027, time.April, 29),
			UpgradeFrom: 15,
			Traits:      Traits{InternalSocket: true, StrictDurations: true, PasswordFromStdin: true},
		},
		{
			// Assumed to keep 18's traits; verify before moving it into Releases.
			Major: 19, LTS: true, Released: date(2027, time.April, 15), EOL: date(2028, time.July, 13),
			UpgradeFrom: 15,
			Traits:      Traits{InternalSocket: true, StrictDurations: true, PasswordFromStdin: true},
		},
	},
}

// VersionTable lists Forgejo major releases. Releases have been verified with the operator;
// Upcoming ones are known from the upstream schedule but not verified yet: images of an
// upcoming major are reported as Future and run with the traits of the newest verified
// release combined with the prepared app.ini traits of the upcoming rows up to that major
// (Traits).
type VersionTable struct {
	// Releases, oldest first, one row per major without gaps. Keep every row after its end
	// of life: majors older than the oldest row are reported as end of life and run with
	// LegacyTraits, which only fit majors before 18.
	Releases []Release
	// Upcoming, oldest first, all newer than every row of Releases.
	Upcoming []Release
}

// Release is one Forgejo major release line (N.0.x).
type Release struct {
	Major int
	// LTS releases are maintained upstream for about 15 months; the others until shortly
	// after the next release.
	LTS bool
	// Released and EOL are upstream's release and end-of-life dates (UTC days).
	Released, EOL time.Time
	// UpgradeFrom is the oldest major the operator upgrades to this one directly. Instances
	// on an older major must upgrade to UpgradeFrom first. 0 allows any.
	UpgradeFrom int
	// Advisory recommends a minimum patch release of this major.
	Advisory *Advisory
	// Traits are how the operator runs this major.
	Traits Traits
}

// Advisory recommends upgrading past patch releases with known problems.
type Advisory struct {
	// FixedIn is the first good patch number: N.0.<FixedIn>.
	FixedIn int
	// Note says what is wrong with the older patches.
	Note string
}

// Traits are the per-major differences in how the operator renders app.ini and runs the
// Forgejo CLI.
type Traits struct {
	// InternalToken renders [security] INTERNAL_TOKEN. Forgejo 17 and older generate a
	// missing token and write it into app.ini at start, which fails on the read-only mount.
	// Forgejo 18 removed it (forgejo#14148) and only logs an error when it is present.
	InternalToken bool
	// InternalSocket renders [server] INTERNAL_LISTENER_PATH on the per-pod /tmp volume.
	// Since Forgejo 18 git hooks, the SSH server and `forgejo manager` reach Forgejo through
	// this UNIX socket; its default (run/forgejo.sock under WORK_PATH) is on the shared data
	// volume, where the pods of a multi-replica instance would replace each other's socket.
	// Older majors ignore the key.
	InternalSocket bool
	// StrictDurations: a duration setting that is set but does not parse stops Forgejo at
	// start (Forgejo 17, forgejo#10273). The operator warns about such values.
	StrictDurations bool
	// PasswordFromStdin: `admin user change-password` reads the password from stdin
	// (Forgejo 18, forgejo#14164), so the bootstrap keeps it out of the process list.
	PasswordFromStdin bool
}

// ConfigProfile names the traits that shape the rendered app.ini. An app.ini rendered for one
// profile must not be mounted for an image that needs another (see the last known good
// config of autopilot).
func (t Traits) ConfigProfile() string {
	var parts []string
	if t.InternalToken {
		parts = append(parts, "internal-token")
	}
	if t.InternalSocket {
		parts = append(parts, "internal-socket")
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, "+")
}

// LegacyConfigProfile is the profile of app.ini Secrets written before profiles were
// recorded: every one of them carried INTERNAL_TOKEN.
const LegacyConfigProfile = "internal-token"

// LegacyTraits are how the operator runs majors older than the version table. Every Forgejo
// before 18 generates a missing INTERNAL_TOKEN at start and writes it into app.ini, which
// fails on the read-only mount, so app.ini must carry the token. They are fixed rather than
// taken from the oldest row, so that an instance left on an old major keeps its app.ini
// (and the profile of its last known good config) whatever the table's oldest row is.
// TestVersionTableIsWellFormed checks that the oldest row has the same profile, so no major
// falls between LegacyTraits and the table.
var LegacyTraits = Traits{InternalToken: true}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Day formats a table date.
func Day(t time.Time) string { return t.Format(time.DateOnly) }

// out reports whether the release has been published at now.
func (r *Release) out(now time.Time) bool { return !now.Before(r.Released) }

// ended reports whether the release reached its end of life at now.
func (r *Release) ended(now time.Time) bool { return !now.Before(r.EOL) }

// Name is "15 (LTS)" or "17".
func (r *Release) Name() string {
	if r.LTS {
		return fmt.Sprintf("%d (LTS)", r.Major)
	}
	return fmt.Sprint(r.Major)
}

// Release returns the verified release of a major, or nil.
func (t VersionTable) Release(major int) *Release {
	for i := range t.Releases {
		if t.Releases[i].Major == major {
			return &t.Releases[i]
		}
	}
	return nil
}

// WithVerified returns a copy of the table in which the upcoming releases up to major have
// been moved into Releases, as the release workflow does once a major has been verified.
// Tests use it to exercise prepared traits.
func (t VersionTable) WithVerified(major int) VersionTable {
	out := VersionTable{Releases: slices.Clone(t.Releases)}
	for _, u := range t.Upcoming {
		if u.Major <= major {
			out.Releases = append(out.Releases, u)
		} else {
			out.Upcoming = append(out.Upcoming, u)
		}
	}
	return out
}

// upcoming returns the upcoming release of a major, or nil.
func (t VersionTable) upcoming(major int) *Release {
	for i := range t.Upcoming {
		if t.Upcoming[i].Major == major {
			return &t.Upcoming[i]
		}
	}
	return nil
}

// Newest is the newest verified release.
func (t VersionTable) Newest() *Release { return &t.Releases[len(t.Releases)-1] }

// Oldest is the oldest release still in the table.
func (t VersionTable) Oldest() *Release { return &t.Releases[0] }

// Supported applies the support policy at now: the newest LTS release and the newest
// non-LTS release that have been published and have not reached their end of life. Both
// are derived from the table, so the policy follows the data: when a new release is added
// (or its date arrives), the one it replaces becomes Deprecated until its end of life.
func (t VersionTable) Supported(now time.Time) []Release {
	var lts, latest *Release
	for i := range t.Releases {
		r := &t.Releases[i]
		if !r.out(now) || r.ended(now) {
			continue
		}
		if r.LTS {
			if lts == nil || r.Major > lts.Major {
				lts = r
			}
		} else if latest == nil || r.Major > latest.Major {
			latest = r
		}
	}
	var out []Release
	for _, r := range []*Release{lts, latest} {
		if r != nil {
			out = append(out, *r)
		}
	}
	slices.SortFunc(out, func(a, b Release) int { return a.Major - b.Major })
	return out
}

// CurrentLTS is the supported LTS release at now, or nil.
func (t VersionTable) CurrentLTS(now time.Time) *Release {
	for _, r := range t.Supported(now) {
		if r.LTS {
			return &r
		}
	}
	return nil
}

// SupportLevel classifies a Forgejo version against the support policy.
type SupportLevel string

const (
	// SupportSupported: the current LTS or the latest release, verified with the operator.
	SupportSupported SupportLevel = "Supported"
	// SupportDeprecated: still maintained upstream, but outside the operator's policy.
	SupportDeprecated SupportLevel = "Deprecated"
	// SupportEndOfLife: no longer maintained upstream.
	SupportEndOfLife SupportLevel = "EndOfLife"
	// SupportFuture: newer than the verified releases (or not published yet).
	SupportFuture SupportLevel = "Future"
	// SupportUnknown: the version cannot be determined (digest-only or non-version tag).
	SupportUnknown SupportLevel = "Unknown"
)

// Support is the classification of one version.
type Support struct {
	Level SupportLevel
	// Version that was classified; Major is -1 for SupportUnknown.
	Version Version
	// Release is the verified release of the major, nil when it is not in the table.
	Release *Release
	// Advisory is set when Version is a patch release older than the release's advisory.
	Advisory *Advisory
	// Message explains the level, what to upgrade to and by when.
	Message string
}

// Classify applies the support policy to a version at now. ok is false when the version is
// unknown.
func (t VersionTable) Classify(v Version, ok bool, now time.Time) Support {
	if !ok || v.Major < 0 {
		return Support{Level: SupportUnknown, Version: Version{Major: -1, Minor: -1, Patch: -1},
			Message: "The Forgejo version is unknown: the image tag carries no version and Forgejo has not reported one yet."}
	}
	s := Support{Version: v, Release: t.Release(v.Major)}
	// The message names the major only, so it stays the same when the exact version becomes
	// known (status.version); only an advisory names the patch release.
	name := fmt.Sprintf("Forgejo %d", v.Major)
	policy := t.policyText(now)
	r := s.Release
	switch {
	case r == nil && v.Major > t.Newest().Major:
		s.Level = SupportFuture
		s.Message = fmt.Sprintf("%s is newer than this operator's version table (newest verified: %d) and has not been "+
			"verified with it%s. It runs, but update the operator to a release that supports Forgejo %d.",
			name, t.Newest().Major, t.scheduleText(v.Major), v.Major)
	case r == nil && v.Major < t.Oldest().Major:
		s.Level = SupportEndOfLife
		s.Message = fmt.Sprintf("%s has reached its upstream end of life and receives no security fixes. %s",
			name, t.upgradeAdvice(v.Major, now, "now"))
	case r == nil:
		s.Level = SupportUnknown
		s.Message = fmt.Sprintf("%s is not in this operator's version table.", name)
	case !r.out(now):
		s.Level = SupportFuture
		s.Message = fmt.Sprintf("%s is not released yet (due %s) and is not supported by this operator before then.",
			name, Day(r.Released))
	case r.ended(now):
		s.Level = SupportEndOfLife
		s.Message = fmt.Sprintf("%s reached its upstream end of life on %s and receives no security fixes. %s",
			name, Day(r.EOL), t.upgradeAdvice(v.Major, now, "now"))
	case slices.ContainsFunc(t.Supported(now), func(x Release) bool { return x.Major == r.Major }):
		s.Level = SupportSupported
		kind := "the latest release"
		if r.LTS {
			kind = "the current LTS release"
		}
		s.Message = fmt.Sprintf("%s is supported: %s, upstream end of life %s.", name, kind, Day(r.EOL))
	default:
		s.Level = SupportDeprecated
		s.Message = fmt.Sprintf("%s is outside this operator's support policy (%s) and reaches its upstream end of life on %s. %s",
			name, policy, Day(r.EOL), t.upgradeAdvice(v.Major, now, "before "+Day(r.EOL)))
	}
	if r != nil && r.Advisory != nil && v.Minor == 0 && v.Patch >= 0 && v.Patch < r.Advisory.FixedIn {
		s.Advisory = r.Advisory
		s.Message += fmt.Sprintf(" The running patch release %s is older than recommended: use %d.0.%d or newer (%s).",
			v, v.Major, r.Advisory.FixedIn, r.Advisory.Note)
	}
	return s
}

// policyText lists the supported releases, e.g. "15 (LTS) and 17".
func (t VersionTable) policyText(now time.Time) string {
	supported := t.Supported(now)
	names := make([]string, 0, len(supported))
	for _, r := range supported {
		names = append(names, r.Name())
	}
	if len(names) == 0 {
		return "no supported release"
	}
	return "supported: " + strings.Join(names, " and ")
}

// scheduleText describes an upcoming major's release date, if known.
func (t VersionTable) scheduleText(major int) string {
	if u := t.upcoming(major); u != nil {
		return fmt.Sprintf(" (upstream release %s)", Day(u.Released))
	}
	return ""
}

// upgradeAdvice names the supported releases an instance on major can upgrade to, with the
// minimum patch release of a target that has an advisory ("17 (17.0.1 or newer)"): a floating
// tag may still pull the patch the advisory is about. The text comes from the table only, so
// it stays the same across the running patch releases.
func (t VersionTable) upgradeAdvice(major int, now time.Time, when string) string {
	var targets []string
	for _, r := range t.Supported(now) {
		if r.Major <= major {
			continue
		}
		var notes []string
		if r.LTS {
			notes = append(notes, "LTS")
		}
		if r.Advisory != nil {
			notes = append(notes, fmt.Sprintf("%d.0.%d or newer", r.Major, r.Advisory.FixedIn))
		}
		if ok, via := t.DirectUpgrade(major, r.Major); !ok {
			notes = append(notes, fmt.Sprintf("through %d first", via))
		}
		name := strconv.Itoa(r.Major)
		if len(notes) > 0 {
			name += " (" + strings.Join(notes, ", ") + ")"
		}
		targets = append(targets, name)
	}
	if len(targets) == 0 {
		return "No newer supported release is in this operator's version table: update the operator."
	}
	return fmt.Sprintf("Upgrade to %s %s.", strings.Join(targets, " or "), when)
}

// newestRow is the newest row of the table, upcoming or verified.
func (t VersionTable) newestRow() *Release {
	if len(t.Upcoming) > 0 {
		return &t.Upcoming[len(t.Upcoming)-1]
	}
	return t.Newest()
}

// DirectUpgrade reports whether the operator upgrades from major from to major to in one
// step. When it does not, via is the major to upgrade to first. The target's row (verified or
// upcoming) decides; a major newer than every row is held to the newest row's UpgradeFrom, so
// that an unknown future major is at least as strict as the newest known one. Majors older
// than the table are not restricted.
func (t VersionTable) DirectUpgrade(from, to int) (ok bool, via int) {
	r := t.Release(to)
	if r == nil {
		r = t.upcoming(to)
	}
	if newest := t.newestRow(); r == nil && to > newest.Major {
		r = newest
	}
	if r == nil || from >= to || r.UpgradeFrom == 0 || from >= r.UpgradeFrom {
		return true, 0
	}
	return false, r.UpgradeFrom
}

// Traits returns how the operator runs a version. Unknown versions get the traits that work
// on every major (CompatTraits), and majors older than the table LegacyTraits. Majors newer
// than the verified releases run with the newest verified release's traits plus the app.ini
// traits (InternalToken, InternalSocket) and StrictDurations of every upcoming row up to
// their major (all of them beyond the table): app.ini then carries what both need, as for
// CompatTraits, while CLI traits (PasswordFromStdin) stay those of the verified release.
func (t VersionTable) Traits(v Version, ok bool) Traits {
	switch {
	case !ok || v.Major < 0:
		return t.CompatTraits()
	case t.Release(v.Major) != nil:
		return t.Release(v.Major).Traits
	case v.Major > t.Newest().Major:
		c := t.Newest().Traits
		for _, u := range t.Upcoming {
			if u.Major <= v.Major {
				c.InternalToken = c.InternalToken || u.Traits.InternalToken
				c.InternalSocket = c.InternalSocket || u.Traits.InternalSocket
				c.StrictDurations = c.StrictDurations || u.Traits.StrictDurations
			}
		}
		return c
	default:
		return LegacyTraits
	}
}

// StrictDurationsSince is the oldest major (verified or upcoming) whose traits have
// StrictDurations, or 0 when none has.
func (t VersionTable) StrictDurationsSince() int {
	for _, r := range append(slices.Clone(t.Releases), t.Upcoming...) {
		if r.Traits.StrictDurations {
			return r.Major
		}
	}
	return 0
}

// CompatTraits are the traits for an image of unknown version, which may be any major,
// including one older or newer than the table: app.ini carries every key that LegacyTraits,
// some verified release or some upcoming one needs (each one is ignored or only logged by the
// others), and CLI features are used only when every verified release and LegacyTraits have
// them. StrictDurations is set when some verified release has it, so the duration warnings
// cover a digest-pinned 17; ReconcileConfig takes it from the reported version once known.
func (t VersionTable) CompatTraits() Traits {
	c := LegacyTraits
	for _, r := range t.Releases {
		c.InternalToken = c.InternalToken || r.Traits.InternalToken
		c.InternalSocket = c.InternalSocket || r.Traits.InternalSocket
		c.StrictDurations = c.StrictDurations || r.Traits.StrictDurations
		c.PasswordFromStdin = c.PasswordFromStdin && r.Traits.PasswordFromStdin
	}
	for _, u := range t.Upcoming {
		c.InternalToken = c.InternalToken || u.Traits.InternalToken
		c.InternalSocket = c.InternalSocket || u.Traits.InternalSocket
	}
	return c
}

// InstanceVersion is the version a Forgejo instance runs: from the tag of image, refined
// by reported (status.version, what Forgejo's API returned) when the tag floats ("17") or
// carries no version (a digest). A reported version of another major than the tag is stale
// (from before an image change) and ignored.
func InstanceVersion(image, reported string) (Version, bool) {
	v, ok := ImageVersion(image)
	if ok && v.Major < 0 {
		ok = false
	}
	rv, rok := ParseVersion(reported)
	if !rok || rv.Major < 0 {
		return v, ok
	}
	switch {
	case !ok:
		return rv, true
	case rv.Major == v.Major && rv.Compare(v) == 0 && (v.Minor < 0 || v.Patch < 0):
		return rv, true
	}
	return v, ok
}
