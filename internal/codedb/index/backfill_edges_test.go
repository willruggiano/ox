package index

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sageox/ox/internal/codedb/store"
	"github.com/sageox/ox/internal/codedb/symbols"
)

// TestBackfillSymbolEdges_PopulatesEdgesAndBumpsVersion seeds the SQLite half
// of a store with a parsed blob (edge_version=0) plus its symbols+refs, runs
// BackfillSymbolEdges, and verifies: (a) an edge was inserted with the
// expected src/dst/confidence, (b) edge_version was bumped to current, (c) a
// second backfill is a no-op (idempotent).
func TestBackfillSymbolEdges_PopulatesEdgesAndBumpsVersion(t *testing.T) {
	s, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Seed one repo + one commit + one blob (parsed=1, edge_version=0).
	_, err = s.Exec("INSERT INTO repos (id, name, path) VALUES (1, 'test', '/tmp/test')")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO commits (id, repo_id, hash) VALUES (1, 1, 'abc')")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO blobs (id, content_hash, language, parsed, edge_version) VALUES (1, 'h1', 'go', 1, 0)")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO file_revs (id, commit_id, path, blob_id) VALUES (1, 1, 'main.go', 1)")
	require.NoError(t, err)

	// Two symbols: helper (callee, id=10) and caller (id=11).
	_, err = s.Exec("INSERT INTO symbols (id, blob_id, name, kind, line, col) VALUES (10, 1, 'helper', 'function', 1, 1)")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO symbols (id, blob_id, name, kind, line, col) VALUES (11, 1, 'caller', 'function', 5, 1)")
	require.NoError(t, err)

	// A call ref from caller (containing symbol_id=11) to "helper".
	_, err = s.Exec("INSERT INTO symbol_refs (blob_id, symbol_id, ref_name, kind, line, col) VALUES (1, 11, 'helper', 'call', 6, 5)")
	require.NoError(t, err)

	// Sanity: edge_version starts at 0, no edges yet.
	var ver int
	require.NoError(t, s.QueryRow("SELECT edge_version FROM blobs WHERE id=1").Scan(&ver))
	require.Equal(t, 0, ver)
	var edges int
	require.NoError(t, s.QueryRow("SELECT COUNT(*) FROM symbol_edges WHERE src_blob_id=1").Scan(&edges))
	require.Equal(t, 0, edges)

	// Run backfill.
	stats, err := BackfillSymbolEdges(context.Background(), s, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.BlobsProcessed)
	require.Equal(t, uint64(1), stats.EdgesInserted)

	// Verify the edge: src=11 (caller), dst=10 (helper), inferred (single same-file match).
	var srcID, dstID int64
	var dstName, kind, confidence string
	require.NoError(t, s.QueryRow(
		`SELECT src_symbol_id, dst_symbol_id, dst_name, kind, confidence
		 FROM symbol_edges WHERE src_blob_id=1`,
	).Scan(&srcID, &dstID, &dstName, &kind, &confidence))
	require.Equal(t, int64(11), srcID)
	require.Equal(t, int64(10), dstID)
	require.Equal(t, "helper", dstName)
	require.Equal(t, "call", kind)
	require.Equal(t, "inferred", confidence)

	// edge_version bumped to current.
	require.NoError(t, s.QueryRow("SELECT edge_version FROM blobs WHERE id=1").Scan(&ver))
	require.Equal(t, symbols.ResolverVersion, ver)

	// Idempotent: re-run is a no-op.
	stats2, err := BackfillSymbolEdges(context.Background(), s, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(0), stats2.BlobsProcessed,
		"second backfill must skip blobs already at current resolver version")
}

// TestBackfillSymbolEdges_AmbiguousFansOut verifies the resolver's ambiguity
// policy survives the SQL round-trip: a name shared by multiple same-file
// symbols produces one "ambiguous" edge per candidate.
func TestBackfillSymbolEdges_AmbiguousFansOut(t *testing.T) {
	s, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Exec("INSERT INTO repos (id, name, path) VALUES (1, 't', '/tmp')")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO commits (id, repo_id, hash) VALUES (1, 1, 'abc')")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO blobs (id, content_hash, language, parsed, edge_version) VALUES (1, 'h', 'go', 1, 0)")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO file_revs (id, commit_id, path, blob_id) VALUES (1, 1, 'm.go', 1)")
	require.NoError(t, err)
	// Three same-name targets + one caller.
	for i, id := range []int{1, 2, 3} {
		_, err = s.Exec("INSERT INTO symbols (id, blob_id, name, kind, line, col) VALUES (?, 1, 'Process', 'function', ?, 1)", id, i+1)
		require.NoError(t, err)
	}
	_, err = s.Exec("INSERT INTO symbols (id, blob_id, name, kind, line, col) VALUES (100, 1, 'caller', 'function', 10, 1)")
	require.NoError(t, err)
	_, err = s.Exec("INSERT INTO symbol_refs (blob_id, symbol_id, ref_name, kind, line, col) VALUES (1, 100, 'Process', 'call', 11, 5)")
	require.NoError(t, err)

	stats, err := BackfillSymbolEdges(context.Background(), s, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(3), stats.EdgesInserted, "one edge per same-name candidate")

	var n int
	require.NoError(t, s.QueryRow(
		"SELECT COUNT(*) FROM symbol_edges WHERE src_blob_id=1 AND confidence='ambiguous'",
	).Scan(&n))
	require.Equal(t, 3, n)
}
