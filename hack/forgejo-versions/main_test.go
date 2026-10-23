package main

import (
	"strings"
	"testing"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

func TestCheckAdvisories(t *testing.T) {
	table := func(adv *forgejo.Advisory) forgejo.VersionTable {
		return forgejo.VersionTable{Releases: []forgejo.Release{{Major: 17, UpgradeFrom: 15, Advisory: adv}}}
	}
	fixedIn1 := &forgejo.Advisory{FixedIn: 1, Note: "upstream advises against 17.0.0"}
	cases := []struct {
		name  string
		table forgejo.VersionTable
		patch forgejo.Version
		notes string
		want  []string // the expected findings, each a substring of one line
	}{
		{
			name:  "the edited 17.0.0 notes advise against 17.0.0",
			table: table(nil),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 0},
			notes: "## Release notes\n\nWe currently advise against upgrading to Forgejo 17.0.0: pushes and\n" +
				"merges can fail in some repositories (forgejo#14866).\n\n- Strict durations.",
			want: []string{"WARN   17.0.0: the release notes advise against it", "set Advisory.FixedIn of 17 to 1"},
		},
		{
			name:  "17.0.1 lifts the advisory against 17.0.0",
			table: table(fixedIn1),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 1},
			notes: "This release fixes forgejo#14866. We no longer advise against 17.0.0; upgrade to 17.0.1 instead.",
			want:  []string{`INFO   17.0.1: the release notes mention an advisory: "We no longer advise against 17.0.0"`},
		},
		{
			name:  "17.0.1 advises against itself",
			table: table(fixedIn1),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 1},
			notes: "- Fix merges.\n- We advise against v17.0.1 too, wait for the next patch release.",
			want:  []string{"WARN   17.0.1: the release notes advise against it", "set Advisory.FixedIn of 17 to 2"},
		},
		{
			name:  "a longer version with the same prefix is another version",
			table: table(fixedIn1),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 1},
			notes: "We advise against 17.0.10.",
			want:  []string{"INFO   17.0.1: the release notes mention an advisory"},
		},
		{
			name:  "the advisory's fix is not released yet",
			table: table(&forgejo.Advisory{FixedIn: 2}),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 1},
			notes: "We advise against 17.0.1.",
			want:  []string{"INFO   17: the advisory's fix 17.0.2 is not released yet (latest 17.0.1)"},
		},
		{
			name:  "notes without an advisory",
			table: table(fixedIn1),
			patch: forgejo.Version{Major: 17, Minor: 0, Patch: 1},
			notes: "Bug fixes.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &report{}
			checkAdvisories(r, c.table, map[int]patch{17: {version: c.patch, body: c.notes}})
			got := strings.Join(r.lines, "\n")
			if len(c.want) == 0 && len(r.lines) != 0 {
				t.Fatalf("want no findings, got:\n%s", got)
			}
			if len(c.want) > 0 && len(r.lines) != 1 {
				t.Fatalf("want one finding, got:\n%s", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("want a finding containing %q, got:\n%s", w, got)
				}
			}
			if r.stale {
				t.Errorf("advisory findings must not make the table stale")
			}
		})
	}
}

func TestSentences(t *testing.T) {
	got := sentences("First 17.0.1 sentence. Second\nline!\n\n- item one\n- item two; clause\n# Heading")
	want := []string{"First 17.0.1 sentence", "Second line", "item one", "item two", "clause", "Heading"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sentences = %q, want %q", got, want)
	}
}
