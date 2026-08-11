package review

import (
	"fmt"
	"strings"
)

// Preflight is the block of mechanical truths computed once from the diff and
// injected into every perspective prompt. Handing these over beats hoping the
// model runs the right git command — twig shipped 11 committed binaries past
// eight perspectives because nothing put that count in front of them. The
// command layer fills this in from git (see repo.Preflight); the review package
// only formats it, so the pipeline stays unit-testable without git.
type Preflight struct {
	// BinaryAdded lists binary files the diff adds (numstat "- -" rows).
	BinaryAdded []string
	// NewFiles is the count of files the diff adds.
	NewFiles int
	// UntrackedSource lists source files present in the worktree but never
	// `git add`ed — invisible to a diff review, so their code ships without its
	// tests and the review says nothing (observed on real PRs).
	UntrackedSource []string
	// AddedSymbols lists top-level symbols the diff adds (for a reuse check).
	AddedSymbols []string
}

// String renders the preflight facts as the prompt block, matching review.sh's
// §6a wording so the injected facts read identically to the twig pipeline's.
func (pf Preflight) String() string {
	var b strings.Builder
	b.WriteString("=== PREFLIGHT FACTS (computed for you — do not re-derive, do act on) ===\n")
	fmt.Fprintf(&b, "Binary files added by this diff: %d\n", len(pf.BinaryAdded))
	for _, f := range pf.BinaryAdded {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	fmt.Fprintf(&b, "New files added: %d\n", pf.NewFiles)
	fmt.Fprintf(&b, "Untracked source files in the worktree (NOT in this diff): %d\n", len(pf.UntrackedSource))
	for _, f := range pf.UntrackedSource {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	b.WriteString("Top-level symbols added:\n")
	if len(pf.AddedSymbols) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, s := range pf.AddedSymbols {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	b.WriteString("\nA non-zero untracked count is a FINDING in its own right: work written but never `git add`ed " +
		"is invisible to this review and will not ship. If an untracked file is a test for code that IS in the diff, " +
		"that is code shipping without its coverage — report it, do not assume it was intentional.\n")
	b.WriteString("A non-zero binary count is a finding unless the file is a genuine product asset. For EVERY added " +
		"symbol above, before accepting it as new work, grep for an existing equivalent and check whether it " +
		"re-implements something the repo already exports rather than composing it. Report what you checked, not just what you found.\n")
	return b.String()
}

// preamble is the generic single-perspective review instruction, including the
// two override rules ported verbatim from review.sh. It is deliberately repo-
// agnostic: the gen2-be "REPO FACTS" block (permission migrations, house
// patterns, …) is repo-specific data and is NOT hardcoded here — a per-repo
// preamble/facts hook is a later addition (see STATUS.md). {{DIFF_CMD}} is the
// only substitution.
const preamble = `You are running a single-perspective code review inside a git worktree.
Review ONLY this diff: ` + "`{{DIFF_CMD}}`" + ` (run it yourself). You may read any repo file and run read-only git/grep commands to verify claims. Do not modify any file.
Follow the perspective instructions below exactly, including the Output contract.
Two rules that override any fix suggestion you are inclined to make:
1. VERDICT COUNTS MUST MATCH THE BODY. Every finding you count in the VERDICT line must appear as a full '### [SEVERITY] title (path:line)' block above it. If you cannot produce the evidence/why/fix for a finding, it does not exist — drop it and lower the count. A verdict claiming findings with no matching blocks is a failed run, not a report.
2. PROPOSE THE STRUCTURALLY CORRECT FIX, NOT THE ONE THAT MAKES THE CHECK PASS. Where a cheap suppression and a real fix both exist, name both and say plainly which is which, so the decider is choosing rather than being nudged.
End with exactly one line: VERDICT {{SLUG}}: blockers=N majors=N minors=N`

// diffCmdPlaceholder / slugPlaceholder are the preamble substitution tokens.
const (
	diffCmdPlaceholder = "{{DIFF_CMD}}"
	slugPlaceholder    = "{{SLUG}}"
)

// PerspectivePrompt assembles the full message for one perspective: the shared
// preamble (with the diff command and the perspective's slug substituted), the
// preflight facts, and the perspective file's own content.
func PerspectivePrompt(p Perspective, pf Preflight, diffCmd string) string {
	head := strings.ReplaceAll(preamble, diffCmdPlaceholder, diffCmd)
	head = strings.ReplaceAll(head, slugPlaceholder, p.Slug)
	return strings.Join([]string{head, "", pf.String(), "---", p.Content}, "\n")
}

// SynthesisPrompt assembles the decision-queue synthesis message from the
// usable report file paths. The rules and output contract are ported from
// review.sh §8: merge findings that share a root cause, take the higher severity
// on disagreement, discard nothing, ignore the inputs' VERDICT lines (they are
// counts, not evidence), and end with a synthesis VERDICT the same postcondition
// will validate.
func SynthesisPrompt(branch, base string, reportPaths []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are synthesizing single-perspective code-review reports into ONE decision queue for a human reviewer. Read these report files: %s\n", strings.Join(reportPaths, " "))
	b.WriteString(`Rules:
- Findings from different perspectives that share a root cause (same file/line or same underlying defect) are ONE entry: merge them, keep the strongest evidence/fix, list every contributing perspective.
- Keep each perspective's severity honest — if two perspectives disagree on severity, take the higher and note it.
- Discard nothing else; the human decides, not you. Order entries: blockers, then majors, then minors.
- Do not invent findings; only synthesize what the reports contain.
- A finding exists only where a report gives you a body (evidence/why/fix) you can read. IGNORE every 'VERDICT' line in the inputs — they are counts, not findings. Never create an entry about the review pipeline itself, a report file, or a missing/short report.
- Your own VERDICT counts must equal the F<n> entries you actually wrote, counted by severity.
Output EXACTLY this structure and nothing else:
`)
	fmt.Fprintf(&b, "# Review synthesis — %s vs %s\n", branch, base)
	b.WriteString("> Human: flip each `decision:` to AGREE or REJECT <short reason>.\n")
	b.WriteString("> Dev session then addresses AGREE items: grep -B8 \"decision: AGREE\" .bowt-review/SYNTHESIS.md\n\n")
	b.WriteString("## Decision queue\n")
	b.WriteString("### F<n> [BLOCKER|MAJOR|MINOR] <title> (`file:line`)\n")
	b.WriteString("- perspectives: <comma-separated slugs>\n")
	b.WriteString("- evidence: <merged strongest evidence>\n")
	b.WriteString("- why: <consequence>\n")
	b.WriteString("- fix: <concrete suggestion>\n")
	b.WriteString("- decision: PENDING\n\n")
	b.WriteString("End with exactly one line: VERDICT synthesis: blockers=N majors=N minors=N (deduped from M raw findings)")
	return b.String()
}
