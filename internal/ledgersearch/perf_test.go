package ledgersearch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkSearch_SyntheticLedger generates a realistic-sized ledger
// (250 sessions, ~200 lines of markdown each) and measures Search latency.
// The acceptance criterion for ox-m01h is <50ms p95 warm-cache.
func BenchmarkSearch_SyntheticLedger(b *testing.B) {
	dir := b.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		b.Fatal(err)
	}
	// generate 250 session folders
	for i := 0; i < 250; i++ {
		ts := time.Now().Add(-time.Duration(i) * time.Hour)
		name := fmt.Sprintf("%s-ryan-Ox%04X", ts.UTC().Format("2006-01-02T15-04"), i)
		sd := filepath.Join(sessionsDir, name)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			b.Fatal(err)
		}
		// each summary ~2KB — mirrors observed real-world summary.md sizes
		content := fmt.Sprintf("# Session %d\n\nWorked on cache invalidation, OAuth, deployment pipeline.\nSearched for widget interactions.\n", i)
		for k := 0; k < 20; k++ {
			content += "Additional notes and detail to pad realistic file size. The system handled this gracefully.\n"
		}
		if err := os.WriteFile(filepath.Join(sd, "summary.md"), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Search(Options{LedgerPath: dir, Query: "widget oauth", Limit: 5})
		if err != nil {
			b.Fatal(err)
		}
	}
}
