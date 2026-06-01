//go:build memprofile

package index

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/stretchr/testify/require"
)

// TestDiffSizeDistribution opens an existing on-disk diff Bleve index
// (default-stored content) and reports the distribution of diff text sizes.
// Drives ADR-018 open question 1: keep diff stored, regenerate from blobs, or
// store a truncated `text_head` in SQLite — the right call depends on the size
// distribution and the cost of truncation.
//
//	CODEDB_DIFF_INDEX=/path/to/codedb/bleve/diff \
//	go test -tags=memprofile -run TestDiffSizeDistribution -v ./internal/codedb/index/
func TestDiffSizeDistribution(t *testing.T) {
	path := os.Getenv("CODEDB_DIFF_INDEX")
	if path == "" {
		t.Skip("set CODEDB_DIFF_INDEX to a path like <codedb>/bleve/diff")
	}

	idx, err := bleve.Open(path)
	require.NoError(t, err, "open diff bleve")
	defer idx.Close()

	docCount, err := idx.DocCount()
	require.NoError(t, err)
	t.Logf("diff index: %d docs at %s", docCount, path)

	// Page through all docs collecting content sizes. Pagination keeps peak
	// memory bounded (the full set could be 100s of MB on big repos).
	const pageSize = 2000
	var sizes []int
	var total int64
	from := 0
	for {
		req := bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), pageSize, from, false)
		req.Fields = []string{"content"}
		res, err := idx.Search(req)
		require.NoError(t, err)
		if len(res.Hits) == 0 {
			break
		}
		for _, hit := range res.Hits {
			if c, ok := hit.Fields["content"].(string); ok {
				sizes = append(sizes, len(c))
				total += int64(len(c))
			}
		}
		if len(res.Hits) < pageSize {
			break
		}
		from += pageSize
	}

	n := len(sizes)
	require.Greater(t, n, 0, "expected at least one diff doc with content field")
	sort.Ints(sizes)
	pct := func(q float64) int {
		if n == 0 {
			return 0
		}
		i := int(float64(n-1) * q)
		return sizes[i]
	}

	t.Logf("size distribution (bytes): count=%d  total=%.1f MB  mean=%d  min=%d  p50=%d  p90=%d  p95=%d  p99=%d  max=%d",
		n, mb(uint64(total)), int(total)/n, sizes[0], pct(0.5), pct(0.9), pct(0.95), pct(0.99), sizes[n-1])

	t.Logf("Option B (full diff text in SQLite): %.1f MB", mb(uint64(total)))

	t.Logf("Option C (text_head cap → SQLite + Bleve index-only):")
	t.Logf("  cap    SQLite_MB   covered_full  truncated  truncation_loss_MB")
	for _, cap := range []int{1024, 2048, 4096, 8192, 16384} {
		var sum int64
		covered := 0
		for _, sz := range sizes {
			if sz > cap {
				sum += int64(cap)
			} else {
				sum += int64(sz)
				covered++
			}
		}
		lossMB := mb(uint64(total)) - mb(uint64(sum))
		t.Logf("  %5d  %9.1f  %7d/%-7d  %7d   %6.1f",
			cap, mb(uint64(sum)), covered, n, n-covered, lossMB)
	}
}

// TestDiffStoredVsIndexOnly rebuilds the same diff corpus into two fresh Bleve
// indexes — one with the default (stored) mapping and one with the ADR-018
// phase 2 index-only mapping — and reports the actual on-disk saving. Drives
// OQ1's quantitative case by replacing the extrapolated estimate from the
// code-only TestBleveStoredContentCost with a measurement on real diff text.
//
//	CODEDB_DIFF_INDEX=/path/to/codedb/bleve/diff \
//	go test -tags=memprofile -run TestDiffStoredVsIndexOnly -v -timeout 30m ./internal/codedb/index/
func TestDiffStoredVsIndexOnly(t *testing.T) {
	path := os.Getenv("CODEDB_DIFF_INDEX")
	if path == "" {
		t.Skip("set CODEDB_DIFF_INDEX to a path like <codedb>/bleve/diff")
	}

	src, err := bleve.Open(path)
	require.NoError(t, err, "open source diff bleve")
	defer src.Close()

	// Collect (id, content) pairs from the source. We hold everything in memory
	// for indexing speed; OK because the harness is opt-in and the source caps
	// at ~hundreds of MB.
	type doc struct {
		id      string
		content string
	}
	var docs []doc
	const pageSize = 2000
	from := 0
	for {
		req := bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), pageSize, from, false)
		req.Fields = []string{"content"}
		res, err := src.Search(req)
		require.NoError(t, err)
		if len(res.Hits) == 0 {
			break
		}
		for _, hit := range res.Hits {
			if c, ok := hit.Fields["content"].(string); ok {
				docs = append(docs, doc{id: hit.ID, content: c})
			}
		}
		if len(res.Hits) < pageSize {
			break
		}
		from += pageSize
	}
	t.Logf("corpus: %d diff docs", len(docs))

	buildIdx := func(name string, m mapping.IndexMapping) uint64 {
		dir := filepath.Join(t.TempDir(), name)
		idx, err := bleve.New(dir, m)
		require.NoError(t, err)
		batch := idx.NewBatch()
		for i, d := range docs {
			require.NoError(t, batch.Index(d.id, BleveDiffDoc{Content: d.content}))
			if (i+1)%500 == 0 {
				require.NoError(t, idx.Batch(batch))
				batch = idx.NewBatch()
			}
		}
		require.NoError(t, idx.Batch(batch))
		require.NoError(t, idx.Close())
		return dirSize(dir)
	}

	storedSize := buildIdx("stored", bleve.NewIndexMapping())
	require.Greater(t, storedSize, uint64(0),
		"stored baseline must be > 0 — otherwise the saving% log emits Inf/NaN, not actionable output")

	ft := bleve.NewTextFieldMapping()
	ft.Store = false
	ft.IncludeTermVectors = false
	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("content", ft)
	mPhase2 := bleve.NewIndexMapping()
	mPhase2.DefaultMapping = dm
	indexOnlySize := buildIdx("indexonly", mPhase2)

	saving := 100 * (1.0 - float64(indexOnlySize)/float64(storedSize))
	t.Logf("BLEVE DISK: stored=%.1f MB  index-only=%.1f MB  saving=%.1f%% (%.1f MB)",
		mb(storedSize), mb(indexOnlySize), saving, mb(storedSize)-mb(indexOnlySize))
}
