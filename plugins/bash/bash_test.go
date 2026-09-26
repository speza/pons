package bash

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samperrin/pons/protocol"
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
	for _, test := range []struct {
		name           string
		callerCanceled bool
	}{
		{name: "tool timeout"},
		{name: "caller cancellation", callerCanceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			wantError := "timed out"
			if test.callerCanceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				wantError = "canceled"
			}
			started := time.Now()
			res, _ := New(Config{}).run(ctx, Run("sleep 3 & wait", 1))
			if res.OK || !strings.Contains(res.Error, wantError) {
				t.Fatalf("timeout: %+v", res)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("descendant held the output pipe beyond cancellation: %s", elapsed)
			}
		})
	}
}

func TestLargeOutputRetainsBoundedTail(t *testing.T) {
	for _, test := range []struct {
		name         string
		command      string
		droppedLines int
	}{
		{name: "long line", command: "head -c 16777216 /dev/zero; printf tail-marker"},
		{name: "many lines", command: `head -c 16777216 /dev/zero | tr '\000' '\012'; printf tail-marker`, droppedLines: 16777212},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			res, _ := New(Config{MaxLines: 5, MaxBytes: 128}).run(t.Context(), Run(test.command, 10))
			runtime.ReadMemStats(&after)
			if !res.OK || !strings.HasSuffix(res.Output, "tail-marker") {
				t.Fatalf("large command output lost its tail: %+v", res)
			}
			extras, ok := AsExecResult(res)
			if !ok || !extras.Truncated || extras.FullOutput == "" || extras.DroppedLines != test.droppedLines {
				t.Fatalf("incorrect spill metadata: %+v", extras)
			}
			cleanupSpill(t, res)
			info, err := os.Stat(extras.FullOutput)
			if err != nil || info.Size() > maxFullFileBytes {
				t.Fatalf("spill exceeds its bound: %v, %v", info, err)
			}
			// Allow ample overhead above the 8 MiB spill tail. This rejects retaining
			// and repeatedly copying the complete command output before truncation.
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
				t.Fatalf("bounded command output allocated %d bytes", allocated)
			}
		})
	}
}

func TestTailTruncationWithFullOutput(t *testing.T) {
	p := New(Config{MaxLines: 5})
	cmd := "for i in 1 2 3 4 5 6 7 8 9 10; do echo line$i; done"
	res, _ := p.run(t.Context(), Run(cmd, 10))
	cleanupSpill(t, res)
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

func cleanupSpill(t *testing.T, result protocol.ToolResult) {
	t.Helper()
	if extras, ok := AsExecResult(result); ok && extras.FullOutput != "" {
		t.Cleanup(func() { _ = os.Remove(extras.FullOutput) })
	}
}

func TestByteTruncationKeepsValidUTF8(t *testing.T) {
	// 100 two-byte runes, no trailing newline, byte cut lands mid-rune.
	s := strings.Repeat("é", 100)
	res, _ := New(Config{MaxLines: 1000, MaxBytes: 15}).run(t.Context(), Run("printf '"+s+"'", 5))
	cleanupSpill(t, res)
	if !res.OK || !utf8.ValidString(res.Output) {
		t.Fatalf("byte truncation produced invalid UTF-8: %+v", res)
	}
}

func TestByteOnlyTruncationReportsBytesNotZeroLines(t *testing.T) {
	// One huge line: no line boundary in range, so zero lines drop but
	// bytes are still cut. The note must not claim "0 earlier lines".
	s := strings.Repeat("x", 1000)
	res, _ := New(Config{MaxLines: 1000, MaxBytes: 15}).run(t.Context(), Run("printf '"+s+"'", 5))
	cleanupSpill(t, res)
	extras, ok := AsExecResult(res)
	if !res.OK || !ok || !extras.Truncated {
		t.Fatalf("expected truncation: %+v", extras)
	}
	if strings.Contains(res.Output, "0 earlier lines") {
		t.Fatalf("misleading zero-lines note:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "earlier bytes truncated") {
		t.Fatalf("expected bytes note:\n%s", res.Output)
	}
}
