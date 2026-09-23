// Package unidiff generates standard unified diffs from two file contents.
// Dependency-free line-level diff (LCS), git-style headers — used by the
// edit tool to return a reviewable patch (pi's edit tool returns
// details.patch for the same reason).
package unidiff

import (
	"fmt"
	"strings"
)

const maxDPSize = 4_000_000 // LCS cells; beyond this fall back to a coarse diff

type op struct {
	kind byte   // ' ', '-', '+'
	line string // content without trailing newline
}

// Unified returns a unified diff between a and b for filename, with the
// given number of context lines (git default: 3). Empty string when equal.
func Unified(filename string, a, b string, context int) string {
	if context < 0 {
		context = 3
	}
	if a == b {
		return ""
	}
	aLines, aNL := splitLines(a)
	bLines, bNL := splitLines(b)

	var ops []op
	if len(aLines)*len(bLines) <= maxDPSize {
		// Compare with newline-state-aware keys so EOF newline differences
		// surface as real changes (git semantics).
		ops = diff(keys(aLines, aNL), keys(bLines, bNL))
	} else {
		ops = coarse(aLines, bLines) // too large for exact LCS
	}
	return render(filename, ops, context, len(aLines), aNL, len(bLines), bNL)
}

func splitLines(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	trailing := strings.HasSuffix(s, "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return lines, trailing
}

// keys renders comparison keys where the final line of a file WITHOUT a
// trailing newline differs from the same line WITH one — so "a" vs "a\n"
// diffs like git does, and the no-newline marker is emitted on the right op.
func keys(lines []string, trailing bool) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if i == len(lines)-1 && !trailing {
			out[i] = l
		} else {
			out[i] = l + "\n"
		}
	}
	return out
}

// diff computes the edit script via LCS dynamic programming. a and b are
// newline-state-aware line keys (see keys).
func diff(a, b []string) []op {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	strip := func(s string) string { return strings.TrimSuffix(s, "\n") }
	ops := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{kind: ' ', line: strip(a[i])})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{kind: '-', line: strip(a[i])})
			i++
		default:
			ops = append(ops, op{kind: '+', line: strip(b[j])})
			j++
		}
	}

	for ; i < n; i++ {
		ops = append(ops, op{kind: '-', line: strip(a[i])})
	}
	for ; j < m; j++ {
		ops = append(ops, op{kind: '+', line: strip(b[j])})
	}
	return ops
}

func coarse(a, b []string) []op {
	ops := make([]op, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, op{kind: '-', line: l})
	}
	for _, l := range b {
		ops = append(ops, op{kind: '+', line: l})
	}
	return ops
}

// render groups changed ops into hunks separated by > 2*context equal lines,
// extends each with context, and emits git-style output.
func render(filename string, ops []op, context int, aTotal int, aNL bool, bTotal int, bNL bool) string {
	// Prefix sums: lines of a/b consumed before ops[i].
	aBefore := make([]int, len(ops)+1)
	bBefore := make([]int, len(ops)+1)
	for i, o := range ops {
		aBefore[i+1] = aBefore[i]
		bBefore[i+1] = bBefore[i]
		switch o.kind {
		case ' ':
			aBefore[i+1]++
			bBefore[i+1]++
		case '-':
			aBefore[i+1]++
		case '+':
			bBefore[i+1]++
		}
	}

	var clusters [][2]int // inclusive op-index range of a change cluster
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		if n := len(clusters); n > 0 && i-clusters[n-1][1] <= 2*context {
			clusters[n-1][1] = i
		} else {
			clusters = append(clusters, [2]int{i, i})
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", filename, filename)
	for _, c := range clusters {
		lo := max(0, c[0]-context)
		hi := min(len(ops), c[1]+1+context)
		emitHunk(&out, ops, aBefore, bBefore, lo, hi, aTotal, aNL, bTotal, bNL)
	}
	return out.String()
}

func emitHunk(out *strings.Builder, ops []op, aBefore, bBefore []int, lo, hi int, aTotal int, aNL bool, bTotal int, bNL bool) {
	aCount, bCount := 0, 0
	for i := lo; i < hi; i++ {
		switch ops[i].kind {
		case ' ':
			aCount++
			bCount++
		case '-':
			aCount++
		case '+':
			bCount++
		}
	}

	aStart, bStart := aBefore[lo], bBefore[lo]
	fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n",
		startLine(aStart, aCount), aCount, startLine(bStart, bCount), bCount)
	for i := lo; i < hi; i++ {
		fmt.Fprintf(out, "%c%s\n", ops[i].kind, ops[i].line)
		switch ops[i].kind {
		case '-':
			if aBefore[i+1] == aTotal && !aNL {
				fmt.Fprint(out, "\\ No newline at end of file\n")
			}
		case '+':
			if bBefore[i+1] == bTotal && !bNL {
				fmt.Fprint(out, "\\ No newline at end of file\n")
			}
		}
	}
}

// startLine is the 1-based first line of a hunk region (0 when the hunk
// adds/removes at the very start of an empty side).
func startLine(before, count int) int {
	if count == 0 {
		return before
	}
	return before + 1
}
