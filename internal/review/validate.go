// Package review is bowt's perspective code-review pipeline: fan a diff out to
// several single-perspective reviewers (each an agent one-shot), then refuse to
// present any report whose VERDICT lies about what its body contains.
//
// The value of the pipeline is not the fan-out — it is the validation
// postconditions in this file. A report whose VERDICT claims findings the body
// does not carry is worse than a failed run: the count is unactionable AND
// untrustworthy, and a synthesis pass then manufactures a phantom decision-queue
// entry from it. The perspective prompt already forbids the mismatch; a prompt
// is not a postcondition. These helpers make the harness reject such a report as
// a failed run — ported faithfully from twig's review.sh (§ structural
// postconditions).
package review

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Counts is the (blockers, majors, minors) triple a report both claims (in its
// VERDICT line) and demonstrates (as finding blocks). The postcondition is that
// these two are equal.
type Counts struct {
	Blockers int
	Majors   int
	Minors   int
}

// String renders the triple space-separated, matching review.sh's "b m n" so
// rejection reasons read identically to the bash pipeline's.
func (c Counts) String() string {
	return fmt.Sprintf("%d %d %d", c.Blockers, c.Majors, c.Minors)
}

// cleanBodyFloor is the minimum non-whitespace body size (bytes, excluding the
// VERDICT line) a clean 0/0/0 report must carry. Verdict/body consistency cannot
// catch a report that is nothing but "VERDICT …: 0 0 0": zero claimed against
// zero present is self-consistent. Observed clean reports run 1.8-3.6 KB and the
// smallest empty fakes were 49-60 bytes, so this floor separates "found nothing"
// from "did nothing" without arguing about how thorough is thorough.
const cleanBodyFloor = 300

var (
	// reFence toggles fenced-code state; a "### [MAJOR]" inside a ``` block is a
	// documentation example, not a finding, and must not be counted.
	reFence = regexp.MustCompile("^[[:space:]]*```")
	// reHeading matches an ATX heading of level >= 2 ("## ", "### ", …).
	reHeading = regexp.MustCompile(`^#{2,}[[:space:]]`)
	// reHashPrefix strips the leading hashes+space so the severity token is first.
	reHashPrefix = regexp.MustCompile(`^#+[[:space:]]*`)
	// reFNum strips a synthesis-style "F3 " numbering prefix ("### F3 [MAJOR] t").
	reFNum = regexp.MustCompile(`^F[0-9]+[[:space:]]+`)
	// Per-severity matchers accept the grammar variants seen in the wild:
	// "[MAJOR] t", "MAJOR: t", "MAJOR t" (brackets optional, colon or space).
	reBlocker = regexp.MustCompile(`^\[?BLOCKER\]?([[:space:]:]|$)`)
	reMajor   = regexp.MustCompile(`^\[?MAJOR\]?([[:space:]:]|$)`)
	reMinor   = regexp.MustCompile(`^\[?MINOR\]?([[:space:]:]|$)`)
)

// countFindings counts "### [SEVERITY] …" finding blocks in a report body,
// skipping anything inside a fenced code block. It accepts the heading grammar
// variants review.sh documents: "### [MAJOR] t", "### MAJOR: t", and the
// synthesis numbering "### F3 [MAJOR] t".
func countFindings(body string) Counts {
	var c Counts
	fence := false
	for _, line := range strings.Split(body, "\n") {
		if reFence.MatchString(line) {
			fence = !fence
			continue
		}
		if fence || !reHeading.MatchString(line) {
			continue
		}
		l := reHashPrefix.ReplaceAllString(line, "")
		l = reFNum.ReplaceAllString(l, "")
		switch {
		case reBlocker.MatchString(l):
			c.Blockers++
		case reMajor.MatchString(l):
			c.Majors++
		case reMinor.MatchString(l):
			c.Minors++
		}
	}
	return c
}

// verdictDecoration is the class of markdown emphasis stripped before a VERDICT
// line is matched. Models variously emit the verdict bare, in `backticks`, in
// **bold**, or as a "### heading"; a formatting choice must never cost a whole
// report. Strip the class, do not chase one variant (each new variant that
// slipped through in bash was a new decoration character).
var verdictDecoration = strings.NewReplacer("`", "", "*", "", "_", "", "~", "", "#", "")

var (
	reVBlockers = regexp.MustCompile(`blockers=([0-9]+)`)
	reVMajors   = regexp.MustCompile(`majors=([0-9]+)`)
	reVMinors   = regexp.MustCompile(`minors=([0-9]+)`)
)

// parseVerdict returns the counts from the LAST well-formed
// "VERDICT <slug>: blockers=N majors=N minors=N" line, tolerant of markdown
// decoration. ok is false when no parseable line exists.
func parseVerdict(body, slug string) (counts Counts, ok bool) {
	reLine := regexp.MustCompile(`^VERDICT[[:space:]]+` + regexp.QuoteMeta(slug) + `[[:space:]]*:`)
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimLeft(verdictDecoration.Replace(raw), " \t")
		if !reLine.MatchString(line) {
			continue
		}
		bm := reVBlockers.FindStringSubmatch(line)
		mm := reVMajors.FindStringSubmatch(line)
		nm := reVMinors.FindStringSubmatch(line)
		if bm == nil || mm == nil || nm == nil {
			continue
		}
		counts = Counts{Blockers: atoi(bm[1]), Majors: atoi(mm[1]), Minors: atoi(nm[1])}
		ok = true
	}
	return counts, ok
}

// reVerdictLine matches a VERDICT line (after optional leading decoration) for
// the byte-floor computation, which excludes it from the counted body.
var reVerdictLine = regexp.MustCompile("^[`*_~#[:space:]]*VERDICT[[:space:]].*$")

// bodyBytes counts non-whitespace bytes across every line that is not the
// VERDICT line — the "evidence of work" measure for a clean report. Mirrors
// review.sh's `sed …/VERDICT/…// | tr -d '[:space:]' | wc -c`.
func bodyBytes(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if reVerdictLine.MatchString(line) {
			continue
		}
		for i := 0; i < len(line); i++ {
			if !isSpaceByte(line[i]) {
				n++
			}
		}
	}
	return n
}

// ValidateReport enforces the structural postcondition for one report and
// returns the rejection reason, or "" when the report is usable. The order and
// wording of the checks mirror review.sh so operators see the same diagnostics:
//
//  1. empty report — the model produced no output;
//  2. no parseable VERDICT line;
//  3. verdict/body mismatch — claimed counts != counted finding blocks;
//  4. a clean 0/0/0 verdict with a body below the evidence floor.
//
// A slug of "synthesis" applies the same postcondition to the decision queue.
func ValidateReport(slug, body string) string {
	if strings.TrimSpace(body) == "" {
		return "empty report — the model produced no output"
	}
	claimed, ok := parseVerdict(body, slug)
	if !ok {
		return fmt.Sprintf("no parseable 'VERDICT %s: blockers=N majors=N minors=N' line", slug)
	}
	found := countFindings(body)
	if claimed != found {
		return fmt.Sprintf("verdict/body mismatch — VERDICT claims (blockers majors minors)=(%s) "+
			"but the report contains (%s) '### [SEVERITY] …' finding blocks", claimed, found)
	}
	if claimed == (Counts{}) {
		if n := bodyBytes(body); n < cleanBodyFloor {
			return fmt.Sprintf("clean verdict with no evidence of work — %d bytes outside the VERDICT line. "+
				"A 0/0/0 report must still say what was swept and why it is clean", n)
		}
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}
