package bash

import (
	"strings"
	"testing"
)

func TestRunCapturesOutput(t *testing.T) {
	p := New(Config{})
	res, _ := p.run(t.Context(), Run("echo hello", 5))
	if res.OK == false || res.Output != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("run: %+v", res)
	}
}

func TestExitCodeIsObservation(t *testing.T) {
	p := New(Config{})
	res, _ := p.run(t.Context(), Run("false", 5))
	if res.OK == false || res.ExitCode != 1 {
		t.Fatalf("non-zero exit should be an observation: %+v", res)
	}
}

func TestTimeout(t *testing.T) {
	p := New(Config{})
	res, _ := p.run(t.Context(), Run("sleep 5", 1))
	if res.OK || !strings.Contains(res.Error, "timed out") {
		t.Fatalf("timeout: %+v", res)
	}
}

func TestTailTruncationWithFullOutput(t *testing.T) {
	p := New(Config{MaxLines: 5})
	cmd := "for i in 1 2 3 4 5 6 7 8 9 10; do echo line$i; done"
	res, _ := p.run(t.Context(), Run(cmd, 10))
	if !strings.Contains(res.Output, "earlier lines truncated") {
		t.Fatalf("missing truncation note:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "line10") || strings.Contains(res.Output, "line1\n") {
		t.Fatalf("tail should keep the LAST lines:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "full output: ") {
		t.Fatalf("full output path missing:\n%s", res.Output)
	}
}
