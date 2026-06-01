// Package diffformat produces the simple "before vs after" diff text CodeDB
// indexes for diff search and re-derives at search-read time for snippets
// (ADR-018 phase 2 Option A). The format is intentionally minimal — not a
// unified diff — and MUST stay byte-stable across producers: the indexer
// writes it into the Bleve diff index for tokenization; the search assemble
// path re-computes it from blobs for the snippet shown to agents.
//
// Bump the diff mapping version in store.bleveMappingVersion("diff") and force
// a reindex if you change the format here — drift would split-brain the
// inverted index against the regenerated snippet.
package diffformat

import "strings"

// MaxLines caps each of the "before" and "after" sides at 100 lines, matching
// the indexer's historical cap.
const MaxLines = 100

// Format returns the diff text for one file change. Pass "" for oldText when
// the file was added (no prior content) and "" for newText when deleted.
func Format(path, oldText, newText string) string {
	var b strings.Builder
	b.WriteString("--- a/")
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString("+++ b/")
	b.WriteString(path)
	b.WriteByte('\n')
	if oldText != "" {
		writePrefixedLines(&b, oldText, '-', MaxLines)
	}
	if newText != "" {
		writePrefixedLines(&b, newText, '+', MaxLines)
	}
	return b.String()
}

// writePrefixedLines writes up to maxLines lines from text into b, each
// prefixed with prefix. Optimized version preserved from the indexer (avoids
// strings.SplitN allocations).
func writePrefixedLines(b *strings.Builder, text string, prefix byte, maxLines int) {
	for remaining := maxLines; remaining > 0 && text != ""; remaining-- {
		var line string
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			line = text[:idx]
			text = text[idx+1:]
		} else {
			line = text
			text = ""
		}
		b.WriteByte(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}
