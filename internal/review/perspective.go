package review

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// PerspectivesSubdir is the directory (inside the repo's config dir) holding the
// perspective prompt files.
const PerspectivesSubdir = "review-perspectives"

// Perspective is one review lens: a prompt file that carries `layers:`
// front-matter. Slug is the filename without .md; Path is absolute; Content is
// the file's text (injected into the assembled prompt).
type Perspective struct {
	Slug    string
	Path    string
	Content string
}

// reLayersFrontMatter identifies a perspective file: a `layers: [ … ]` line in
// the YAML front-matter. README.md and CONTRACT.md live in the same directory
// and must never run as a review (an earlier --all picked CONTRACT.md up), so
// the front-matter is the discriminator, not the file extension.
var reLayersFrontMatter = regexp.MustCompile(`(?m)^layers:[[:space:]]*\[`)

// Discover reads <perspectivesDir>/*.md and returns every file that is a
// perspective (carries `layers:` front-matter), sorted by slug. A missing
// directory is a hard error — a review with no perspectives can review nothing.
func Discover(perspectivesDir string) ([]Perspective, error) {
	if fi, err := os.Stat(perspectivesDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("no perspectives directory at %s", perspectivesDir)
	}
	matches, err := filepath.Glob(filepath.Join(perspectivesDir, "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var ps []Perspective
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("read perspective %s: %w", m, err)
		}
		if !reLayersFrontMatter.Match(b) {
			continue
		}
		ps = append(ps, Perspective{
			Slug:    strings.TrimSuffix(filepath.Base(m), ".md"),
			Path:    m,
			Content: string(b),
		})
	}
	return ps, nil
}

// Select resolves which perspectives run. An explicit slug list selects exactly
// those (erroring on any slug with no matching perspective file); otherwise all
// discovered perspectives are selected.
//
// Layer auto-routing by changed paths — matching a diff's touched files against
// each perspective's `layers:` — is repo-specific (e.g. hard-coding a
// framework's directory layout). It is DEFERRED: bowt defaults to every
// perspective, and a per-repo layer classifier is a later hook. See STATUS.md.
func Select(all []Perspective, explicit []string) ([]Perspective, error) {
	if len(explicit) == 0 {
		return all, nil
	}
	byslug := make(map[string]Perspective, len(all))
	for _, p := range all {
		byslug[p.Slug] = p
	}
	// De-dup while preserving the caller's order.
	seen := map[string]bool{}
	var out []Perspective
	for _, s := range explicit {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		p, ok := byslug[s]
		if !ok {
			return nil, fmt.Errorf("no perspective %q in %s (have: %s)", s, PerspectivesSubdir, availableSlugs(all))
		}
		out = append(out, p)
	}
	return out, nil
}

func availableSlugs(all []Perspective) string {
	slugs := make([]string, len(all))
	for i, p := range all {
		slugs[i] = p.Slug
	}
	return strings.Join(slugs, ", ")
}
