package research

import (
	"fmt"
	"strconv"
	"strings"
)

// TaskBriefs expands the requested fan-out into one brief per agent. --queries
// wins: a non-empty queries slice yields one agent per query (n is ignored).
// Otherwise the single brief is fanned into n angled copies — each carrying an
// "angle i of n" directive so the agents cover distinct ground rather than
// duplicating one another. n<=1 (and no queries) yields the brief unchanged.
func TaskBriefs(brief string, queries []string, n int) []string {
	if len(queries) > 0 {
		out := make([]string, len(queries))
		copy(out, queries)
		return out
	}
	if n <= 1 {
		return []string{brief}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("%s\n\n=== ANGLE %d of %d ===\nResearch this from a distinct angle (#%d). Cover ground the other angles are unlikely to, rather than restating shared basics.",
			brief, i+1, n, i+1)
	}
	return out
}

// TaskID is the findings filename stem for the i-th agent (zero-based) in a
// fan-out of total agents, e.g. "r01". Zero-padded to the width of total (min
// two digits) so a directory listing sorts in launch order at any fan-out size —
// "r009" before "r010" for a 15-agent run, not "r10" before "r9".
func TaskID(i, total int) string {
	width := len(strconv.Itoa(total))
	if width < 2 {
		width = 2
	}
	return fmt.Sprintf("r%0*d", width, i+1)
}

// SynthesisPrompt builds the prompt for the final synthesis agent: read every
// written findings file and write a merged synthesis to outPath. It is a plain
// instruction (no versioned wrapper) — the synthesis agent is still a headless,
// subscription-powered child, it just reads local files rather than the web.
func SynthesisPrompt(findingPaths []string, outPath string) string {
	var b strings.Builder
	b.WriteString("You are a research SYNTHESIS agent. Read every findings file listed below, ")
	b.WriteString("then write ONE merged synthesis to exactly this path (create parent dirs if needed):\n\n")
	fmt.Fprintf(&b, "    %s\n\n", outPath)
	b.WriteString("Do NOT modify the repository or any other file. In the synthesis: reconcile agreements ")
	b.WriteString("and contradictions across the findings, keep the citations, and lead with the ")
	b.WriteString("through-line a reader needs. Findings to synthesize:\n\n")
	for _, p := range findingPaths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}
