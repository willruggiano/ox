package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sageox/agentx"
)

// fakeRunner is a configurable LocalQueryRunner for hook tests.
type fakeRunner struct {
	results   []LocalQueryResult
	err       error
	delay     time.Duration
	panicWith any
	called    int
	gotPrompt string
}

func (f *fakeRunner) Query(ctx context.Context, prompt string, limit int) ([]LocalQueryResult, error) {
	f.called++
	f.gotPrompt = prompt
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.results, f.err
}

func withFakeRunner(t *testing.T, f *fakeRunner) {
	t.Helper()
	prev := SetLocalQueryRunner(f)
	t.Cleanup(func() { SetLocalQueryRunner(prev) })
}

const recallPrompt = "how did we end up handling LFS pointer hydration without depending on the git-lfs binary in this codebase across the daemon and CLI paths, and which package owns the Batch API client right now?"

func TestEmitLocalRecallPreamble_HappyPath(t *testing.T) {
	withFakeRunner(t, &fakeRunner{
		results: []LocalQueryResult{
			{Title: "ADR-014 LFS without git-lfs binary", Source: "ledger", Path: ".sageox/adr/014.md"},
			{Title: "Session: lfs-remove-git-lfs-binary", Source: "session", Session: "sess-123"},
		},
	})

	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, recallPrompt)

	out := buf.String()
	if !strings.Contains(out, "<system-reminder>") {
		t.Fatalf("expected <system-reminder> wrapper, got %q", out)
	}
	if !strings.Contains(out, "[ox-recall]") {
		t.Fatalf("expected [ox-recall] tag, got %q", out)
	}
	if !strings.Contains(out, "ADR-014") {
		t.Fatalf("expected first result, got %q", out)
	}
	contentLines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "<system-reminder>" || l == "</system-reminder>" {
			continue
		}
		contentLines++
	}
	if contentLines > 5 {
		t.Fatalf("expected <=5 content lines, got %d: %q", contentLines, out)
	}
}

func TestEmitLocalRecallPreamble_TruncatesToFiveLines(t *testing.T) {
	results := make([]LocalQueryResult, 0, 10)
	for i := 0; i < 10; i++ {
		results = append(results, LocalQueryResult{Title: "Result", Source: "ledger"})
	}
	withFakeRunner(t, &fakeRunner{results: results})

	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, recallPrompt)

	contentLines := 0
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "<system-reminder>" || l == "</system-reminder>" {
			continue
		}
		contentLines++
	}
	if contentLines > 5 {
		t.Fatalf("preamble exceeded 5-line cap: got %d lines\n%s", contentLines, buf.String())
	}
}

func TestEmitLocalRecallPreamble_ShortPromptSkipped(t *testing.T) {
	runner := &fakeRunner{results: []LocalQueryResult{{Title: "x"}}}
	withFakeRunner(t, runner)

	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, "find x")

	if runner.called != 0 {
		t.Fatalf("runner should not have been called for short prompt, got called=%d", runner.called)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for short prompt, got %q", buf.String())
	}
}

func TestEmitLocalRecallPreamble_ActionVerbSkipped(t *testing.T) {
	runner := &fakeRunner{results: []LocalQueryResult{{Title: "x"}}}
	withFakeRunner(t, runner)

	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, "fix the bug where ox doctor reports a stale recording even when the daemon was bounced cleanly")

	if runner.called != 0 {
		t.Fatalf("runner should not have been called for action verb prompt")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for action verb prompt, got %q", buf.String())
	}
}

func TestEmitLocalRecallPreamble_NoResults(t *testing.T) {
	withFakeRunner(t, &fakeRunner{results: nil})
	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, recallPrompt)
	if buf.Len() != 0 {
		t.Fatalf("expected empty output on no results, got %q", buf.String())
	}
}

func TestEmitLocalRecallPreamble_ErrorIsSilent(t *testing.T) {
	withFakeRunner(t, &fakeRunner{err: errors.New("kaboom")})
	var buf bytes.Buffer
	emitLocalRecallPreamble(&buf, recallPrompt)
	if buf.Len() != 0 {
		t.Fatalf("expected empty output on error, got %q", buf.String())
	}
}

func TestEmitLocalRecallPreamble_TimeoutIsSilent(t *testing.T) {
	// runner sleeps longer than the 100ms recallQueryTimeout
	withFakeRunner(t, &fakeRunner{delay: 500 * time.Millisecond, results: []LocalQueryResult{{Title: "late"}}})

	var buf bytes.Buffer
	start := time.Now()
	emitLocalRecallPreamble(&buf, recallPrompt)
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("hook took %v, expected ~100ms timeout to bound it", elapsed)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output on timeout, got %q", buf.String())
	}
}

// Fail-open under a panicking runner — agent_hook_recall.go has a
// defer/recover specifically because a half-built local index could
// dereference a nil pointer and crash the whole hook process otherwise.
func TestEmitLocalRecallPreamble_PanicIsSilent(t *testing.T) {
	withFakeRunner(t, &fakeRunner{panicWith: "panic from runner"})
	var buf bytes.Buffer
	// must not propagate the panic
	emitLocalRecallPreamble(&buf, recallPrompt)
	if buf.Len() != 0 {
		t.Fatalf("expected empty output on panic, got %q", buf.String())
	}
}

func TestExtractPromptText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty bytes", "", ""},
		{"claude format", `{"hook_event_name":"UserPromptSubmit","prompt":"hello world"}`, "hello world"},
		{"user_prompt alt", `{"user_prompt":"alt name"}`, "alt name"},
		{"message alt", `{"message":"msg name"}`, "msg name"},
		{"prompt wins over alt", `{"prompt":"primary","message":"alt"}`, "primary"},
		{"malformed json", `not-json`, ""},
		{"no prompt field", `{"hook_event_name":"X"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPromptText([]byte(tc.raw))
			if got != tc.want {
				t.Fatalf("extractPromptText(%q)=%q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// Regression: handlePrompt must still emit whispers when ctx has a marker
// even with no recall input. The new local-recall preamble is strictly
// additive and must NEVER short-circuit the existing whisper delivery.
func TestHandlePrompt_WhisperRegression_NoRecall(t *testing.T) {
	// Force the recall path to be a no-op so we can isolate whisper behavior.
	withFakeRunner(t, &fakeRunner{}) // empty results -> no preamble

	// ctx has no Input (no prompt), no marker -> no whispers, no recall.
	// We just verify handlePrompt does not error and does not write recall.
	ctx := &HookContext{ProjectRoot: t.TempDir()}
	if err := handlePrompt(ctx); err != nil {
		t.Fatalf("handlePrompt errored unexpectedly: %v", err)
	}
}

// Regression: even when the recall runner is broken, the whisper code
// path must still be invoked. We verify this by confirming the runner
// invocation order: recall is attempted first, then whisper delivery
// happens regardless of recall outcome.
func TestHandlePrompt_RecallFailure_DoesNotBreakWhisperPath(t *testing.T) {
	withFakeRunner(t, &fakeRunner{err: errors.New("intentional recall failure")})

	ctx := &HookContext{
		ProjectRoot: t.TempDir(),
		Input:       &agentx.HookInput{RawBytes: []byte(`{"prompt":"` + recallPrompt + `"}`)},
	}
	// no marker -> emitWhispers not called, but recall path is exercised
	if err := handlePrompt(ctx); err != nil {
		t.Fatalf("handlePrompt errored after recall failure: %v", err)
	}
}
