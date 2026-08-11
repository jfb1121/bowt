package spawn

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseComms is the writeback→comms derivation contract: a table over the
// three artifacts (PLAN.md ESCALATE, STATUS.md PAUSED ON, .bowt-review
// SYNTHESIS.md counts) × present/absent/malformed. ParseComms is pure (file
// reads only), so the whole matrix runs without a process.
func TestParseComms(t *testing.T) {
	tests := []struct {
		name   string
		plan   *string // PLAN.md contents (nil = file absent)
		status *string // STATUS.md contents (nil = absent)
		synth  *string // .bowt-review/SYNTHESIS.md contents (nil = absent)
		want   Comms
	}{
		{
			name: "empty writeback",
			want: Comms{},
		},
		{
			name: "escalate marker with inline note",
			plan: ptr("# Plan\n**ESCALATE**: no mechanism fits resume-after-followup\nrest"),
			want: Comms{Escalated: true, EscalationNote: "no mechanism fits resume-after-followup"},
		},
		{
			name: "escalate token alone takes next non-empty line as note",
			plan: ptr("# Plan\n### ESCALATE\n\nthe lock primitive has no shared mode\n"),
			want: Comms{Escalated: true, EscalationNote: "the lock primitive has no shared mode"},
		},
		{
			name: "plan present but no escalation",
			plan: ptr("# Plan\nphase 1 → files → tests. All DIRECT FIT.\n"),
			want: Comms{},
		},
		{
			name: "taxonomy legend line is not an escalation",
			plan: ptr("Every decision labelled DIRECT FIT / PLANNED EXTENSION / ESCALATE with cites.\n"),
			want: Comms{},
		},
		{
			name: "escalate inside a fenced block is ignored",
			plan: ptr("# Plan\n```\nexample: mark it ESCALATE here\n```\ndone\n"),
			want: Comms{},
		},
		{
			name:   "paused on with owner",
			status: ptr("# Status\nblocked.\nPAUSED ON alice\n"),
			want:   Comms{Paused: true, PausedOn: "alice"},
		},
		{
			name:   "paused on decorated + heading",
			status: ptr("## `PAUSED ON` platform-team\n"),
			want:   Comms{Paused: true, PausedOn: "platform-team"},
		},
		{
			name:   "paused on without owner still pauses",
			status: ptr("PAUSED ON\n"),
			want:   Comms{Paused: true, PausedOn: ""},
		},
		{
			name:   "status present but not paused",
			status: ptr("# Status\nshipped, opened PR, gate green.\n"),
			want:   Comms{},
		},
		{
			name:  "synthesis counts present",
			synth: ptr("# queue\nVERDICT synthesis: blockers=1 majors=2 minors=3 (deduped from 6)\n"),
			want:  Comms{ReviewBlockers: 1, ReviewMajors: 2, ReviewMinors: 3},
		},
		{
			name:  "synthesis malformed → zero counts",
			synth: ptr("VERDICT synthesis: blockers=1 majors=2\n"),
			want:  Comms{},
		},
		{
			name:   "all three at once",
			plan:   ptr("**ESCALATE** retrofit risk on lock.Probe\n"),
			status: ptr("PAUSED ON bob\n"),
			synth:  ptr("VERDICT synthesis: blockers=0 majors=0 minors=2\n"),
			want: Comms{
				Escalated: true, EscalationNote: "retrofit risk on lock.Probe",
				Paused: true, PausedOn: "bob",
				ReviewMinors: 2,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wb := t.TempDir()
			reviewDir := t.TempDir()
			writeIf(t, filepath.Join(wb, "PLAN.md"), tt.plan)
			writeIf(t, filepath.Join(wb, "STATUS.md"), tt.status)
			writeIf(t, filepath.Join(reviewDir, "SYNTHESIS.md"), tt.synth)

			got, err := ParseComms(wb, reviewDir)
			if err != nil {
				t.Fatalf("ParseComms: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseComms = %+v; want %+v", got, tt.want)
			}
		})
	}
}

// A file that exists but cannot be read (a directory in its place) is a genuine
// error, not a silent zero value.
func TestParseCommsReadError(t *testing.T) {
	wb := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wb, "PLAN.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseComms(wb, t.TempDir()); err == nil {
		t.Fatal("want an error when PLAN.md cannot be read (a directory in its place)")
	}
}

func ptr(s string) *string { return &s }

func writeIf(t *testing.T, path string, content *string) {
	t.Helper()
	if content == nil {
		return
	}
	if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
		t.Fatal(err)
	}
}
