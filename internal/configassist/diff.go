package configassist

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a human-readable unified diff of one file. It is the
// human's view only (design §3.1): the op bundle is authoritative and the
// diff is derived from it, never parsed back. Files past maxDiffLines on
// either side render as a whole-file replacement notice rather than a
// quadratic diff.
//
//nolint:gocognit // Plain LCS + hunk rendering is clearer here than splitting tiny stateful helpers.
func UnifiedDiff(path string, before, after string) string {
	const maxDiffLines = 4000
	a := splitLines(before)
	b := splitLines(after)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	if before == "" {
		sb.WriteString("@@ -0,0 +1," + fmt.Sprint(len(b)) + " @@\n")
		for _, l := range b {
			sb.WriteString("+" + l + "\n")
		}
		return sb.String()
	}
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		fmt.Fprintf(&sb, "@@ whole-file replacement (%d → %d lines; too large to render line by line) @@\n", len(a), len(b))
		return sb.String()
	}
	ops := lcsDiff(a, b)
	// Render with 3 lines of context, merging hunks.
	const ctx = 3
	type hunk struct{ start, end int } // indices into ops
	var hunks []hunk
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		if n := len(hunks); n > 0 && i-hunks[n-1].end <= 2*ctx+1 {
			hunks[n-1].end = i
		} else {
			hunks = append(hunks, hunk{start: i, end: i})
		}
	}
	for _, h := range hunks {
		start := h.start - ctx
		if start < 0 {
			start = 0
		}
		end := h.end + ctx
		if end > len(ops)-1 {
			end = len(ops) - 1
		}
		aStart, bStart, aLen, bLen := 0, 0, 0, 0
		for i := 0; i < start; i++ {
			if ops[i].kind != '+' {
				aStart++
			}
			if ops[i].kind != '-' {
				bStart++
			}
		}
		for i := start; i <= end; i++ {
			if ops[i].kind != '+' {
				aLen++
			}
			if ops[i].kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart+1, aLen, bStart+1, bLen)
		for i := start; i <= end; i++ {
			sb.WriteString(string(ops[i].kind) + ops[i].text + "\n")
		}
	}
	return sb.String()
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lcsDiff is a plain O(n·m) LCS line diff — adequate for config files,
// which are small by construction (the caller caps the size).
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, diffOp{'-', a[i]})
			i++
		default:
			out = append(out, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffOp{'+', b[j]})
	}
	return out
}

// ChangedLines returns the +/- lines of a unified diff (without markers),
// the material a judge's summary must quote (design test 18).
func ChangedLines(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if len(l) < 2 {
			continue
		}
		if strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "@@") {
			continue
		}
		if l[0] == '+' || l[0] == '-' {
			if t := strings.TrimSpace(l[1:]); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
