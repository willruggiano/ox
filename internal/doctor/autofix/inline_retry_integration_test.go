package autofix

import (
	"context"
	"os"
	"testing"
)

// TestCheckSessionInlineSummaryRetry_LiveLedger runs the autofix against a
// real local ledger to prove hydration + reset works end-to-end.
//
// Gated behind OX_LIVE_LEDGER_ROOT so this never runs in CI and never hits a
// developer's primary repo by accident. To run locally:
//
//	OX_LIVE_LEDGER_ROOT=/path/to/initialized/repo go test ./internal/doctor/autofix -run LiveLedger -count=1
func TestCheckSessionInlineSummaryRetry_LiveLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("short: live ledger integration test")
	}
	ledgerRoot := os.Getenv("OX_LIVE_LEDGER_ROOT")
	if ledgerRoot == "" {
		t.Skip("set OX_LIVE_LEDGER_ROOT to run live-ledger integration test")
	}

	result := checkSessionInlineSummaryRetry(context.Background(), ledgerRoot)
	t.Logf("Status: %d", result.Status)
	t.Logf("Summary: %s", result.Summary)
	t.Logf("Repo: %s", result.Repo)

	// We expect it to find and reset sessions
	if result.Status == StatusError {
		t.Fatalf("autofix returned error: %s", result.Summary)
	}
}
