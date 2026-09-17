package bash

import (
	"strings"
	"testing"
	"unicode/utf8"
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
	if !strings.Contains(res.Output, "line10") || !strings.Contains(res.Output, "line6\n") || strings.Contains(res.Output, "line5\n") {
		t.Fatalf("tail should keep the LAST lines:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "full output: ") {
		t.Fatalf("full output path missing:\n%s", res.Output)
	}
}

func TestByteTruncationKeepsValidUTF8(t *testing.T) {
	// 100 two-byte runes, no trailing newline, byte cut lands mid-rune.
	s := strings.Repeat("é", 100)
	out, _ := truncateOutput(s, 1000, 15)
	if !utf8.ValidString(out) {
		t.Fatalf("byte truncation produced invalid UTF-8: %q", out)
	}
}

func TestByteOnlyTruncationReportsBytesNotZeroLines(t *testing.T) {
	// One huge line: no line boundary in range, so zero lines drop but
	// bytes are still cut. The note must not claim "0 earlier lines".
	s := strings.Repeat("x", 1000)
	out, extras := truncateOutput(s, 1000, 15)
	if extras == nil || !extras.Truncated {
		t.Fatalf("expected truncation: %+v", extras)
	}
	if strings.Contains(out, "0 earlier lines") {
		t.Fatalf("misleading zero-lines note:\n%s", out)
	}
	if !strings.Contains(out, "earlier bytes truncated") {
		t.Fatalf("expected bytes note:\n%s", out)
	}
}

func TestCaptureCapBoundsRunawayOutput(t *testing.T) {
	// ~4KB of output with a ~1KB capture window: memory and the spilled
	// file stay bounded, the tail survives, and the cap is disclosed.
	p := New(Config{MaxLines: 10000, MaxBytes: 100, MaxCaptureBytes: 200})
	res, _ := p.run(t.Context(), Run("seq 1 500", 10))
	if !strings.Contains(res.Output, "capture cap") {
		t.Fatalf("missing capture-cap note:\n%s", res.Output)
	}
	extras, ok := AsExecResult(res)
	if !ok || !extras.Truncated || extras.FullOutput == "" {
		t.Fatalf("missing truncation payload: %+v", res)
	}
	if !strings.Contains(res.Output, "500\n") {
		t.Fatalf("tail should survive the capture window:\n%s", res.Output)
	}
}
