package search

import (
	"bytes"
	"regexp"
	"strings"
)

// codeSnippetMaxLen caps the re-derived single-line snippet returned for code
// hits. Matches the truncation length the agent path (`cmd/ox/code.go`)
// already applies via compactSnippet.
const codeSnippetMaxLen = 120

// extractSearchTerms returns the user's literal search terms from a Bleve
// query-string-syntax string, in order. Strips Bleve syntax (+/-/quotes/glob
// wildcards), operator words (AND/OR/NOT), field qualifiers (field:value →
// value), and very short fragments. Best-effort — used only to locate a match
// in source for snippet re-derivation (ADR-018 phase 1b); a miss yields an
// empty snippet, never an error.
func extractSearchTerms(bleveQuery string) []string {
	var terms []string
	for _, f := range strings.Fields(bleveQuery) {
		f = strings.Trim(f, `+-"*?()`)
		if f == "" {
			continue
		}
		switch strings.ToUpper(f) {
		case "AND", "OR", "NOT":
			continue
		}
		if i := strings.Index(f, ":"); i >= 0 {
			f = f[i+1:]
		}
		if len(f) < 2 {
			continue
		}
		terms = append(terms, f)
	}
	return terms
}

// locateMatchAndSnippet finds a likely match position in blob content for the
// given plan and returns (1-based line number, single-line snippet truncated
// to maxLen). For regex plans it runs the regex; for query-string plans it
// scans for the earliest occurrence of any extracted term (case-insensitive).
// Returns (0, "") when nothing's found — caller treats empty snippet as "no
// preview available," not an error.
func locateMatchAndSnippet(content []byte, plan *ExecutionPlan, maxLen int) (int, string) {
	if len(content) == 0 {
		return 0, ""
	}
	if plan.IsRegex {
		re, err := regexp.Compile("(?i)" + plan.BleveQuery)
		if err != nil {
			return 0, ""
		}
		loc := re.FindIndex(content)
		if loc == nil {
			return 0, ""
		}
		return lineAtOffset(content, loc[0], maxLen)
	}

	terms := extractSearchTerms(plan.BleveQuery)
	if len(terms) == 0 {
		return 0, ""
	}
	lower := bytes.ToLower(content)
	best := -1
	for _, t := range terms {
		idx := bytes.Index(lower, []byte(strings.ToLower(t)))
		if idx >= 0 && (best == -1 || idx < best) {
			best = idx
		}
	}
	if best < 0 {
		return 0, ""
	}
	return lineAtOffset(content, best, maxLen)
}

// lineAtOffset returns the 1-based line number of byte offset off in content,
// and the matching line's text trimmed and truncated to maxLen.
func lineAtOffset(content []byte, off int, maxLen int) (int, string) {
	if off < 0 || off >= len(content) {
		return 0, ""
	}
	// count newlines up to off
	line := 1 + bytes.Count(content[:off], []byte{'\n'})
	// expand to line bounds
	start := off
	for start > 0 && content[start-1] != '\n' {
		start--
	}
	end := off
	for end < len(content) && content[end] != '\n' {
		end++
	}
	s := strings.TrimSpace(string(content[start:end]))
	// truncate to maxLen, respecting rune boundaries
	if len(s) > maxLen {
		// find rune boundary at-or-below maxLen
		cut := maxLen
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return line, s
}

// isRuneStart reports whether b begins a UTF-8 sequence (i.e., is not a
// continuation byte). Used to avoid cutting in the middle of a multi-byte rune.
func isRuneStart(b byte) bool {
	return b < 0x80 || b >= 0xC0
}
