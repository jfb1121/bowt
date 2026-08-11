package repo

import (
	"regexp"
	"sort"
	"strings"
)

// DiffFacts is the diff scope and preflight facts for a review: the base the
// diff is computed against and the mechanical truths a reviewer must not miss.
// The command layer maps this onto the review package's Preflight (repo stays
// review-agnostic).
type DiffFacts struct {
	// MergeBase is merge-base(base, HEAD): the diff runs from here to the working
	// tree, so local commits AND uncommitted changes are covered.
	MergeBase string
	// DiffCmd is the exact command a reviewer is told to run.
	DiffCmd string
	// ChangedFiles are the paths the diff touches.
	ChangedFiles []string
	// BinaryAdded are binary files the diff adds (numstat "- -" rows).
	BinaryAdded []string
	// NewFiles is the count of files the diff adds.
	NewFiles int
	// AddedSymbols are top-level symbols the diff adds (for a reuse check).
	AddedSymbols []string
	// UntrackedSource are source files present but never `git add`ed — invisible
	// to a diff review.
	UntrackedSource []string
}

// sourceExts are the extensions treated as "source" for the untracked-files
// check. Untracked source is a review hazard: code written but never added
// ships without its coverage and the diff review says nothing.
var sourceExts = []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".java", ".rb"}

// reAddedSymbol captures a top-level declaration on an added (+) diff line,
// across the languages bowt reviews. It is deliberately broad — a reviewer uses
// the list as reuse-check hunt items, so a false positive costs nothing.
var reAddedSymbol = regexp.MustCompile(`^\+[[:space:]]*(?:func|type|class|def|const|var)[[:space:]]+([A-Za-z_][A-Za-z0-9_]*)`)

// ReviewScope resolves the in-place review diff scope and preflight facts for
// the worktree at dir against base. It best-effort fetches base first (offline
// is fine), computes merge-base(base, HEAD), then derives the changed-file list
// and preflight facts from merge-base → working tree.
func ReviewScope(dir, base string) (DiffFacts, error) {
	// Best-effort fetch so an origin/* base reflects the remote tip; offline or a
	// non-remote base is not fatal.
	if remote, ref, ok := splitRemoteRef(base); ok {
		_, _ = run(dir, "fetch", remote, ref, "--quiet")
	}

	mb, err := run(dir, "merge-base", base, "HEAD")
	if err != nil {
		return DiffFacts{}, err
	}

	f := DiffFacts{
		MergeBase: mb,
		DiffCmd:   "git diff " + mb,
	}
	f.ChangedFiles = nonEmptyLines(mustRun(dir, "diff", "--name-only", mb))

	// Binary files added: numstat rows where both added and deleted counts are "-".
	for _, line := range nonEmptyLines(mustRun(dir, "diff", "--numstat", mb)) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "-" && fields[1] == "-" {
			f.BinaryAdded = append(f.BinaryAdded, fields[2])
		}
	}

	f.NewFiles = len(nonEmptyLines(mustRun(dir, "diff", "--diff-filter=A", "--name-only", mb)))

	// Added top-level symbols, de-duplicated and sorted.
	symSet := map[string]bool{}
	for _, line := range strings.Split(mustRun(dir, "diff", mb), "\n") {
		if m := reAddedSymbol.FindStringSubmatch(line); m != nil {
			symSet[m[1]] = true
		}
	}
	for s := range symSet {
		f.AddedSymbols = append(f.AddedSymbols, s)
	}
	sort.Strings(f.AddedSymbols)

	// Untracked source files (not in the diff at all).
	for _, path := range nonEmptyLines(mustRun(dir, "ls-files", "--others", "--exclude-standard")) {
		if isSource(path) {
			f.UntrackedSource = append(f.UntrackedSource, path)
		}
	}

	return f, nil
}

// mustRun runs a read-only git command and returns its stdout, swallowing errors
// as empty output: a preflight fact that cannot be computed is simply absent
// (matching review.sh's `2>/dev/null || true`), never a hard failure of the run.
func mustRun(dir string, args ...string) string {
	out, _ := run(dir, args...)
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func isSource(path string) bool {
	for _, ext := range sourceExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// splitRemoteRef splits an "origin/main" style base into ("origin", "main") for
// a fetch. A bare ref (no slash) or a SHA yields ok=false — nothing to fetch.
func splitRemoteRef(base string) (remote, ref string, ok bool) {
	i := strings.IndexByte(base, '/')
	if i <= 0 || i == len(base)-1 {
		return "", "", false
	}
	return base[:i], base[i+1:], true
}

// UpstreamOrDefault returns the diff base for an in-place review: the branch's
// tracked upstream (@{upstream}) if set, else "origin/main". The command layer
// passes an explicit --base ahead of this.
func UpstreamOrDefault(dir string) string {
	if up, err := run(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil && up != "" {
		return up
	}
	return "origin/main"
}
