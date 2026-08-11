// Package spawn assembles the prompt handed to a headless coding agent.
//
// The plan/impl wrapper text is the highest-leverage policy in the repo — it
// applies to every agent run — so it lives in versioned files under prompts/
// (embedded into the binary) rather than as a string literal. spawn loads the
// mode's prompt, stamps a provenance line the agent is told to copy into its
// writeback, and substitutes the one placeholder, {{BRIEF}}, with the resolved
// brief. See rfc/versioned-spawn-prompts.md.
package spawn

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git object hashing is defined in terms of SHA-1; not a security use.
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// embedded ships the versioned prompt files inside the binary, so an installed
// bowt carries its policy with it (no lookup relative to an install dir).
//
//go:embed prompts/plan.md prompts/impl.md prompts/VERSION
var embedded embed.FS

// briefPlaceholder is the single substitution token. Keep it dumb — one
// placeholder, plain string replace, no templating engine (see the RFC).
const briefPlaceholder = "{{BRIEF}}"

// Mode selects which versioned wrapper to load.
type Mode string

const (
	// ModePlan is the planning wrapper (validate + writeback, no production code).
	ModePlan Mode = "plan"
	// ModeImpl is the implementation wrapper (validate, implement, PR, STATUS.md).
	ModeImpl Mode = "impl"
)

// File is the prompt filename for the mode ("plan.md" / "impl.md").
func (m Mode) File() string { return string(m) + ".md" }

// Label is the human description printed in the spawn header.
func (m Mode) Label() string {
	if m == ModeImpl {
		return "impl"
	}
	return "plan + writeback"
}

// loadPrompt reads name from fsys, treating a missing OR empty file as a hard
// error — never a silent fallback to an empty prompt (an empty prompt is the
// worst failure mode: the agent runs with no policy at all).
func loadPrompt(fsys fs.FS, name string) ([]byte, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("load prompt %s: %w", name, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("prompt %s is empty", name)
	}
	return b, nil
}

// version returns the trimmed contents of the embedded VERSION file.
func version(fsys fs.FS) (string, error) {
	b, err := loadPrompt(fsys, "prompts/VERSION")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// gitBlobHash computes the git object hash of b — the same value `git
// hash-object` prints — so a prompt's provenance hash matches what git reports
// for the committed file. We compute it in-process rather than shelling out to
// git so provenance works even where the embedded bytes aren't a git blob.
func gitBlobHash(b []byte) string {
	h := sha1.New() //nolint:gosec // see package import note.
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Assembled is the result of building a spawn prompt: the full message handed
// to the agent (provenance line prepended) plus the provenance line on its own
// for the header echo.
type Assembled struct {
	Prompt     string
	Provenance string
}

// Assemble loads the mode's embedded prompt, computes the provenance line, and
// substitutes {{BRIEF}} with brief. The provenance line is prepended to the
// prompt (the wrapper text tells the agent to copy it into every writeback).
// A missing or empty prompt is a hard error.
func Assemble(mode Mode, brief string) (Assembled, error) {
	return assemble(embedded, mode, brief)
}

func assemble(fsys fs.FS, mode Mode, brief string) (Assembled, error) {
	raw, err := loadPrompt(fsys, "prompts/"+mode.File())
	if err != nil {
		return Assembled{}, err
	}
	ver, err := version(fsys)
	if err != nil {
		return Assembled{}, err
	}
	full := gitBlobHash(raw)
	prov := fmt.Sprintf("prompt: %s @ %s (%s)", mode.File(), ver, full[:7])
	body := strings.ReplaceAll(string(raw), briefPlaceholder, brief)
	return Assembled{
		Prompt:     prov + "\n\n" + body,
		Provenance: prov,
	}, nil
}

// briefCandidates are the relative paths tried, in order, when no explicit
// brief is passed. First match wins. (twig also checks docs/*_PROMPT.md; bowt's
// brief scopes this to these three — noted in the slice STATUS.)
var briefCandidates = []string{"subagent/PROMPT.md", "subagent/*-prompt.md", "PROMPT.md"}

// ResolveBrief finds the brief file for a spawn rooted at dir and returns its
// display path (relative to dir) and its contents with subagent/FOLLOWUP.md
// appended when present. An explicit arg overrides the search. A missing brief
// is a hard error.
func ResolveBrief(dir, arg string) (path, content string, err error) {
	rel, err := pickBrief(dir, arg)
	if err != nil {
		return "", "", err
	}
	abs := rel
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, rel)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", "", fmt.Errorf("read brief %s: %w", rel, err)
	}
	content = string(b)

	// Auto-append the orchestrator follow-up if present (feedback on a prior pass).
	fu := filepath.Join(dir, "subagent", "FOLLOWUP.md")
	if data, ferr := os.ReadFile(fu); ferr == nil && len(bytes.TrimSpace(data)) > 0 {
		content += "\n\n=== FOLLOW-UP — read after the brief (subagent/FOLLOWUP.md) ===\n" +
			string(data) +
			"\n\nNote: a prior pass already wrote findings to subagent/writeback/; read those, fold in this follow-up, then proceed."
	}
	return rel, content, nil
}

// pickBrief resolves the brief path (relative to dir), honoring an explicit arg
// first, then the candidate list. Returns a hard error if nothing matches.
func pickBrief(dir, arg string) (string, error) {
	if arg != "" {
		abs := arg
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, arg)
		}
		if !isFile(abs) {
			return "", fmt.Errorf("brief not found: %s", arg)
		}
		return arg, nil
	}
	for _, cand := range briefCandidates {
		if strings.ContainsAny(cand, "*?[") {
			matches, _ := filepath.Glob(filepath.Join(dir, cand))
			sort.Strings(matches) // deterministic pick when several match
			for _, m := range matches {
				if isFile(m) {
					if r, rerr := filepath.Rel(dir, m); rerr == nil {
						return r, nil
					}
					return m, nil
				}
			}
			continue
		}
		if isFile(filepath.Join(dir, cand)) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no brief found — pass a path, or add subagent/PROMPT.md")
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// ResolveModel maps model aliases to full IDs and applies the plan-mode default
// (planning is design work, so it gets the strongest model unless overridden).
// Unknown values pass through to the agent verbatim.
func ResolveModel(model string, impl bool) string {
	switch model {
	case "opus":
		model = "claude-opus-4-8"
	case "sonnet":
		model = "claude-sonnet-5"
	case "haiku":
		model = "claude-haiku-4-5-20251001"
	}
	if model == "" && !impl {
		model = "claude-opus-4-8"
	}
	return model
}

// ResolveEffort applies the plan-mode default (high effort for design work);
// impl inherits the session default unless the orchestrator picks one.
func ResolveEffort(effort string, impl bool) string {
	if effort == "" && !impl {
		effort = "high"
	}
	return effort
}
