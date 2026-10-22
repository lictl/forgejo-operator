// Command forgejo-versions compares the operator's Forgejo version table
// (internal/forgejo/versions.go) with what Forgejo has published, and exits with 1 when the
// table is stale. It reads only public endpoints and needs no credentials:
//
//   - the release schedule (LTS flags, release and end-of-life dates):
//     https://codeberg.org/forgejo/docs/raw/branch/next/release-schedule.json
//   - the releases API (published patch releases and their notes):
//     https://codeberg.org/api/v1/repos/forgejo/forgejo/releases
//   - the container registry (whether the images of the latest patches exist), with an
//     anonymous pull token.
//
// Usage:
//
//	go run ./hack/forgejo-versions [-now YYYY-MM-DD] [-registry=false]
//
// Exit status: 0 the table is current, 1 it is stale (see the STALE lines), 2 an upstream
// source could not be read. docs/versions.md describes what to do for each finding.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	scheduleURL = "https://codeberg.org/forgejo/docs/raw/branch/next/release-schedule.json"
	releasesURL = "https://codeberg.org/api/v1/repos/forgejo/forgejo/releases"
	registry    = "https://codeberg.org"
	repository  = "forgejo/forgejo"
)

var client = &http.Client{Timeout: 30 * time.Second}

type scheduleEntry struct {
	Major   string `json:"major"`
	Minor   string `json:"minor"`
	Cut     string `json:"cut"`
	Release string `json:"release"`
	EOL     string `json:"eol"`
	LTS     bool   `json:"lts"`
}

type release struct {
	TagName    string `json:"tag_name"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// patch is the newest published patch release of a major.
type patch struct {
	version forgejo.Version
	body    string
}

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

func main() {
	nowFlag := flag.String("now", "", "evaluate as of this day (YYYY-MM-DD) instead of today")
	checkRegistry := flag.Bool("registry", true, "check that the images of the latest patch releases exist")
	flag.Parse()

	now := time.Now().UTC()
	if *nowFlag != "" {
		t, err := time.Parse(time.DateOnly, *nowFlag)
		if err != nil {
			fail(err)
		}
		now = t
	}

	schedule, err := fetchSchedule()
	if err != nil {
		fail(fmt.Errorf("release schedule: %w", err))
	}
	latest, err := fetchLatestPatches()
	if err != nil {
		fail(fmt.Errorf("releases: %w", err))
	}
	scheduled := map[int]scheduleEntry{}
	for _, e := range schedule {
		if major, err := strconv.Atoi(e.Major); err == nil && e.Minor == "0" {
			scheduled[major] = e
		}
	}

	r := &report{}
	table := forgejo.Versions
	checkDates(r, table, scheduled)
	checkNewMajors(r, table, scheduled, latest, now)
	checkDefaultImage(r, table, now)
	checkAdvisories(r, table, latest)

	var token string
	if *checkRegistry {
		if token, err = registryToken(); err != nil {
			fail(fmt.Errorf("registry token: %w", err))
		}
	}
	printMatrix(table, scheduled, latest, now, token, r)

	fmt.Println()
	if len(r.lines) == 0 {
		fmt.Println("The version table is current.")
	}
	for _, l := range r.lines {
		fmt.Println(l)
	}
	if r.stale {
		os.Exit(1)
	}
}

// checkDates compares the LTS flags and dates of the table's rows with the schedule.
func checkDates(r *report, table forgejo.VersionTable, scheduled map[int]scheduleEntry) {
	for _, rows := range [][]forgejo.Release{table.Releases, table.Upcoming} {
		for _, row := range rows {
			e, ok := scheduled[row.Major]
			if !ok {
				r.add("WARN", "%d is in the table but not in the upstream schedule", row.Major)
				continue
			}
			if e.LTS != row.LTS || e.Release != forgejo.Day(row.Released) || e.EOL != forgejo.Day(row.EOL) {
				r.add("STALE", "%d: upstream lts=%v release=%s eol=%s, table lts=%v release=%s eol=%s",
					row.Major, e.LTS, e.Release, e.EOL, row.LTS, forgejo.Day(row.Released), forgejo.Day(row.EOL))
			}
		}
	}
}

// checkNewMajors reports majors upstream has published or branched that the table does not
// cover yet.
func checkNewMajors(r *report, table forgejo.VersionTable, scheduled map[int]scheduleEntry, latest map[int]patch,
	now time.Time) {
	for _, m := range slices.Sorted(maps.Keys(scheduled)) {
		e := scheduled[m]
		if table.Release(m) != nil || m < table.Oldest().Major {
			continue
		}
		_, published := latest[m]
		switch {
		case published:
			r.add("STALE", "Forgejo %d.0 was released on %s but is not a verified release: verify it and move it into "+
				"Releases (docs/versions.md)", m, e.Release)
		case !isUpcoming(table, m) && e.Cut <= forgejo.Day(now):
			r.add("STALE", "Forgejo %d.0 was branched on %s (test images exist): add it to Upcoming", m, e.Cut)
		case !isUpcoming(table, m):
			r.add("INFO", "Forgejo %d.0 is scheduled for %s (branch cut %s); not in the table yet", m, e.Release, e.Cut)
		}
	}
}

// checkDefaultImage reports a default image that is not the current LTS release.
func checkDefaultImage(r *report, table forgejo.VersionTable, now time.Time) {
	v, ok := forgejo.ImageVersion(forgejov1.DefaultImage)
	if !ok {
		r.add("STALE", "the default image %s has no version tag", forgejov1.DefaultImage)
		return
	}
	if lts := table.CurrentLTS(now); lts != nil && lts.Major != v.Major {
		r.add("STALE", "the default image %s is not the current LTS release %d: change DefaultImage and its "+
			"+kubebuilder:default marker in api/v1alpha1/forgejo_types.go", forgejov1.DefaultImage, lts.Major)
	}
}

// checkAdvisories compares the advisories of the table with the newest patch releases.
func checkAdvisories(r *report, table forgejo.VersionTable, latest map[int]patch) {
	for _, row := range table.Releases {
		p, ok := latest[row.Major]
		if !ok {
			continue
		}
		switch {
		case row.Advisory != nil && p.version.Patch < row.Advisory.FixedIn:
			r.add("INFO", "%d: the advisory's fix %d.0.%d is not released yet (latest %s)",
				row.Major, row.Major, row.Advisory.FixedIn, p.version)
		case strings.Contains(strings.ToLower(p.body), "advise against"):
			r.add("WARN", "%s: the release notes advise against it; set Advisory.FixedIn of %d to %d",
				p.version, row.Major, p.version.Patch+1)
		}
	}
}

type report struct {
	lines []string
	stale bool
}

func (r *report) add(level, format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf("%-5s  %s", level, fmt.Sprintf(format, args...)))
	if level == "STALE" {
		r.stale = true
	}
}

func printMatrix(table forgejo.VersionTable, scheduled map[int]scheduleEntry, latest map[int]patch, now time.Time,
	token string, r *report) {
	fmt.Printf("Forgejo releases as of %s (table: internal/forgejo/versions.go)\n\n", forgejo.Day(now))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "MAJOR\tLTS\tRELEASE\tEOL\tTABLE\tOPERATOR\tLATEST PATCH\tIMAGES")
	for _, m := range slices.Sorted(maps.Keys(scheduled)) {
		if m < table.Oldest().Major-1 {
			continue
		}
		e := scheduled[m]
		in := "-"
		switch {
		case table.Release(m) != nil:
			in = "verified"
		case isUpcoming(table, m):
			in = "upcoming"
		}
		lts := "no"
		if e.LTS {
			lts = "yes"
		}
		level := table.Classify(forgejo.Version{Major: m, Minor: -1, Patch: -1}, true, now).Level
		patchText, images := "-", "-"
		if p, ok := latest[m]; ok {
			patchText = p.version.String()
			if token != "" && e.EOL > forgejo.Day(now) {
				images = imageStatus(token, p.version, r)
			}
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m, lts, e.Release, e.EOL, in, level, patchText, images)
	}
	_ = w.Flush()
}

func isUpcoming(t forgejo.VersionTable, major int) bool {
	for _, u := range t.Upcoming {
		if u.Major == major {
			return true
		}
	}
	return false
}

// imageStatus checks the root and rootless images of a patch release.
func imageStatus(token string, v forgejo.Version, r *report) string {
	var out []string
	for _, tag := range []string{v.String(), v.String() + "-rootless"} {
		ok, err := manifestExists(token, tag)
		switch {
		case err != nil:
			out = append(out, tag+"?")
			r.add("WARN", "registry: %s: %v", tag, err)
		case ok:
			out = append(out, tag)
		default:
			out = append(out, tag+" MISSING")
			r.add("WARN", "codeberg.org/%s:%s is not published", repository, tag)
		}
	}
	return strings.Join(out, ", ")
}

func fetchSchedule() ([]scheduleEntry, error) {
	var s []scheduleEntry
	if err := getJSON(scheduleURL, &s); err != nil {
		return nil, err
	}
	if len(s) == 0 {
		return nil, errors.New("empty schedule")
	}
	return s, nil
}

// fetchLatestPatches returns the newest published N.0.P release per major.
func fetchLatestPatches() (map[int]patch, error) {
	latest := map[int]patch{}
	for page := 1; page <= 30; page++ {
		var rs []release
		url := fmt.Sprintf("%s?draft=false&pre-release=false&limit=50&page=%d", releasesURL, page)
		if err := getJSON(url, &rs); err != nil {
			return nil, err
		}
		if len(rs) == 0 {
			return latest, nil
		}
		for _, rel := range rs {
			m := tagPattern.FindStringSubmatch(rel.TagName)
			if m == nil || rel.Draft || rel.Prerelease {
				continue
			}
			v, _ := forgejo.ParseVersion(rel.TagName)
			if v.Minor != 0 {
				continue
			}
			if cur, ok := latest[v.Major]; !ok || v.Compare(cur.version) > 0 {
				latest[v.Major] = patch{version: v, body: rel.Body}
			}
		}
	}
	return latest, nil
}

func registryToken() (string, error) {
	var t struct {
		Token string `json:"token"`
	}
	url := fmt.Sprintf("%s/v2/token?service=container_registry&scope=repository:%s:pull", registry, repository)
	if err := getJSON(url, &t); err != nil {
		return "", err
	}
	if t.Token == "" {
		return "", errors.New("no token in the answer")
	}
	return t.Token, nil
}

func manifestExists(token, tag string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", registry, repository, tag), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("HTTP %d", resp.StatusCode)
}

func getJSON(url string, into any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "forgejo-versions:", err)
	os.Exit(2)
}
