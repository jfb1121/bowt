// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
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

	wt, err := worktree.New(st, branch, *base)
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

	if err := worktree.Remove(st, branch); err != nil {
		return err
	}
	// A declared shape (even anonymous) beats an ad-hoc map for agent-facing JSON.
	return output.Emit(struct {
		Removed string `json:"removed"`
	}{Removed: branch})
}
