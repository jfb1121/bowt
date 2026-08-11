// Package output renders results as JSON (for agents) or as a human table (for
// a terminal). Agent-first: JSON is the default whenever stdout is not a TTY,
// so piping bowt into another program always yields machine-readable output.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// IsTTY reports whether stdout is an interactive terminal.
func IsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Emit prints v as indented JSON to stdout — the structured result of a command.
func Emit(v any) error { return EmitTo(os.Stdout, v) }

// EmitTo is Emit with an explicit writer — the seam that makes output testable
// ("accept an io.Writer" is the idiomatic Go way to keep I/O out of your logic).
func EmitTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Errf prints a bowt error to stderr in one consistent form.
func Errf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bowt: "+format+"\n", args...)
}
