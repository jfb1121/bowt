package run

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecCapturesStdout(t *testing.T) {
	out, err := Exec{}.Run("", "printf", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "hello" {
		t.Fatalf("stdout = %q; want %q", out, "hello")
	}
}

func TestExecStderrStreamsWhenSet(t *testing.T) {
	var buf bytes.Buffer
	_, err := Exec{Stderr: &buf}.Run("", "bash", "-c", "echo oops 1>&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "oops") {
		t.Fatalf("stderr not streamed: %q", buf.String())
	}
}

func TestExecFoldsStderrIntoErrorWhenUnset(t *testing.T) {
	_, err := Exec{}.Run("", "bash", "-c", "echo boom 1>&2; exit 2")
	if err == nil {
		t.Fatal("want error from exit 2")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should include captured stderr: %v", err)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := &Fake{Func: func(_, _ string, _ []string) (string, error) { return "ok", nil }}
	out, err := f.Run("/dir", "git", "status", "-s")
	if err != nil || out != "ok" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Dir != "/dir" || c.Name != "git" || strings.Join(c.Args, " ") != "status -s" {
		t.Fatalf("recorded call wrong: %+v", c)
	}
}
