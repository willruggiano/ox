package store

import (
	"log/slog"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/sageox/ox/internal/codedb/diffformat"
)

// diffSnippetCacheSize bounds the in-process diff-text cache used by the
// search read path's snippet re-derivation (ADR-018 phase 2 Option A). 512
// recent diffs × <~10 KB each ≈ a few MB live — small relative to the daemon
// footprint, big enough to absorb repeat-hit workloads.
const diffSnippetCacheSize = 512

// DiffSnippet returns the diff text (indexer-format) for one file change,
// re-derived from the two blob content_hashes. Empty hashes are treated as
// "no content on that side" (add/delete). A per-Store LRU caches recent diffs
// so repeat hits during a search session don't re-read blobs.
//
// Returns "" when neither side could be read (blob missing, binary, or too
// large) — callers treat empty as "no snippet available" rather than an error.
func (s *Store) DiffSnippet(oldHash, newHash, path string) string {
	s.initDiffCache()
	key := oldHash + "|" + newHash + "|" + path
	if s.diffCache != nil {
		if v, ok := s.diffCache.Get(key); ok {
			return v
		}
	}
	var oldText, newText string
	if oldHash != "" {
		if b := s.ReadBlob(oldHash); b != nil {
			oldText = string(b)
		}
	}
	if newHash != "" {
		if b := s.ReadBlob(newHash); b != nil {
			newText = string(b)
		}
	}
	// Honor the doc'd empty contract: when both sides are unreadable (missing
	// blobs, binary, or too large), return "" rather than the bare
	// "--- a/path\n+++ b/path\n" header diffformat.Format would otherwise emit.
	// Callers treat empty as "no snippet available".
	if oldText == "" && newText == "" {
		if s.diffCache != nil {
			s.diffCache.Add(key, "")
		}
		return ""
	}
	text := diffformat.Format(path, oldText, newText)
	if s.diffCache != nil {
		s.diffCache.Add(key, text)
	}
	return text
}

// initDiffCache lazily allocates the LRU on first use. Allocation failure
// (only on size <= 0) degrades to no-cache pass-through.
func (s *Store) initDiffCache() {
	s.diffCacheOnce.Do(func() {
		c, err := lru.New[string, string](diffSnippetCacheSize)
		if err != nil {
			slog.Warn("init diff snippet LRU failed; running without cache",
				"size", diffSnippetCacheSize, "err", err)
			return
		}
		s.diffCache = c
	})
}
