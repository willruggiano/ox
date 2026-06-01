// Package promptintent implements lightweight classification of a user's
// freshly submitted prompt to decide whether it is worth firing a local
// recall query against the ledger / team-context cache.
//
// Three gates run in order:
//
//  1. Length gate (MinPromptChars) — rules out single-token noise like
//     "yes", "ok", "go", "fix" where recall would surface nothing useful.
//     Threshold is intentionally LOW because the recall runner is local
//     and ~12ms — there is no cloud round-trip tax to amortize.
//  2. Intent classifier — only prompts that look like a *question* or
//     *recall request* are forwarded. Prompts that begin with a clear
//     edit/action verb (fix, add, write, ...) are skipped because the user
//     is already mid-flow and wants execution, not retrieval.
//  3. Match-surface cap (MaxPromptCharsForMatch) — long prompts have their
//     match surface trimmed BEFORE being handed to the runner. The user's
//     actual prompt is never modified; only the bytes used for matching
//     shrink, so AND-term dilution / OR-result-flooding stays bounded.
//
// The package is intentionally dependency-free and pure-string so it can
// be unit-tested in isolation and reused outside the hook handler.
package promptintent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MinPromptChars is the lower bound (in runes, not bytes) below which the
// prompt is considered too short to bother running recall on.
//
// Rationale (revised under ox-5vnf): the original threshold of 160 chars
// (~40 tokens) was tuned when every recall fire was assumed to cost a
// 200-2000ms cloud round-trip — the gate existed to amortize that tax.
// With ox-m01h's local ledgersearch running at ~12ms p95, the cost is
// near-zero. The intent classifier (recallPatterns below) does the real
// work of filtering meaningless prompts; this length gate now exists
// only to reject intent-classifier false positives on fragment-sized
// prompts like "find a" or "explain" that match the regex but carry
// no searchable signal. 20 runes is the boundary between fragments
// ("find a" = 6) and minimum-viable recall questions ("where is the
// ledger" = 19, "how do we handle X" = 18).
//
// Counted in runes so a non-ASCII prompt (CJK, emoji, accented Latin)
// is treated like its ASCII equivalent — a 20-character question is a
// 20-character question regardless of UTF-8 encoding width.
const MinPromptChars = 20

// MaxPromptCharsForMatch caps the number of runes (not bytes) handed to
// the recall runner for matching. Long planning preambles (1000+ chars)
// get tokenized into many keywords; AND semantics produce zero matches
// and OR semantics dilute ranking — either way the 5-line preamble budget
// either stays empty or fills with irrelevant hits.
//
// 400 runes (~100 tokens) is enough to capture the user's intent
// sentence(s) while staying within a sensible matching surface.
// This is intentionally MVP — keyword extraction (CamelCase tokens,
// snake_case identifiers, proper nouns) is a follow-up if truncation
// proves lossy in practice.
//
// Counted in runes so a UTF-8 prompt (emoji, CJK, accented Latin) is
// never sliced mid-codepoint, which would produce invalid UTF-8 and a
// garbled trailing character in the matcher's view of the prompt.
const MaxPromptCharsForMatch = 400

// TrimForMatch returns the prompt trimmed to at most MaxPromptCharsForMatch
// runes, preferring to cut on a word boundary so we never split an
// identifier in half. Returns the prompt unchanged if it already fits.
//
// The caller must keep the original prompt for downstream rendering —
// only the matching surface should ever see the trimmed copy.
//
// Rune-aware: counts and slices on []rune so UTF-8 multi-byte sequences
// are preserved intact. A byte-based slice could split a codepoint and
// hand the runner invalid UTF-8 with a garbled trailing character.
func TrimForMatch(prompt string) string {
	runes := []rune(prompt)
	if len(runes) <= MaxPromptCharsForMatch {
		return prompt
	}
	cut := runes[:MaxPromptCharsForMatch]
	// prefer last whitespace boundary within the cut window so we do not
	// slice through a token. Only honor the boundary if it leaves at
	// least 80% of the budget intact — otherwise the hard cut is closer
	// to user intent than a too-short word-boundary cut.
	threshold := MaxPromptCharsForMatch * 4 / 5
	for i := len(cut) - 1; i >= threshold; i-- {
		if r := cut[i]; r == ' ' || r == '\t' || r == '\n' {
			return string(cut[:i])
		}
	}
	return string(cut)
}

// recallPatterns match phrases that signal the user is trying to recall
// or discover something. They are intentionally broad — false positives
// here only cost a 100ms local lookup, false negatives lose discovery.
var recallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bfind\b`),
	regexp.MustCompile(`(?i)\brecall\b`),
	regexp.MustCompile(`(?i)\bhow\s+did\s+we\b`),
	regexp.MustCompile(`(?i)\bhow\s+do\s+we\b`),
	regexp.MustCompile(`(?i)\bhas\s+anyone\b`),
	regexp.MustCompile(`(?i)\bwhat\s+is\b`),
	regexp.MustCompile(`(?i)\bwhat\s+are\b`),
	regexp.MustCompile(`(?i)\bexplain\b`),
	regexp.MustCompile(`(?i)\bwhere\s+is\b`),
	regexp.MustCompile(`(?i)\bwhy\s+does\b`),
	regexp.MustCompile(`(?i)\bwhy\s+did\b`),
	regexp.MustCompile(`(?i)\bwhen\s+did\b`),
	regexp.MustCompile(`(?i)\bwho\s+(wrote|owns|added|changed)\b`),
	regexp.MustCompile(`(?i)\bsearch\s+for\b`),
	regexp.MustCompile(`(?i)\blook\s+up\b`),
}

// actionVerbs are leading words that imply the user wants execution, not
// retrieval. Matched only at the start of the prompt (after whitespace)
// so that "explain why fix X failed" still triggers recall.
var actionVerbs = map[string]struct{}{
	"edit":      {},
	"fix":       {},
	"add":       {},
	"make":      {},
	"write":     {},
	"create":    {},
	"delete":    {},
	"remove":    {},
	"refactor":  {},
	"rename":    {},
	"implement": {},
	"build":     {},
	"run":       {},
	"commit":    {},
	"push":      {},
	"merge":     {},
	"rebase":    {},
	"deploy":    {},
}

// LooksLikeRecall reports whether the given prompt is worth running a
// local recall query against. It returns true only when:
//   - the prompt is at least MinPromptChars long, AND
//   - it does NOT start with a recognized action verb, AND
//   - it matches at least one recall pattern.
//
// The function is pure: no I/O, no allocations beyond regex matching.
func LooksLikeRecall(prompt string) bool {
	trimmed := strings.TrimSpace(prompt)
	// rune-aware length so a 20-character non-ASCII prompt isn't rejected
	// just because UTF-8 makes each character 2-4 bytes
	if utf8.RuneCountInString(trimmed) < MinPromptChars {
		return false
	}
	if startsWithActionVerb(trimmed) {
		return false
	}
	for _, re := range recallPatterns {
		if re.MatchString(trimmed) {
			return true
		}
	}
	return false
}

// startsWithActionVerb returns true if the first whitespace-delimited
// token of the prompt is a recognized action verb. Comparison is
// case-insensitive and strips trailing punctuation from the leading word.
func startsWithActionVerb(s string) bool {
	first := s
	if idx := strings.IndexAny(s, " \t\n"); idx >= 0 {
		first = s[:idx]
	}
	first = strings.TrimRight(first, ",.;:!?")
	first = strings.ToLower(first)
	_, ok := actionVerbs[first]
	return ok
}
