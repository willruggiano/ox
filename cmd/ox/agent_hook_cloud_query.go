package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/sageox/ox/internal/auth"
	"github.com/sageox/ox/internal/config"
	"github.com/sageox/ox/internal/endpoint"
	"github.com/sageox/ox/internal/session"
)

// CloudQueryDecision captures the resolved policy + the redacted prompt that
// MAY be transmitted to the SageOx endpoint. It is the public contract that
// ox-tshf's UserPromptSubmit handler consumes — every cloud query MUST go
// through PrepareCloudQuery so the redaction pipeline cannot be bypassed
// (this is a load-bearing invariant for ox-8wmo).
//
// Field semantics:
//   - ShouldQuery=false means "local-only path; do NOT send anything off
//     machine." Honor this regardless of any other flag.
//   - RedactedPrompt is the only string safe to transmit. Never substitute
//     the raw prompt back in even if RedactedTokens==0 (zero today does not
//     guarantee zero under a future pattern catalog update).
//   - RedactedTokens is for telemetry only — never log the raw matches.
//   - CloudQueryTimeout matches the local-query budget (100ms) so a slow
//     remote can never delay the prompt.
type CloudQueryDecision struct {
	ShouldQuery       bool
	RedactedPrompt    string
	RedactedTokens    int
	CloudQueryTimeout time.Duration
}

// cloudQueryTimeout mirrors the 100ms local-query budget. Defined as a var
// (not const) so tests can shorten it. NEVER bump above the local-query
// budget — the whole point of the gate is that a slow remote does not
// block the prompt.
var cloudQueryTimeout = 100 * time.Millisecond

// PrepareCloudQuery is the canonical entry point for ox-tshf's
// UserPromptSubmit handler to ask "may I send this prompt to the cloud,
// and if so what bytes are safe to transmit?"
//
// Behavior is intentionally fail-closed at every branch:
//
//  1. If hooks.userpromptsubmit.cloud_query=false (DEFAULT) → ShouldQuery=false.
//     No further work, no network calls, no redactor invocation. This is
//     the verifiable "zero network calls in default config" property.
//  2. If cloud_query=true but no auth token is present for the active
//     endpoint → silently degrade to ShouldQuery=false. The prompt path
//     never errors on missing auth (per ox-8wmo acceptance).
//  3. If cloud_query=true AND authenticated → run the prompt through
//     session.Redactor.RedactPrompt and return ShouldQuery=true with the
//     redacted text. Caller MUST transmit RedactedPrompt, never the raw
//     prompt, even when RedactedTokens==0.
//
// Errors are not returned — every failure mode degrades silently to "do
// not query." Surfacing an error here would risk poisoning the prompt
// path and is incompatible with the fail-open contract of the
// UserPromptSubmit handler (see ox-tshf).
func PrepareCloudQuery(ctx context.Context, projectRoot, prompt string) CloudQueryDecision {
	decision := CloudQueryDecision{CloudQueryTimeout: cloudQueryTimeout}

	// Caller-context observation: the UserPromptSubmit handler wraps this
	// call with a 100ms timeout (recallQueryTimeout). If the caller's
	// context is already done, or becomes done while redaction runs, we
	// MUST degrade to local-only rather than continue silently — the
	// whole point of the gate is that a slow remote (or slow redactor)
	// cannot block the prompt path.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return decision
	}

	// gate 1: opt-in. Default false means no network calls ever.
	if !config.ResolveUserPromptSubmitCloudQuery(projectRoot) {
		return decision
	}

	// gate 2: authentication. Local-only credential check (no network
	// round-trip) to keep the prompt-path fast. A token that exists but
	// fails server-side validation will be discovered when the cloud
	// query itself runs and 401s; we degrade then by simply ignoring the
	// remote result.
	ep := endpoint.GetForProject(projectRoot)
	if ep == "" {
		ep = endpoint.Get()
	}
	ok, err := auth.IsAuthCredentialValidForEndpoint(ep)
	if err != nil || !ok {
		slog.Debug("cloud-query: degrading to local-only (no auth)", "endpoint", ep, "err", err)
		return decision
	}

	// gate 3: redaction. ALWAYS runs before transmit — this is the
	// load-bearing invariant. If RedactPrompt panics or otherwise fails,
	// fail-closed via the recover() below.
	//
	// Run redaction off-goroutine and race it against ctx.Done() so a
	// pathological pattern that takes longer than the caller's budget
	// cannot wedge the prompt path. Goroutine leaks are bounded: even if
	// the redactor never returns, only one goroutine per prompt leaks and
	// the channel is buffered so the goroutine can always exit cleanly.
	type redactResult struct {
		redacted string
		count    int
	}
	resultCh := make(chan redactResult, 1)
	go func() {
		r, c := redactPromptSafe(prompt)
		resultCh <- redactResult{redacted: r, count: c}
	}()

	var redacted string
	var count int
	select {
	case <-ctx.Done():
		slog.Debug("cloud-query: context done before redaction completed, degrading", "err", ctx.Err())
		return decision
	case res := <-resultCh:
		redacted, count = res.redacted, res.count
	}

	if redacted == "" && prompt != "" {
		// defensive: redactor returned empty for non-empty input —
		// treat as redaction failure and degrade
		slog.Debug("cloud-query: redactor returned empty, degrading")
		return decision
	}

	slog.Info("cloud-query: enabled", "redacted_token_count", count, "endpoint", ep)

	decision.ShouldQuery = true
	decision.RedactedPrompt = redacted
	decision.RedactedTokens = count
	return decision
}

// redactPromptSafe wraps RedactPrompt with a panic recovery so a bad
// pattern can never blow up the prompt path. On panic, returns ("", 0)
// which PrepareCloudQuery treats as a redaction failure → no query.
func redactPromptSafe(prompt string) (redacted string, count int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("cloud-query: redactor panicked, failing closed", "panic", r)
			redacted = ""
			count = 0
		}
	}()
	redactor := session.NewRedactor()
	return redactor.RedactPrompt(prompt)
}
