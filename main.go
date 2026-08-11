// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	st, err := state.Open()
	if err != nil {
		output.Errf("%v", err)
		os.Exit(1)
	}

	switch cmd {
	case "new":
		err = cmdNew(st, args)
	case "ls":
		err = cmdLs(st, args)
	case "path":
		err = cmdPath(st, args)
	case "rm":
		err = cmdRm(st, args)
	case "exec":
		err = cmdExec(st, args)
	case "root":
		err = cmdRoot()
	case "shell-init":
		err = cmdShellInit(args)
	case "cd":
		err = fmt.Errorf(`cd needs the shell integration — run: eval "$(bowt shell-init zsh)"`)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		output.Errf("unknown command: %s", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		output.Errf("%v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `bowt — git worktrees with isolated environments

usage:
  bowt new <branch> [-b base]   create a worktree, register it
  bowt ls [--json]              list worktrees (JSON unless a terminal)
  bowt path <branch>            print a worktree's path
  bowt rm <branch>              remove + deregister a worktree
  bowt exec <branch> [--] cmd   run a command inside a worktree
  bowt cd <branch>|main         change directory (needs shell integration)
  bowt root                     print the main repo path
  bowt shell-init [bash|zsh]    print shell integration to eval
  bowt version                  print build version
`)
}

func cmdNew(st state.Store, args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	base := fs.String("b", "", "base branch (default: main repo's current branch)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: bowt new <branch> [-b base]")
	}
	branch := fs.Arg(0)

	// Writers take the per-worktree lock so two bowt processes can't race the
	// same tree. defer releases it as soon as this command returns.
	l, err := lock.Acquire(branch)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	// Hooks stream their stderr live so a slow setup.sh shows progress;
	// stdout stays reserved for bowt's JSON result.
	r := run.Exec{Stderr: os.Stderr}
	wt, err := worktree.New(st, r, branch, *base)
	if err != nil {
		return err
	}
	return output.Emit(wt)
}

func cmdLs(st state.Store, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "force JSON output")
	_ = fs.Parse(args)

	wts, err := worktree.List(st)
	if err != nil {
		return err
	}
	// A nil slice marshals to JSON `null`; agents expect an array. Coerce to [].
	if wts == nil {
		wts = []state.Worktree{}
	}
	// Agent-first: JSON unless a human is at the terminal (or --json forces it).
	if *asJSON || !output.IsTTY() {
		return output.Emit(wts)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BRANCH\tPORT\tOFFSET\tPATH")
	for _, wt := range wts {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", wt.Branch, wt.Port, wt.Offset, wt.Path)
	}
	return w.Flush()
}

func cmdPath(st state.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bowt path <branch>")
	}
	p, err := worktree.Path(st, args[0])
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

func cmdRm(st state.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bowt rm <branch>")
	}
	branch := args[0]

	l, err := lock.Acquire(branch)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	r := run.Exec{Stderr: os.Stderr}
	if err := worktree.Remove(st, r, branch); err != nil {
		return err
	}
	// A declared shape (even anonymous) beats an ad-hoc map for agent-facing JSON.
	return output.Emit(struct {
		Removed string `json:"removed"`
	}{Removed: branch})
}

func cmdExec(st state.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bowt exec <branch> [--] <cmd> [args...]")
	}
	branch, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: bowt exec <branch> [--] <cmd> [args...]")
	}

	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	name := filepath.Base(main)
	wt, ok, err := st.Get(name, branch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no worktree registered for %q", branch)
	}

	// Inject the per-worktree env (BOWT_*/GWT_* + config vars) so the command
	// sees the same environment as the hooks. A broken config is a warning, not
	// a hard failure — exec stays usable.
	vars, err := config.Load(run.Exec{Stderr: os.Stderr}, config.Dir(main))
	if err != nil {
		output.Errf("load config: %v — running without config env", err)
		vars = nil
	}
	envKV := env.Build(env.Info{
		Path:     wt.Path,
		Branch:   branch,
		Offset:   wt.Offset,
		Port:     wt.Port,
		MainRepo: main,
		RepoName: name,
	}, vars)

	c := exec.Command(rest[0], rest[1:]...)
	c.Dir = wt.Path
	c.Env = append(os.Environ(), envKV...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		// Propagate the child's exit code rather than masking it as our own.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func cmdRoot() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	fmt.Println(main)
	return nil
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
