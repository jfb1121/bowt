// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
//
// The CLI is built on cobra so every command gets real --help, usage, and
// shell completion (see `bowt completion`) — but the command logic still lives
// in the cmd* functions below, called from each command's RunE.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/extension"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/state"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	// Discover user drop-in providers (~/.bowt/agents/*.json) once at startup,
	// AFTER package init has registered the built-ins, so a colliding drop-in is
	// skipped (built-ins win) and provider selection sees the full set. Mirrors
	// how the extension loader is wired from main; a missing dir is a no-op.
	agent.LoadDropins(output.Errf)

	root := newRootCmd()
	// Git-style dispatch: if the first word is not a built-in command but the
	// repo ships a matching extension, run it and exit with its code. Built-ins
	// always win; an unknown word with no extension falls through to cobra's
	// normal unknown-command error below.
	if code, handled := tryExtension(root, os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := root.Execute(); err != nil {
		// Preserve bowt's error contract: "bowt: <err>" on stderr, exit 1.
		// Usage/errors are silenced on the commands so cobra doesn't also print
		// its own "Error:" line or dump usage on a runtime failure.
		output.Errf("%v", err)
		os.Exit(1)
	}
}

// newRootCmd builds the full command tree. It's a constructor (not a package
// var) so tests can build a fresh, isolated tree per case.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bowt",
		Short: "git worktrees with isolated environments",
		Long: `bowt — git worktrees with isolated environments.

Every branch gets its own worktree with an allocated port/offset. Commands emit
JSON by default whenever stdout is not a terminal, so agents parse structured
output instead of scraping tables.`,
		Version: version,
		// Own our error/usage output (see main): print "bowt: <err>" ourselves.
		SilenceUsage:  true,
		SilenceErrors: true,
		// No bash-completion command noise for a private tool; we ship our own.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	// `bowt --version` prints just the version string (matches the old behavior),
	// not cobra's default "bowt version X.Y".
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newNewCmd(),
		newLsCmd(),
		newPathCmd(),
		newRmCmd(),
		newExecCmd(),
		newSpawnCmd(),
		newLaneCmd(),
		newLanesCmd(),
		newStatusCmd(),
		newLaneRunCmd(),
		newGateCmd(),
		newLandCmd(),
		newReviewCmd(),
		newResearchCmd(),
		newDoctorCmd(),
		newRootPathCmd(),
		newCdCmd(),
		newShellInitCmd(),
		newExtensionsCmd(),
		newLibCmd(),
		newVersionCmd(),
		newCompletionCmd(),
	)
	return root
}

func newDoctorCmd() *cobra.Command {
	var agentName string
	var dryRun bool
	c := &cobra.Command{
		Use:   "doctor --agent <name>",
		Short: "smoke-check an agent provider",
		Long: `Smoke-check a provider descriptor: its binary is on PATH, its config dir is
resolvable, and — when the provider supports one-shot — a trivial prompt
round-trips through it. Emits a machine-readable report; exits non-zero if any
check fails.

(Only the --agent path exists in this slice; a fuller doctor covering deps,
ports, and the registry is a later slice.)`,
		Example: `  bowt doctor --agent claude
  bowt doctor --agent codex
  bowt doctor --agent claude --dry-run   # skip the one-shot launch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agentName == "" {
				return fmt.Errorf("doctor currently supports only --agent <name>")
			}
			ag, err := agent.New(agentName)
			if err != nil {
				return err
			}
			rep := doctorAgent(ag, exec.LookPath, os.Getenv("HOME"), dryRun)
			if emitErr := output.Emit(rep); emitErr != nil {
				return emitErr
			}
			if !rep.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent provider to smoke-check (claude, codex)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "skip the one-shot round-trip (check bin + config only)")
	return c
}

func newRootPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "root",
		Short:   "print the main repo path",
		Long:    "Print the absolute path of the main repository root (works from inside a worktree).",
		Example: "  bowt root",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdRoot()
		},
	}
}

func newCdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cd <branch>|main",
		Short: "change directory into a worktree (needs shell integration)",
		Long: `Change the caller's directory into a worktree.

Changing the parent shell's cwd is the one thing a child process cannot do, so
cd requires the shell integration:

  eval "$(bowt shell-init zsh)"`,
		Example:           "  bowt cd feature/login",
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf(`cd needs the shell integration — run: eval "$(bowt shell-init zsh)"`)
		},
	}
}

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init [bash|zsh]",
		Short: "print shell integration to eval",
		Long: `Print the shell integration snippet to eval, e.g.:

  eval "$(bowt shell-init zsh)"

This installs a bowt() wrapper so 'bowt cd' can change your shell's directory,
and loads bowt's shell completion for the chosen shell.`,
		Example: `  eval "$(bowt shell-init zsh)"
  eval "$(bowt shell-init bash)"`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"bash", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdShellInit(args)
		},
	}
}

func newExtensionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extensions",
		Short: "list the repo's per-repo extensions",
		Long: `List the extensions this repo ships under <configDir>/extensions/*.sh, with
each one's description and lock mode.

An extension is a bash script the repo drops in to add a 'bowt <cmd>' without
patching bowt. When <cmd> is not a built-in and a matching script exists, bowt
runs it as a subprocess with the per-worktree BOWT_*/GWT_* environment injected
and BOWT_LIB pointing at the helper (see 'bowt lib').

A leading comment header configures it:
  # bowt-lock: none|shared|exclusive   (bowt takes the lock around the run)
  # bowt-desc: <one-line description>`,
		Example: `  bowt extensions
  bowt extensions --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdExtensions()
		},
	}
}

func newLibCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lib",
		Short: "print the sourceable bash helper for extensions",
		Long: `Print bowt.lib.sh, the bash helper an extension sources via "$BOWT_LIB". bowt
exports BOWT_LIB into every extension pointing at this content, so a script can:

  source "$BOWT_LIB"
  bowt_log "starting"

It defines bowt_log / bowt_err (both to stderr). The BOWT_*/GWT_* environment is
already exported into the script, so the helper stays thin.`,
		Example: "  bowt lib",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// WriteString (not fmt.Print) since Lib legitimately contains %s.
			_, err := os.Stdout.WriteString(extension.Lib)
			return err
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "print build version",
		Example: "  bowt version",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(version)
			return nil
		},
	}
}

func newCompletionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "generate a shell completion script",
		Long: `Generate a shell completion script for bowt.

Usually you don't run this directly — 'bowt shell-init' sources it for you. To
load it manually for the current session:

  source <(bowt completion zsh)     # zsh
  source <(bowt completion bash)    # bash`,
		Example: `  bowt completion zsh
  source <(bowt completion bash)`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("unsupported shell %q (want bash, zsh, fish, or powershell)", args[0])
			}
		},
	}
	return c
}

// orDash renders an empty scalar as "-" in a human table.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// newLaneID mints a stable, filesystem-safe lane id: a slug of the ticket (when
// given) plus a short random suffix, so it can name the log/spec files and be
// reported before the fork races. Random bytes come from crypto/rand.
func newLaneID(ticket string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lane id: %w", err)
	}
	suffix := hex.EncodeToString(b[:])
	slug := slugify(ticket)
	if slug == "" {
		return "lane-" + suffix, nil
	}
	return slug + "-" + suffix, nil
}

// slugify lowercases s and collapses any run of non-alphanumeric bytes to a
// single dash, trimming leading/trailing dashes — a filesystem-safe stem. A
// pending dash is only emitted once a real char follows, so leading and
// trailing separators never survive.
func slugify(s string) string {
	var out []rune
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && len(out) > 0 {
				out = append(out, '-')
			}
			out = append(out, r)
			dash = false
		} else {
			dash = true
		}
	}
	return string(out)
}

// orDefault labels an empty model/effort as the agent's session default for the
// human-facing header.
func orDefault(s string) string {
	if s == "" {
		return "session-default"
	}
	return s
}

// doctorCheck is one smoke-check outcome in the agent report.
type doctorCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// doctorReport is the agent-facing result of `bowt doctor --agent`.
type doctorReport struct {
	Agent  string        `json:"agent"`
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// doctorAgent runs the provider smoke-checks. lookPath and home are injected so
// tests exercise it without a real PATH or HOME; dryRun skips the one-shot
// launch so the check is safe to run without launching the CLI. When the
// provider has no one-shot mode, that step is a pass (nothing to check), not a
// failure — SupportsOneshot=false is a legitimate provider shape.
func doctorAgent(ag agent.Agent, lookPath func(string) (string, error), home string, dryRun bool) doctorReport {
	caps := ag.Caps()
	rep := doctorReport{Agent: caps.Name, OK: true}
	add := func(name string, pass bool, detail string) {
		rep.Checks = append(rep.Checks, doctorCheck{Name: name, Pass: pass, Detail: detail})
		if !pass {
			rep.OK = false
		}
	}

	// schema (RFC §11.1) — FIRST check: statically validate the descriptor
	// (known schemaVersion, required fields, effort keys ∈ enum, legal prompt
	// deliveries, at-most-one-{value} render lists) so a malformed or
	// lying-at-the-value-level drop-in fails here, not at run time. A failing
	// schema check sets rep.OK=false, and newDoctorCmd's os.Exit(1) then fires.
	// A non-descriptor provider (future-proofing) is skipped, not failed.
	if problems, checked := agent.Validate(ag); !checked {
		add("schema", true, "not a descriptor-backed provider (skipped)")
	} else if len(problems) == 0 {
		add("schema", true, "descriptor valid")
	} else {
		add("schema", false, strings.Join(problems, "; "))
	}

	// origin/provenance (informational, never fails): whether this provider is a
	// trusted embedded built-in or a user drop-in, and for a drop-in the file to
	// edit — the debugging context a doctor run on one's own descriptor wants.
	if builtin, path, ok := agent.Provenance(ag); ok {
		if builtin {
			add("origin", true, "built-in (embedded)")
		} else {
			add("origin", true, "drop-in: "+path)
		}
	}

	// bin on PATH
	if p, err := lookPath(caps.Bin); err == nil {
		add("bin-on-path", true, p)
	} else {
		add("bin-on-path", false, fmt.Sprintf("%s not found on PATH", caps.Bin))
	}

	// config dir resolvable (path computable from a known HOME; existence is a
	// detail, not a failure — a fresh machine may not have run the agent yet).
	switch {
	case caps.ConfigDir == "":
		add("config-dir", false, "provider declares no config dir")
	case home == "":
		add("config-dir", false, "HOME unset; cannot resolve "+caps.ConfigDir)
	default:
		dir := filepath.Join(home, caps.ConfigDir)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			add("config-dir", true, dir)
		} else {
			add("config-dir", true, dir+" (absent)")
		}
	}

	// one-shot round-trip (only when supported and not dry-run)
	switch {
	case !caps.SupportsOneshot:
		add("oneshot", true, "not supported by this provider (skipped)")
	case dryRun:
		add("oneshot", true, "skipped (--dry-run)")
	default:
		out, err := ag.Oneshot(context.Background(), "Reply with the single word: ok")
		if err != nil {
			add("oneshot", false, err.Error())
		} else {
			add("oneshot", true, fmt.Sprintf("round-trip ok (%d bytes)", len(strings.TrimSpace(out))))
		}
	}
	return rep
}

func cmdRoot() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	fmt.Println(main)
	return nil
}

// tryExtension implements the git-style dispatch fallback. It returns
// handled=true only when args name a per-repo extension that bowt ran (code is
// then the extension's exit code); handled=false means "not an extension — let
// cobra handle it" (a built-in, a flag, or an unknown command with no matching
// extension, which becomes cobra's normal error).
func tryExtension(root *cobra.Command, args []string) (code int, handled bool) {
	// No word, or a flag (e.g. --help/--version): cobra's job, never an extension.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	// Let cobra resolve the word first: if it maps to any real command (built-in,
	// help, __complete, …), Find returns that command and built-ins always win —
	// an extension can never shadow a built-in. When the word is unknown, Find
	// returns the root command WITH an "unknown command" error; that error is
	// exactly our signal to try an extension, so we key only on cmd != root.
	cmd, _, _ := root.Find(args)
	if cmd != root {
		return 0, false
	}
	// args[0] is unknown to cobra. Only inside a repo with a config dir can an
	// extension exist; otherwise fall through to cobra's unknown-command error.
	main, err := repo.MainRepo()
	if err != nil {
		return 0, false
	}
	ext, found, err := extension.Find(config.Dir(main), args[0])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	if !found {
		return 0, false
	}
	code, err = cmdExtension(main, ext, args[1:])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	return code, true
}

// cmdExtension runs a resolved extension against the worktree the caller stands
// in: cwd = that worktree, the per-worktree BOWT_*/GWT_* + config env injected,
// and the manifest's lock taken around the run so the author writes no lock
// code. It returns the child's exit code; err covers only setup/lock failures.
func cmdExtension(main string, ext extension.Extension, args []string) (int, error) {
	top, err := repo.Toplevel("")
	if err != nil {
		return 1, err
	}
	name := filepath.Base(main)
	branch := repo.CurrentBranch(top)

	// Per-worktree env — the same environment exec/gate build. A broken config is
	// a warning, not a hard failure: the extension stays runnable.
	r := run.Exec{Stderr: os.Stderr}
	vars, err := config.Load(r, config.Dir(main))
	if err != nil {
		output.Errf("load config: %v — running %q without config env", err, ext.Cmd)
		vars = nil
	}
	info := env.Info{Path: top, Branch: branch, MainRepo: main, RepoName: name}
	if st, err := state.Open(); err == nil {
		if wt, ok, gerr := st.Get(name, branch); gerr == nil && ok {
			info.Offset, info.Port, info.CodeOnly = wt.Offset, wt.Port, wt.CodeOnly()
		}
	}
	envKV := env.Build(info, vars)

	// The manifest owns locking. Key on the worktree path (as gate/spawn/review
	// do) and fail fast with the busy message if another bowt process holds it.
	// The lock is released by defer before this returns, so main's os.Exit with
	// the propagated code never leaks it (the kernel also frees flock on exit).
	switch ext.Manifest.Lock {
	case extension.LockExclusive:
		l, lerr := lock.AcquireReentrant(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	case extension.LockShared:
		l, lerr := lock.AcquireSharedReentrant(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	}

	return extension.Run(ext, top, envKV, args, os.Stdin, os.Stdout, os.Stderr)
}

// extInfo is the agent-facing shape for one extension in `bowt extensions`.
type extInfo struct {
	Cmd  string `json:"cmd"`
	Desc string `json:"desc"`
	Lock string `json:"lock"`
}

func cmdExtensions() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	exts, err := extension.List(config.Dir(main))
	if err != nil {
		return err
	}
	infos := make([]extInfo, 0, len(exts))
	for _, e := range exts {
		lockMode := e.Manifest.Lock
		if lockMode == "" {
			lockMode = extension.LockNone
		}
		infos = append(infos, extInfo{Cmd: e.Cmd, Desc: e.Manifest.Desc, Lock: string(lockMode)})
	}
	// Agent-first: JSON unless a human is at the terminal.
	if !output.IsTTY() {
		return output.Emit(infos)
	}
	if len(infos) == 0 {
		fmt.Println("no extensions")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMAND\tLOCK\tDESCRIPTION")
	for _, e := range infos {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Cmd, e.Lock, e.Desc)
	}
	return w.Flush()
}

func cmdShellInit(args []string) error {
	sh := "zsh"
	if len(args) > 0 {
		sh = args[0]
	}
	out, err := shell.Init(sh)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
