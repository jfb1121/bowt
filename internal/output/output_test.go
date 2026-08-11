package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmitTo(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitTo(&buf, map[string]int{"port": 8001}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"port": 8001`) {
		t.Fatalf("EmitTo output missing port field:\n%s", got)
	}
}
