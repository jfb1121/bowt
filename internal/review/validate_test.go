package review

import (
	"strings"
	"testing"
)

func TestCountFindings(t *testing.T) {
	body := strings.Join([]string{
		"# Report",
		"### [BLOCKER] one (a.go:1)",
		"body",
		"### MAJOR: two (b.go:2)",       // colon grammar, no brackets
		"### F3 [MAJOR] three (c.go:3)", // synthesis numbering prefix
		"#### [MINOR] four (d.go:4)",    // level-4 heading still counts
		"```",
		"### [BLOCKER] not-a-finding-inside-a-fence",
		"```",
		"## Section header that is not a severity",
		"# [MAJOR] single-hash is not a finding block",
	}, "\n")

	got := countFindings(body)
	want := Counts{Blockers: 1, Majors: 2, Minors: 1}
	if got != want {
		t.Fatalf("countFindings = %v; want %v", got, want)
	}
}

func TestParseVerdictTolerant(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Counts
		ok   bool
	}{
		{"bare", "VERDICT s: blockers=1 majors=2 minors=3", Counts{1, 2, 3}, true},
		{"backticked", "`VERDICT s: blockers=0 majors=0 minors=1`", Counts{0, 0, 1}, true},
		{"bold+heading", "### **VERDICT s: blockers=4 majors=0 minors=0**", Counts{4, 0, 0}, true},
		{"trailing text", "VERDICT s: blockers=0 majors=1 minors=0 (deduped from 5)", Counts{0, 1, 0}, true},
		{"last wins", "VERDICT s: blockers=9 majors=9 minors=9\nVERDICT s: blockers=1 majors=1 minors=1", Counts{1, 1, 1}, true},
		{"absent", "no verdict here", Counts{}, false},
		{"partial", "VERDICT s: blockers=1 majors=2", Counts{}, false},
		{"wrong slug", "VERDICT other: blockers=1 majors=1 minors=1", Counts{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseVerdict(tt.body, "s")
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("parseVerdict = (%v,%v); want (%v,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// The four fixture classes the brief names, asserted through ValidateReport.
func TestValidateReport(t *testing.T) {
	longBody := "## Swept\n" + strings.Repeat("checked files and hunt items, all clean. ", 20)

	tests := []struct {
		name       string
		slug       string
		body       string
		wantReject bool
		reasonHas  string
	}{
		{
			name: "valid report accepted",
			slug: "good",
			body: "### [MAJOR] real finding (a.go:1)\n- evidence\n- why\n- fix\n\nVERDICT good: blockers=0 majors=1 minors=0",
		},
		{
			name:       "verdict/body mismatch rejected",
			slug:       "liar",
			body:       "### [MAJOR] one block only (a.go:1)\n- e\n\nVERDICT liar: blockers=0 majors=2 minors=0",
			wantReject: true,
			reasonHas:  "verdict/body mismatch",
		},
		{
			name:       "clean but empty (<300B) rejected",
			slug:       "lazy",
			body:       "VERDICT lazy: blockers=0 majors=0 minors=0",
			wantReject: true,
			reasonHas:  "no evidence of work",
		},
		{
			name: "clean with real body accepted",
			slug: "clean",
			body: longBody + "\n\nVERDICT clean: blockers=0 majors=0 minors=0",
		},
		{
			name: "backticked verdict accepted",
			slug: "fancy",
			body: "### [MINOR] tiny (b.go:2)\n- e\n\n`VERDICT fancy: blockers=0 majors=0 minors=1`",
		},
		{
			name:       "empty report rejected",
			slug:       "void",
			body:       "   \n  ",
			wantReject: true,
			reasonHas:  "empty report",
		},
		{
			name:       "no verdict line rejected",
			slug:       "mute",
			body:       "### [MAJOR] found something (a.go:1)\n- e",
			wantReject: true,
			reasonHas:  "no parseable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := ValidateReport(tt.slug, tt.body)
			if tt.wantReject {
				if reason == "" {
					t.Fatalf("expected rejection, got accept")
				}
				if !strings.Contains(reason, tt.reasonHas) {
					t.Fatalf("reason %q does not contain %q", reason, tt.reasonHas)
				}
				return
			}
			if reason != "" {
				t.Fatalf("expected accept, got rejection: %s", reason)
			}
		})
	}
}

// A clean 0/0/0 body sitting right at the floor boundary is separated correctly.
func TestCleanBodyFloor(t *testing.T) {
	just299 := strings.Repeat("x", 299) + "\nVERDICT s: blockers=0 majors=0 minors=0"
	if r := ValidateReport("s", just299); r == "" {
		t.Fatalf("299 non-space bytes should be rejected")
	}
	just300 := strings.Repeat("x", 300) + "\nVERDICT s: blockers=0 majors=0 minors=0"
	if r := ValidateReport("s", just300); r != "" {
		t.Fatalf("300 non-space bytes should pass, got: %s", r)
	}
}
