package spawn

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSupervisorArgs(t *testing.T) {
	got := SupervisorArgs("/wt/.bowt/lane-x.spec.json")
	want := []string{"_lane-run", "/wt/.bowt/lane-x.spec.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupervisorArgs = %v; want %v", got, want)
	}
	if got[0] != LaneRunCommand {
		t.Errorf("first arg = %q; want the hidden verb %q", got[0], LaneRunCommand)
	}
}

func TestLanePaths(t *testing.T) {
	if got := LaneLogPath("/wt", "abc"); got != filepath.FromSlash("/wt/.bowt/lane-abc.log") {
		t.Errorf("LaneLogPath = %q", got)
	}
	if got := SpecPath("/wt", "abc"); got != filepath.FromSlash("/wt/.bowt/lane-abc.spec.json") {
		t.Errorf("SpecPath = %q", got)
	}
}

// TestSpecRoundTrip proves the launcher→supervisor handoff survives a real
// write+read: WriteSpec creates <worktree>/.bowt and ReadSpecAt decodes an
// identical struct (including the potentially large prompt and the deps slice).
func TestSpecRoundTrip(t *testing.T) {
	wt := t.TempDir()
	in := LaneSpec{
		ID: "lane-1", Ticket: "ENG-1", Repo: "demo", Branch: "slice/x", Worktree: wt,
		Agent: "claude", Model: "opus", Effort: "high", Mode: "impl",
		PromptVersion: "3", PromptHash: "deadbeef", BriefPath: "subagent/PROMPT.md", BriefHash: "cafef00d",
		WritebackDir: DefaultWritebackDir, LogPath: LaneLogPath(wt, "lane-1"),
		Wave: 2, Deps: []string{"a", "b"}, Prompt: "the fully\nassembled prompt",
	}
	path, err := WriteSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	if path != SpecPath(wt, "lane-1") {
		t.Errorf("WriteSpec path = %q; want %q", path, SpecPath(wt, "lane-1"))
	}
	got, err := ReadSpecAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("spec round-trip mismatch:\n in=%+v\ngot=%+v", in, got)
	}
}

func TestReadSpecMissing(t *testing.T) {
	if _, err := ReadSpecAt(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("ReadSpecAt(missing) should error")
	} else if !strings.Contains(err.Error(), "read lane spec") {
		t.Errorf("error should name the operation; got %q", err)
	}
}
