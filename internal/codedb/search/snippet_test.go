package search

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExtractSearchTerms documents what extractSearchTerms strips from a Bleve
// query so a future change here that breaks snippet location is caught.
func TestExtractSearchTerms(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"single term", "foo", []string{"foo"}},
		{"AND terms", "foo bar", []string{"foo", "bar"}},
		{"OR ignored", "foo OR bar", []string{"foo", "bar"}},
		{"NOT ignored", "foo NOT bar", []string{"foo", "bar"}},
		{"negation prefix stripped", "foo -bar", []string{"foo", "bar"}},
		{"required prefix stripped", "+foo bar", []string{"foo", "bar"}},
		{"field qualifier stripped", "lang:go mutex", []string{"go", "mutex"}},
		{"too short ignored", "x foo", []string{"foo"}},
		{"wildcards stripped", "*foo*", []string{"foo"}},
		{"empty", "", nil},
		{"only operators", "AND OR NOT", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSearchTerms(tt.query)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestLocateMatchAndSnippet_Literal covers the common search path: locating
// the first occurrence of any extracted term and returning the matching line.
// Failure prevented: code search returning Line=0 or empty snippet when the
// term clearly appears in the source.
func TestLocateMatchAndSnippet_Literal(t *testing.T) {
	content := []byte(`package main

import "fmt"

func main() {
	x := computeFoo()
	fmt.Println(x)
}
`)
	tests := []struct {
		name     string
		query    string
		wantLine int
		wantSub  string // substring expected in the snippet
	}{
		{"single term", "computeFoo", 6, "computeFoo"},
		{"case-insensitive", "COMPUTEFOO", 6, "computeFoo"},
		{"first of two terms wins", "Println computeFoo", 6, "computeFoo"},
		{"falls back across terms", "missing computeFoo", 6, "computeFoo"},
		{"no match", "doesNotExist", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &ExecutionPlan{BleveQuery: tt.query, IsRegex: false}
			line, snippet := locateMatchAndSnippet(content, plan, codeSnippetMaxLen)
			require.Equal(t, tt.wantLine, line)
			if tt.wantSub == "" {
				require.Equal(t, "", snippet)
			} else {
				require.Contains(t, snippet, tt.wantSub)
			}
		})
	}
}

// TestLocateMatchAndSnippet_Regex verifies the regex path used by `patterntype:regexp`.
func TestLocateMatchAndSnippet_Regex(t *testing.T) {
	content := []byte("package x\n\nfunc handle(err error) error {\n\treturn err\n}\n")
	plan := &ExecutionPlan{BleveQuery: `func\s+\w+\(`, IsRegex: true}
	line, snippet := locateMatchAndSnippet(content, plan, codeSnippetMaxLen)
	require.Equal(t, 3, line)
	require.Contains(t, snippet, "func handle")
}

// TestLocateMatchAndSnippet_Truncation verifies the snippet honors the max
// length and doesn't slice a multi-byte UTF-8 rune.
func TestLocateMatchAndSnippet_Truncation(t *testing.T) {
	// a long line with the match early, then plenty more text including a multi-byte rune past the cap
	long := strings.Repeat("a", 80) + " match " + strings.Repeat("β", 80) // β = 2 bytes
	content := []byte(long + "\n")
	plan := &ExecutionPlan{BleveQuery: "match"}
	_, snippet := locateMatchAndSnippet(content, plan, 50)
	require.LessOrEqual(t, len(snippet), 50)
	// must be valid UTF-8 (no orphaned continuation byte at the cut)
	require.True(t, isValidUTF8(snippet), "snippet must end on a rune boundary")
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestLineAtOffset spot-checks the 1-based line counting around boundaries.
func TestLineAtOffset(t *testing.T) {
	c := []byte("a\nbb\nccc\n")
	tests := []struct {
		off, wantLine int
		wantSnip      string
	}{
		{0, 1, "a"},
		{2, 2, "bb"},
		{5, 3, "ccc"},
	}
	for _, tt := range tests {
		line, snip := lineAtOffset(c, tt.off, 120)
		require.Equal(t, tt.wantLine, line, "off=%d", tt.off)
		require.Equal(t, tt.wantSnip, snip, "off=%d", tt.off)
	}
}
