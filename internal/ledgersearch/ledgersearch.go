// Package ledgersearch provides zero-network, local-only full-text search over
// the cached ledger contents: session summaries/markdown and recent murmurs.
//
// Design choice (see ox-m01h): this is an in-memory grep over a bounded window
// of recent files rather than a persistent bleve index. Rationale:
//   - The ledger sparse-checkout already keeps the on-disk footprint small
//     (default 30 days of github data, 12 hours of murmurs, all sessions
//     within sparse checkout).
//   - For a repo of ox's size (~250 sessions over months) a cold scan over
//     just the small per-session summary.md + meta.json files is well under
//     50ms on warm cache, which satisfies the target p95.
//   - Building a bleve index would add an indexing dependency to every hook
//     invocation and add cache-staleness logic for marginal gain at current
//     sizes. Revisit if session count grows or queries get more complex.
//
// All callers MUST go through Search — the package guarantees:
//   - Zero network calls.
//   - Fail-open: any read error or empty cache returns empty results, nil err.
//   - Bounded scan: respects MaxSessionAge and result limit.
package ledgersearch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxSessionAge bounds how far back we scan session folders.
// Older sessions exist in git history but aren't worth scanning live for
// the UserPromptSubmit hook latency budget.
const MaxSessionAge = 90 * 24 * time.Hour

// MaxMurmurAge bounds murmur scanning. Sparse checkout currently keeps only
// the last 12 hours hydrated; we scan a slightly wider window to be safe.
const MaxMurmurAge = 7 * 24 * time.Hour

// DefaultLimit is returned when callers pass limit <= 0.
const DefaultLimit = 5

// Result is a single hit returned to callers. Shape mirrors api.QueryResult
// loosely so 'ox query --local' output stays compatible with team-context
// query consumers, but this package stays free of api/ imports to keep the
// dependency graph clean.
type Result struct {
	// Score is a simple 0..1 relevance score (term-frequency normalized).
	Score float64 `json:"score"`
	// Text is a short snippet around the matched term.
	Text string `json:"text"`
	// DocType is "session" or "murmur".
	DocType string `json:"doc_type"`
	// FilePath is the absolute on-disk path of the source file.
	FilePath string `json:"file_path"`
	// SourceType is "ledger" — distinguishes from team-context results when merged.
	SourceType string `json:"source_type"`
	// SourceID is the session folder name or murmur ID.
	SourceID string `json:"source_id"`
	// CreatedAt is the source's timestamp (ISO 8601). Empty if unknown.
	CreatedAt string `json:"created_at,omitempty"`
}

// Options controls a Search invocation.
type Options struct {
	// LedgerPath is the absolute path to the ledger checkout. If empty,
	// Search returns no results (fail-open: no ledger = no local matches).
	LedgerPath string
	// Query is the search text. Multi-word queries are AND-matched.
	Query string
	// Limit caps the number of results. <=0 uses DefaultLimit.
	Limit int
	// Now is injectable for tests. Zero value uses time.Now().
	Now time.Time
}

// Search scans the local ledger for query matches. Pure-local, no network.
// Returns ([], nil) on any read error — this is intentional fail-open behavior
// per the ox-m01h spec (hooks must never error out on missing cache).
func Search(opts Options) ([]Result, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, nil
	}
	if opts.LedgerPath == "" {
		return nil, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	// short-circuit if ledger directory doesn't exist (first session before sync)
	if _, err := os.Stat(opts.LedgerPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		slog.Debug("ledgersearch: ledger path stat failed", "path", opts.LedgerPath, "err", err)
		return nil, nil
	}

	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, nil
	}

	var results []Result
	results = append(results, scanSessions(opts.LedgerPath, terms, now)...)
	results = append(results, scanMurmurs(opts.LedgerPath, terms, now)...)

	// sort by score desc, ties broken by recency desc
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].CreatedAt > results[j].CreatedAt
	})

	if len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

// tokenize splits a query into lowercase terms. Strips punctuation so
// "auth?" matches "auth".
func tokenize(q string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range strings.ToLower(q) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			cur.WriteRune(r)
			continue
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// scanSessions walks sessions/<session-name>/ and scores summary/meta hits.
// Reads only summary.md and meta.json — skips raw.jsonl (large, low signal,
// often dehydrated to LFS pointer files).
func scanSessions(ledgerPath string, terms []string, now time.Time) []Result {
	sessionsDir := filepath.Join(ledgerPath, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	cutoff := now.Add(-MaxSessionAge)
	var results []Result

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		ts := parseSessionTimestamp(name)
		if !ts.IsZero() && ts.Before(cutoff) {
			continue
		}
		sessionPath := filepath.Join(sessionsDir, name)

		// scan summary.md first (highest signal), fall back to session.md
		for _, candidate := range []string{"summary.md", "session.md"} {
			path := filepath.Join(sessionPath, candidate)
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				continue
			}
			score, snippet := scoreContent(string(data), terms)
			if score <= 0 {
				continue
			}
			results = append(results, Result{
				Score:      score,
				Text:       snippet,
				DocType:    "session",
				FilePath:   path,
				SourceType: "ledger",
				SourceID:   name,
				CreatedAt:  ts.Format(time.RFC3339),
			})
			break // one hit per session
		}
	}
	return results
}

// scanMurmurs walks data/murmurs/YYYY-MM-DD/HH/*.json scoring content matches.
func scanMurmurs(ledgerPath string, terms []string, now time.Time) []Result {
	murmursDir := filepath.Join(ledgerPath, "data", "murmurs")
	if _, err := os.Stat(murmursDir); err != nil {
		return nil
	}

	cutoff := now.Add(-MaxMurmurAge)
	var results []Result

	dayEntries, err := os.ReadDir(murmursDir)
	if err != nil {
		return nil
	}
	for _, day := range dayEntries {
		if !day.IsDir() {
			continue
		}
		dayTime, perr := time.Parse("2006-01-02", day.Name())
		if perr != nil || dayTime.Before(cutoff.Truncate(24*time.Hour)) {
			continue
		}
		dayPath := filepath.Join(murmursDir, day.Name())
		hourEntries, _ := os.ReadDir(dayPath)
		for _, hour := range hourEntries {
			if !hour.IsDir() {
				continue
			}
			hourPath := filepath.Join(dayPath, hour.Name())
			files, _ := os.ReadDir(hourPath)
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				path := filepath.Join(hourPath, f.Name())
				data, rerr := os.ReadFile(path)
				if rerr != nil {
					continue
				}
				var m struct {
					ID        string    `json:"id"`
					Timestamp time.Time `json:"timestamp"`
					Topic     string    `json:"topic"`
					Content   string    `json:"content"`
				}
				if jerr := json.Unmarshal(data, &m); jerr != nil {
					continue
				}
				if !m.Timestamp.IsZero() && m.Timestamp.Before(cutoff) {
					continue
				}
				// search across topic + content
				score, snippet := scoreContent(m.Topic+"\n"+m.Content, terms)
				if score <= 0 {
					continue
				}
				results = append(results, Result{
					Score:      score,
					Text:       snippet,
					DocType:    "murmur",
					FilePath:   path,
					SourceType: "ledger",
					SourceID:   m.ID,
					CreatedAt:  m.Timestamp.Format(time.RFC3339),
				})
			}
		}
	}
	return results
}

// scoreContent returns a relevance score and a short snippet for content matching all terms.
// Score = (matched-term-count / total-terms) + tf-bonus capped at 1.0.
// Snippet is the first ~160 chars around the first matched term.
func scoreContent(content string, terms []string) (float64, string) {
	if content == "" {
		return 0, ""
	}
	lower := strings.ToLower(content)
	matched := 0
	firstIdx := -1
	totalHits := 0
	for _, t := range terms {
		idx := strings.Index(lower, t)
		if idx < 0 {
			continue
		}
		matched++
		if firstIdx < 0 || idx < firstIdx {
			firstIdx = idx
		}
		// count tf with a cheap bounded scan (max 10 hits per term)
		hits := 0
		start := 0
		for hits < 10 {
			next := strings.Index(lower[start:], t)
			if next < 0 {
				break
			}
			hits++
			start += next + len(t)
		}
		totalHits += hits
	}
	if matched == 0 {
		return 0, ""
	}
	// AND semantics: require all terms matched
	if matched < len(terms) {
		return 0, ""
	}
	tfBonus := float64(totalHits) / 100.0
	if tfBonus > 0.5 {
		tfBonus = 0.5
	}
	score := 0.5 + tfBonus
	return score, snippetAround(content, firstIdx, 160)
}

// snippetAround returns a window of size chars around idx, trimmed at whitespace.
func snippetAround(s string, idx, size int) string {
	if idx < 0 {
		return ""
	}
	start := idx - size/2
	if start < 0 {
		start = 0
	}
	end := start + size
	if end > len(s) {
		end = len(s)
	}
	out := s[start:end]
	out = strings.TrimSpace(out)
	// collapse internal whitespace runs for compact display
	out = strings.Join(strings.Fields(out), " ")
	if start > 0 {
		out = "..." + out
	}
	if end < len(s) {
		out = out + "..."
	}
	return out
}

// parseSessionTimestamp pulls the prefix from "YYYY-MM-DDTHH-MM-<user>-<id>".
// Returns zero time if the prefix doesn't match — callers treat that as "include".
func parseSessionTimestamp(name string) time.Time {
	if len(name) < 16 {
		return time.Time{}
	}
	// reshape "2026-02-13T14-56" -> "2026-02-13T14:56"
	prefix := name[:16]
	if prefix[13] != '-' || prefix[10] != 'T' {
		return time.Time{}
	}
	candidate := prefix[:13] + ":" + prefix[14:]
	t, err := time.Parse("2006-01-02T15:04", candidate)
	if err != nil {
		return time.Time{}
	}
	return t
}
