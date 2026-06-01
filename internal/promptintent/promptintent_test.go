package promptintent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLooksLikeRecall_LengthGate(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"empty", "", false},
		{"fragment find", "find a", false},
		{"fragment explain", "explain", false},
		{"just under threshold", strings.Repeat("a", MinPromptChars-1), false},
		{"long but no intent", strings.Repeat("alpha beta ", 30), false},
		// regression for ox-5vnf: short recall questions must now pass the
		// length gate now that local query is ~12ms (no cloud-tax to amortize).
		// Intent classifier still does the heavy lifting; length only guards
		// against fragment-sized false positives.
		{"short recall question", "how do we handle auth in this repo?", true},
		{"short where-is question", "where is the ledger cache helper?", true},
		{"short explain prompt", "explain what ox agent prime does please", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeRecall(tc.prompt); got != tc.want {
				t.Fatalf("LooksLikeRecall(%q)=%v want %v", tc.prompt, got, tc.want)
			}
		})
	}
}

// TestMinPromptCharsThreshold pins the threshold so a future PR that raises
// it without updating the rationale doc-comment trips this test first.
// Failure prevented: threshold drift away from the local-only cost model.
func TestMinPromptCharsThreshold(t *testing.T) {
	if MinPromptChars != 20 {
		t.Fatalf("MinPromptChars=%d, want 20 (see ox-5vnf rationale). "+
			"If raising, update the doc-comment in promptintent.go.", MinPromptChars)
	}
}

// TestTrimForMatch_UTF8Safe verifies the rune-aware trimming never splits
// a multi-byte UTF-8 codepoint. Failure prevented: emoji / CJK / accented
// Latin input being sliced mid-codepoint, producing invalid UTF-8 in the
// matcher's view of the prompt.
func TestTrimForMatch_UTF8Safe(t *testing.T) {
	// Build a prompt where each "character" is a 4-byte emoji. A
	// byte-based slice at MaxPromptCharsForMatch would land mid-codepoint
	// and produce a string ending in 0xFFFD (replacement char).
	emoji := "🔥" // 4 bytes
	prompt := strings.Repeat(emoji, MaxPromptCharsForMatch+50)
	got := TrimForMatch(prompt)
	if !utf8.ValidString(got) {
		t.Fatalf("TrimForMatch produced invalid UTF-8 — split a codepoint")
	}
	gotRunes := []rune(got)
	if len(gotRunes) > MaxPromptCharsForMatch {
		t.Fatalf("trimmed result has %d runes, expected <= %d", len(gotRunes), MaxPromptCharsForMatch)
	}
}

// TestTrimForMatch covers the upper-bound cap (ox-3ti2) that protects
// ledgersearch from term dilution on long planning preambles.
// Failure prevented: long prompts being passed verbatim, producing zero
// matches under AND semantics or diluted ranking under OR semantics.
func TestTrimForMatch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // exact length expected, or -1 for "unchanged"
	}{
		{"short prompt unchanged", "how do we handle auth?", -1},
		{"exactly at threshold", strings.Repeat("a", MaxPromptCharsForMatch), -1},
		{"one over threshold no space", strings.Repeat("a", MaxPromptCharsForMatch+1), MaxPromptCharsForMatch},
		{
			"long prompt with late space falls back to hard cut",
			strings.Repeat("a", MaxPromptCharsForMatch*4/5-1) + " " + strings.Repeat("b", MaxPromptCharsForMatch),
			MaxPromptCharsForMatch,
		},
		{
			"long prompt with space inside upper 20% trims to space",
			strings.Repeat("a", MaxPromptCharsForMatch-10) + " " + strings.Repeat("b", 100),
			MaxPromptCharsForMatch - 10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TrimForMatch(tc.in)
			if tc.want == -1 {
				if got != tc.in {
					t.Fatalf("expected unchanged for len=%d, got len=%d", len(tc.in), len(got))
				}
				return
			}
			if len(got) != tc.want {
				t.Fatalf("TrimForMatch len=%d, want %d (input len=%d)", len(got), tc.want, len(tc.in))
			}
			// invariant: trimmed result is always a prefix of the input
			if !strings.HasPrefix(tc.in, got) {
				t.Fatalf("trimmed result is not a prefix of the input — TrimForMatch must never modify content")
			}
		})
	}
}

// TestMaxPromptCharsForMatchThreshold pins the upper cap so a future PR
// that raises it without re-evaluating the term-dilution analysis trips
// this test first.
func TestMaxPromptCharsForMatchThreshold(t *testing.T) {
	if MaxPromptCharsForMatch != 400 {
		t.Fatalf("MaxPromptCharsForMatch=%d, want 400 (see ox-3ti2 rationale). "+
			"If raising, document why term-dilution is no longer a concern.", MaxPromptCharsForMatch)
	}
}

func TestLooksLikeRecall_RecallIntents(t *testing.T) {
	const filler = " — providing extra context here so the prompt clears the length gate without changing leading intent shape"
	cases := []string{
		"find the spot where we decided to use rate-limited queues for the daemon path" + filler,
		"how did we end up handling LFS pointer hydration without the git-lfs binary?" + filler,
		"has anyone explored using a content-addressed cache for redaction rules across sessions?" + filler,
		"what is the canonical place that owns the ledger path resolution helper in the codebase?" + filler,
		"explain the trade-off we picked between inline summarization and delegated summarization workflows" + filler,
		"where is the rate limiter used for the murmur delivery channel implemented in this repo?" + filler,
		"why does the prime command emit a banner even when stdout is not a tty in our hook flow?" + filler,
		"search for the redaction rule file we added in the gitleaks-extras integration last week" + filler,
	}
	for _, c := range cases {
		t.Run(c[:30], func(t *testing.T) {
			if !LooksLikeRecall(c) {
				t.Fatalf("expected recall intent for %q", c)
			}
		})
	}
}

func TestLooksLikeRecall_ActionVerbSkipped(t *testing.T) {
	cases := []string{
		"fix the bug where ox doctor reports a stale recording even when the daemon was bounced cleanly",
		"add a new redaction rule for AWS access key pairs that mirrors the GitHub PAT rule we shipped last sprint",
		"write a regression test that proves the hook does not block past the 100ms budget when the daemon is slow",
		"refactor the agent_hook.go phase dispatcher so each phase handler lives in its own file under cmd/ox",
		"delete the dead code in internal/lfs/legacy.go that nothing has imported since the rewrite landed",
		"implement the new whisper cap rule so per-author murmurs are bounded before token budgeting runs",
	}
	for _, c := range cases {
		t.Run(c[:20], func(t *testing.T) {
			if LooksLikeRecall(c) {
				t.Fatalf("expected action verb skip for %q", c)
			}
		})
	}
}

func TestLooksLikeRecall_ActionVerbDoesNotMatchMidPrompt(t *testing.T) {
	// "explain why fix X failed" should still trigger recall — the word
	// "fix" appears later, not as the leading verb.
	p := "explain why the fix we attempted in the prime command failed when run under the gemini cli adapter, including the relevant context around the hook firing order details"
	if !LooksLikeRecall(p) {
		t.Fatalf("expected mid-prompt action verb to NOT veto recall: %q", p)
	}
}

func TestStartsWithActionVerb_TrailingPunct(t *testing.T) {
	if !startsWithActionVerb("fix, the thing") {
		t.Fatalf("expected punctuation-stripped action verb to match")
	}
	if !startsWithActionVerb("FIX the thing") {
		t.Fatalf("expected case-insensitive action verb match")
	}
	if startsWithActionVerb("explain the thing") {
		t.Fatalf("non-action verb should not match")
	}
}
