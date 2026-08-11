package spawn

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// TestResolveBriefOrder pins the brief-resolution precedence:
// subagent/PROMPT.md → subagent/*-prompt.md → PROMPT.md, first match wins.
func TestResolveBriefOrder(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		wantPath string
	}{
		{
			name:     "subagent/PROMPT.md wins over everything",
			files:    map[string]string{"subagent/PROMPT.md": "A", "subagent/x-prompt.md": "B", "PROMPT.md": "C"},
			wantPath: "subagent/PROMPT.md",
		},
		{
			name:     "glob is next when PROMPT.md absent",
			files:    map[string]string{"subagent/x-prompt.md": "B", "PROMPT.md": "C"},
			wantPath: "subagent/x-prompt.md",
		},
		{
			name:     "glob picks deterministically (sorted) among several",
			files:    map[string]string{"subagent/z-prompt.md": "Z", "subagent/a-prompt.md": "A"},
			wantPath: "subagent/a-prompt.md",
		},
		{
			name:     "PROMPT.md is the final fallback",
			files:    map[string]string{"PROMPT.md": "C"},
			wantPath: "PROMPT.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tt.files)
			got, _, err := ResolveBrief(dir, "")
			if err != nil {
				t.Fatalf("ResolveBrief: %v", err)
			}
			if filepath.ToSlash(got) != tt.wantPath {
				t.Fatalf("path = %q; want %q", got, tt.wantPath)
			}
		})
	}
}

// A missing brief is a hard error, never a silent empty brief.
func TestResolveBriefMissingIsError(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ResolveBrief(dir, ""); err == nil {
		t.Fatal("want error when no brief exists")
	}
	// An explicit but absent path is also an error.
	if _, _, err := ResolveBrief(dir, "nope.md"); err == nil {
		t.Fatal("want error for a missing explicit brief")
	}
}

// An explicit brief arg overrides the search order.
func TestResolveBriefExplicitArg(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"subagent/PROMPT.md": "auto", "custom.md": "explicit body"})
	got, content, err := ResolveBrief(dir, "custom.md")
	if err != nil {
		t.Fatalf("ResolveBrief: %v", err)
	}
	if got != "custom.md" {
		t.Fatalf("path = %q; want custom.md", got)
	}
	if content != "explicit body" {
		t.Fatalf("content = %q; want explicit body", content)
	}
}

// FOLLOWUP.md is auto-appended to the brief when present.
func TestResolveBriefAppendsFollowup(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"subagent/PROMPT.md":   "the brief",
		"subagent/FOLLOWUP.md": "the follow-up",
	})
	_, content, err := ResolveBrief(dir, "")
	if err != nil {
		t.Fatalf("ResolveBrief: %v", err)
	}
	if !strings.Contains(content, "the brief") || !strings.Contains(content, "the follow-up") {
		t.Fatalf("content missing brief or follow-up:\n%s", content)
	}
	if !strings.Contains(content, "FOLLOW-UP") {
		t.Fatalf("follow-up not delimited:\n%s", content)
	}
}

// {{BRIEF}} is substituted (dumbly) and the provenance line is prepended.
func TestAssembleSubstitutesAndPrepends(t *testing.T) {
	a, err := Assemble(ModeImpl, "MY-UNIQUE-BRIEF-BODY")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(a.Prompt, briefPlaceholder) {
		t.Fatal("{{BRIEF}} placeholder was not substituted")
	}
	if !strings.Contains(a.Prompt, "MY-UNIQUE-BRIEF-BODY") {
		t.Fatal("brief body not substituted into the prompt")
	}
	if !strings.HasPrefix(a.Prompt, a.Provenance) {
		t.Fatalf("provenance not prepended; prompt starts:\n%q", a.Prompt[:80])
	}
}

// The provenance line matches `prompt: <mode>.md @ <ver> (<hash>)`.
func TestProvenanceFormat(t *testing.T) {
	re := regexp.MustCompile(`^prompt: (plan|impl)\.md @ \S+ \([0-9a-f]{7}\)$`)
	for _, mode := range []Mode{ModePlan, ModeImpl} {
		a, err := Assemble(mode, "x")
		if err != nil {
			t.Fatalf("Assemble(%s): %v", mode, err)
		}
		if !re.MatchString(a.Provenance) {
			t.Fatalf("provenance %q does not match %s", a.Provenance, re)
		}
		if !strings.HasPrefix(a.Provenance, "prompt: "+mode.File()+" @ ") {
			t.Fatalf("provenance %q missing mode %s", a.Provenance, mode.File())
		}
	}
}

// gitBlobHash equals what `git hash-object` computes for the same bytes.
func TestGitBlobHashMatchesGit(t *testing.T) {
	content := []byte("hello spawn provenance\n")
	got := gitBlobHash(content)
	// Precomputed: printf 'hello spawn provenance\n' | git hash-object --stdin
	// yields the SHA-1 of "blob 23\x00hello spawn provenance\n".
	want := gitHashObject(t, content)
	if got != want {
		t.Fatalf("gitBlobHash = %s; git hash-object = %s", got, want)
	}
}

// A missing or empty prompt is a hard error (via loadPrompt), never silent.
func TestLoadPromptHardErrors(t *testing.T) {
	fsys := fstest.MapFS{
		"prompts/empty.md":      &fstest.MapFile{Data: []byte("   \n\t")},
		"prompts/whitespace.md": &fstest.MapFile{Data: []byte("")},
	}
	if _, err := loadPrompt(fsys, "prompts/missing.md"); err == nil {
		t.Fatal("want error for a missing prompt")
	}
	if _, err := loadPrompt(fsys, "prompts/empty.md"); err == nil {
		t.Fatal("want error for a whitespace-only prompt")
	}
	if _, err := loadPrompt(fsys, "prompts/whitespace.md"); err == nil {
		t.Fatal("want error for an empty prompt")
	}
	// assemble against an FS missing VERSION is also a hard error.
	if _, err := assemble(fstest.MapFS{"prompts/impl.md": &fstest.MapFile{Data: []byte("x")}}, ModeImpl, "b"); err == nil {
		t.Fatal("want error when VERSION is absent")
	}
}

// Clause-survival: assert the load-bearing clauses survive edits to the
// embedded prompts. This is deliberately NOT a whole-text snapshot (which would
// fail on every wording change and get deleted) — just the clauses whose loss
// silently degrades every future lane.
func TestEmbeddedPromptClausesSurvive(t *testing.T) {
	impl, err := loadPrompt(embedded, "prompts/impl.md")
	if err != nil {
		t.Fatalf("load impl.md: %v", err)
	}
	plan, err := loadPrompt(embedded, "prompts/plan.md")
	if err != nil {
		t.Fatalf("load plan.md: %v", err)
	}
	implS, planS := strings.ToLower(string(impl)), strings.ToLower(string(plan))

	// impl mode: the gate + the never-background rule + writeback + placeholder.
	for _, want := range []string{
		"make check",
		"foreground",
		"never background",
		"subagent/writeback",
		"status.md",
		"provenance",
		"{{brief}}",
	} {
		if !strings.Contains(implS, want) {
			t.Errorf("impl.md missing load-bearing clause %q", want)
		}
	}

	// plan mode: the three mechanism outcomes + writeback + placeholder.
	for _, want := range []string{
		"direct fit",
		"planned extension",
		"escalate",
		"subagent/writeback",
		"validation.md",
		"plan.md",
		"provenance",
		"{{brief}}",
	} {
		if !strings.Contains(planS, want) {
			t.Errorf("plan.md missing load-bearing clause %q", want)
		}
	}
}

func TestResolveModelAndEffort(t *testing.T) {
	tests := []struct {
		model, effort string
		impl          bool
		wantModel     string
		wantEffort    string
	}{
		{"opus", "", false, "claude-opus-4-8", "high"},
		{"sonnet", "medium", true, "claude-sonnet-5", "medium"},
		{"", "", false, "claude-opus-4-8", "high"},           // plan defaults
		{"", "", true, "", ""},                               // impl inherits session default
		{"my-custom-model", "", true, "my-custom-model", ""}, // unknown passes through
	}
	for _, tt := range tests {
		if got := ResolveModel(tt.model, tt.impl); got != tt.wantModel {
			t.Errorf("ResolveModel(%q,%v) = %q; want %q", tt.model, tt.impl, got, tt.wantModel)
		}
		if got := ResolveEffort(tt.effort, tt.impl); got != tt.wantEffort {
			t.Errorf("ResolveEffort(%q,%v) = %q; want %q", tt.effort, tt.impl, got, tt.wantEffort)
		}
	}
}

// --- helpers ---

// gitHashObject returns `git hash-object --stdin` for content, skipping the
// test if git is unavailable — we verify gitBlobHash matches the real thing.
func gitHashObject(t *testing.T, content []byte) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command(git, "hash-object", "--stdin")
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
