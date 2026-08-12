package spawn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfb1121/bowt/internal/state"
)

// TestTerminalStatus is the completion→status contract: a table over
// {plan,impl} × {exit 0, exit n} × {writeback artifact present/absent}.
func TestTerminalStatus(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		exitCode int
		artifact string // file to create in the writeback dir ("" = none)
		want     state.Status
	}{
		{"plan exit0 with PLAN.md", "plan", 0, "PLAN.md", state.StatusPlanReview},
		{"plan exit0 no PLAN.md", "plan", 0, "", state.StatusFailed},
		{"plan nonzero with PLAN.md", "plan", 1, "PLAN.md", state.StatusFailed},
		{"plan nonzero no PLAN.md", "plan", 2, "", state.StatusFailed},

		{"impl exit0 with STATUS.md", "impl", 0, "STATUS.md", state.StatusReview},
		{"impl exit0 no STATUS.md", "impl", 0, "", state.StatusFailed},
		{"impl nonzero with STATUS.md", "impl", 1, "STATUS.md", state.StatusFailed},
		{"impl nonzero no STATUS.md", "impl", 3, "", state.StatusFailed},

		// orch mirrors impl for now: STATUS.md → review, else failed.
		{"orch exit0 with STATUS.md", "orch", 0, "STATUS.md", state.StatusReview},
		{"orch exit0 no STATUS.md", "orch", 0, "", state.StatusFailed},
		{"orch nonzero with STATUS.md", "orch", 1, "STATUS.md", state.StatusFailed},
		{"orch exit0 with PLAN.md only", "orch", 0, "PLAN.md", state.StatusFailed},

		// The wrong artifact for the mode does not count as success.
		{"plan exit0 with STATUS.md only", "plan", 0, "STATUS.md", state.StatusFailed},
		{"impl exit0 with PLAN.md only", "impl", 0, "PLAN.md", state.StatusFailed},

		// A negative exit code (non-exit failure, e.g. signalled) is still failed.
		{"impl signalled", "impl", -1, "STATUS.md", state.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wb := t.TempDir()
			if tt.artifact != "" {
				if err := os.WriteFile(filepath.Join(wb, tt.artifact), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := TerminalStatus(tt.mode, tt.exitCode, wb)
			if err != nil {
				t.Fatalf("TerminalStatus: %v", err)
			}
			if got != tt.want {
				t.Errorf("TerminalStatus(%q, %d, artifact=%q) = %q; want %q", tt.mode, tt.exitCode, tt.artifact, got, tt.want)
			}
		})
	}
}

// A directory named like the artifact is not the artifact (fileExists rejects it).
func TestTerminalStatusIgnoresDir(t *testing.T) {
	wb := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wb, "STATUS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := TerminalStatus("impl", 0, wb)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.StatusFailed {
		t.Errorf("a STATUS.md *directory* should not count as the artifact; got %q", got)
	}
}

// An unknown mode is a hard error, not a silent status.
func TestTerminalStatusUnknownMode(t *testing.T) {
	if _, err := TerminalStatus("review", 0, t.TempDir()); err == nil {
		t.Fatal("unknown mode should error")
	}
}
