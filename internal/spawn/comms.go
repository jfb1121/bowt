package spawn

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jfb1121/bowt/internal/review"
)

// Comms is the derived communication state of a finished lane: an escalation
// flag + note (from the plan agent's PLAN.md), a pause request + owner (from the
// impl agent's STATUS.md), and the review C/S/N triple (from the review
// pipeline's SYNTHESIS.md). bowt parses these ONCE at capture and stores the
// scalars on the lane row, so the orchestrator reads fields instead of grepping
// prose ("escalations surfaced, not grepped" — rfc/lanes.md's comms protocol).
//
// The prose stays in the files on disk (files-as-truth); Comms is only the
// projection over them. It carries no status — the supervisor applies the
// precedence (a pause request wins over a clean terminal status; an escalation
// only sets the flag) when it folds Comms into the lane.
type Comms struct {
	// Escalated is set when PLAN.md carries an ESCALATE marker (plan.md step 3:
	// "mark it ESCALATE"); EscalationNote is the human-readable reason (the marked
	// line, or the first non-empty line of its block).
	Escalated      bool
	EscalationNote string

	// Paused is set when STATUS.md carries a "PAUSED ON <owner>" marker
	// (owner-only stop-and-wait); PausedOn is the owner text that follows it (may
	// be empty when the marker is present but malformed — Paused still holds, so
	// automation halts on the conservative side).
	Paused   bool
	PausedOn string

	// Review C/S/N from .bowt-review/SYNTHESIS.md via review.SynthesisCounts. Zero
	// when no synthesis verdict is present (no review run, or a malformed queue).
	ReviewBlockers int
	ReviewMajors   int
	ReviewMinors   int
}

// escalateToken is the marker the plan wrapper tells the agent to write when it
// cannot land a mechanism decision and needs the human owner (plan.md step 3).
const escalateToken = "ESCALATE"

// pausePrefix is the owner-only stop-and-wait marker an impl agent writes into
// STATUS.md; the text after it is the owner who owns the resume.
const pausePrefix = "PAUSED ON"

// ParseComms derives a lane's Comms from its writeback in a SINGLE pass at
// capture (schema principle: "parse once, store scalars"). writebackDir holds
// the agent handshake artifacts (PLAN.md / STATUS.md); reviewDir is the review
// pipeline's output dir (.bowt-review) that holds SYNTHESIS.md. It is PURE — a
// few file reads are its only input — so it is exhaustively table-tested without
// a process.
//
// A missing file is normal (not every lane escalates, pauses, or is reviewed)
// and yields the zero value for that facet; only a genuine read error (a file
// that exists but cannot be read) is returned as an error.
func ParseComms(writebackDir, reviewDir string) (Comms, error) {
	var c Comms

	planBody, err := readIfPresent(filepath.Join(writebackDir, "PLAN.md"))
	if err != nil {
		return Comms{}, err
	}
	c.Escalated, c.EscalationNote = scanEscalation(planBody)

	statusBody, err := readIfPresent(filepath.Join(writebackDir, "STATUS.md"))
	if err != nil {
		return Comms{}, err
	}
	c.Paused, c.PausedOn = scanPause(statusBody)

	synthBody, err := readIfPresent(filepath.Join(reviewDir, "SYNTHESIS.md"))
	if err != nil {
		return Comms{}, err
	}
	if counts, ok := review.SynthesisCounts(synthBody); ok {
		c.ReviewBlockers = counts.Blockers
		c.ReviewMajors = counts.Majors
		c.ReviewMinors = counts.Minors
	}

	return c, nil
}

// readIfPresent returns a file's contents, or "" when it does not exist. A
// not-exist is the common case (the artifact was never written) and is not an
// error; any other read failure is.
func readIfPresent(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read comms artifact %s: %w", path, err)
	}
	return string(b), nil
}

// scanEscalation reports whether PLAN.md marks an ESCALATE decision and returns
// its note. The plan wrapper both TELLS the agent to "mark it ESCALATE" AND, in
// the same document, asks it to label every mechanism decision one of "DIRECT
// FIT / PLANNED EXTENSION / ESCALATE" — so a plan that merely restates that
// three-way legend is NOT an escalation. We disambiguate by ignoring any line
// that also names the other two verdicts (the legend always lists all three);
// an ESCALATE that stands on its own line is the real marker. Fenced code blocks
// are skipped so a documentation example never trips the flag (mirrors
// review.countFindings). The note is the marker line with markdown decoration
// and the ESCALATE token stripped; if that leaves nothing, the first non-empty
// line after it is used.
func scanEscalation(body string) (bool, string) {
	lines := strings.Split(body, "\n")
	fence := false
	for i, raw := range lines {
		if isFenceLine(raw) {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		if !strings.Contains(raw, escalateToken) {
			continue
		}
		if isLegendLine(raw) {
			continue // the "DIRECT FIT / PLANNED EXTENSION / ESCALATE" taxonomy, not a marker
		}
		note := escalationNote(raw)
		if note == "" {
			note = firstNonEmpty(lines[i+1:])
		}
		return true, note
	}
	return false, ""
}

// isLegendLine reports whether a line is merely enumerating the three-way
// mechanism-decision taxonomy rather than raising an escalation. The legend is
// the only place all three verdicts co-occur, so the presence of either sibling
// verdict marks the line as legend, not marker.
func isLegendLine(line string) bool {
	return strings.Contains(line, "DIRECT FIT") || strings.Contains(line, "PLANNED EXTENSION")
}

// escalationNote strips markdown decoration, heading hashes, and list bullets
// from a marker line, removes the ESCALATE token and any leading ":" / "-", and
// returns the trimmed remainder (the reason the agent wrote alongside the mark).
func escalationNote(line string) string {
	s := strings.TrimSpace(commsDecoration.Replace(line))
	s = strings.TrimLeft(s, "#-*> \t")
	idx := strings.Index(s, escalateToken)
	if idx < 0 {
		return strings.TrimSpace(s)
	}
	s = s[idx+len(escalateToken):]
	s = strings.TrimLeft(s, " \t:-—–)")
	return strings.TrimSpace(s)
}

// scanPause reports whether STATUS.md carries a "PAUSED ON <owner>" marker and
// returns the owner. Fenced code blocks are skipped. The marker present with no
// owner text still pauses (Paused=true, PausedOn="") — halting automation is the
// conservative action for a malformed pause.
func scanPause(body string) (bool, string) {
	fence := false
	for _, raw := range strings.Split(body, "\n") {
		if isFenceLine(raw) {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		s := strings.TrimSpace(commsDecoration.Replace(raw))
		s = strings.TrimLeft(s, "#-*> \t")
		if !strings.HasPrefix(s, pausePrefix) {
			continue
		}
		owner := strings.TrimSpace(s[len(pausePrefix):])
		owner = strings.TrimLeft(owner, ":-—–) \t")
		return true, strings.TrimSpace(owner)
	}
	return false, ""
}

// commsDecoration strips the markdown emphasis a model might wrap a marker in,
// so "**ESCALATE**" / "`PAUSED ON alice`" are matched like the bare tokens
// (mirrors review.verdictDecoration).
var commsDecoration = strings.NewReplacer("`", "", "*", "", "_", "", "~", "")

// isFenceLine reports whether a line toggles a fenced code block.
func isFenceLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// firstNonEmpty returns the first line with non-whitespace content, decoration
// stripped, or "".
func firstNonEmpty(lines []string) string {
	for _, raw := range lines {
		s := strings.TrimSpace(commsDecoration.Replace(raw))
		s = strings.TrimLeft(s, "#-*> \t")
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
