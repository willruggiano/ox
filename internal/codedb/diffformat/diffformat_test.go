package diffformat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFormat_Basic covers the three change shapes (add/delete/modify) and the
// header structure. Failure prevented: drift in the diff text format would
// split-brain the indexer's tokens against the search-time regeneration (ADR-018
// phase 2 Option A relies on byte-stable output).
func TestFormat_Basic(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		oldText    string
		newText    string
		wantHeader string
		wantOldN   int // count of '-' prefixed content lines (excludes headers)
		wantNewN   int // count of '+' prefixed content lines (excludes headers)
	}{
		{"modify", "foo.go", "old\nline", "new\nline", "--- a/foo.go\n+++ b/foo.go\n", 2, 2},
		{"add", "new.go", "", "added\n", "--- a/new.go\n+++ b/new.go\n", 0, 1},
		{"delete", "gone.go", "removed\n", "", "--- a/gone.go\n+++ b/gone.go\n", 1, 0},
		{"both empty", "noop.go", "", "", "--- a/noop.go\n+++ b/noop.go\n", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Format(tt.path, tt.oldText, tt.newText)
			require.True(t, strings.HasPrefix(got, tt.wantHeader),
				"want header prefix %q, got %q", tt.wantHeader, got)
			require.Equal(t, tt.wantOldN, countPrefixedLines(got, '-'),
				"old-side ('-') content line count")
			require.Equal(t, tt.wantNewN, countPrefixedLines(got, '+'),
				"new-side ('+') content line count")
		})
	}
}

// TestFormat_LineCap verifies each side is bounded at MaxLines lines. Failure
// prevented: an unbounded diff would let one huge change blow up Bleve token
// counts and search-time regeneration cost.
func TestFormat_LineCap(t *testing.T) {
	// 2 * MaxLines lines on each side; expect cap.
	makeText := func(prefix string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(prefix)
			b.WriteByte('\n')
		}
		return b.String()
	}
	got := Format("big.go", makeText("o", MaxLines*2), makeText("n", MaxLines*2))
	require.Equal(t, MaxLines, countPrefixedLines(got, '-'),
		"old side must cap at MaxLines lines")
	require.Equal(t, MaxLines, countPrefixedLines(got, '+'),
		"new side must cap at MaxLines lines")
}

// TestFormat_NoTrailingNewlineHandled covers the edge case of a final line
// without a trailing newline — must still be emitted as one prefixed line.
func TestFormat_NoTrailingNewlineHandled(t *testing.T) {
	got := Format("p.go", "only-line-no-nl", "only-new-line-no-nl")
	require.Contains(t, got, "\n-only-line-no-nl\n")
	require.Contains(t, got, "\n+only-new-line-no-nl\n")
}

// countPrefixedLines counts lines in s that begin with prefix (excluding the
// initial "--- a/..." and "+++ b/..." header lines).
func countPrefixedLines(s string, prefix byte) int {
	lines := strings.Split(s, "\n")
	count := 0
	for _, l := range lines {
		if l == "" {
			continue
		}
		// skip headers
		if strings.HasPrefix(l, "--- a/") || strings.HasPrefix(l, "+++ b/") {
			continue
		}
		if l[0] == prefix {
			count++
		}
	}
	return count
}
