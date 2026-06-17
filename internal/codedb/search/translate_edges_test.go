package search

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfidenceParse covers the `confidence:` DSL token added for ADR-019.
// Valid values land in Filters.Confidence; invalid values error.
func TestConfidenceParse(t *testing.T) {
	tests := []struct {
		input   string
		wantVal string
		wantErr bool
	}{
		{"calls:Foo confidence:extracted", "extracted", false},
		{"calls:Foo confidence:inferred", "inferred", false},
		{"calls:Foo confidence:ambiguous", "ambiguous", false},
		{"calls:Foo confidence:bogus", "", true},
		{"calls:Foo", "", false}, // omitted — empty (legacy path)
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			q, err := ParseQuery(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantVal, q.Filters.Confidence)
		})
	}
}

// TestTranslateCallers_RoutesByConfidence proves that `confidence:` flips the
// translator from the symbol_refs name-match path (legacy) to the resolved
// symbol_edges path (ADR-019). Failure here means the read swap regressed.
func TestTranslateCallers_RoutesByConfidence(t *testing.T) {
	t.Run("no confidence -> symbol_refs", func(t *testing.T) {
		q, err := ParseQuery("calls:Foo")
		require.NoError(t, err)
		tr, err := Translate(q)
		require.NoError(t, err)
		require.Contains(t, tr.SQL, "FROM symbol_refs sr",
			"legacy path must use symbol_refs when no confidence: filter")
		require.NotContains(t, tr.SQL, "symbol_edges")
	})
	t.Run("confidence set -> symbol_edges", func(t *testing.T) {
		q, err := ParseQuery("calls:Foo confidence:inferred")
		require.NoError(t, err)
		tr, err := Translate(q)
		require.NoError(t, err)
		require.Contains(t, tr.SQL, "FROM symbol_edges e",
			"confidence: filter must route to the resolved symbol_edges path")
		require.Contains(t, tr.SQL, "e.kind = 'call'")
		require.Contains(t, tr.SQL, "e.confidence IN",
			"confidence threshold must constrain the edge set")
	})
}

// TestTranslateCalledBy_RoutesByConfidence — same dispatch for `calledby:`.
func TestTranslateCalledBy_RoutesByConfidence(t *testing.T) {
	q, err := ParseQuery("calledby:Foo confidence:extracted")
	require.NoError(t, err)
	tr, err := Translate(q)
	require.NoError(t, err)
	require.Contains(t, tr.SQL, "symbol_edges")
	require.Contains(t, tr.SQL, "s_dst")
}

// TestConfidenceClause_ThresholdSemantics verifies the minimum-threshold
// expansion: "inferred" → extracted+inferred (excludes ambiguous);
// "ambiguous" → all three; "extracted" → only extracted; invalid → empty.
func TestConfidenceClause_ThresholdSemantics(t *testing.T) {
	tests := []struct {
		level     string
		wantCount int // number of confidence values in the IN clause
	}{
		{"extracted", 1},
		{"inferred", 2},
		{"ambiguous", 3},
		{"", 0},
		{"bogus", 0},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			p := newParamCollector(false)
			clause := confidenceClause(p, tt.level)
			if tt.wantCount == 0 {
				require.Equal(t, "", clause)
				return
			}
			require.True(t, strings.HasPrefix(clause, "AND e.confidence IN ("),
				"clause shape: %q", clause)
			// param count == confidence values in IN clause
			require.Len(t, p.params, tt.wantCount)
		})
	}
}

// TestTranslateCallers_RecursiveUsesEdgesWithConfidence — depth>1 with
// confidence must use the resolved-edges recursive CTE (joins on
// dst_symbol_id ↔ src_symbol_id, NOT on ref_name strings).
func TestTranslateCallers_RecursiveUsesEdgesWithConfidence(t *testing.T) {
	q, err := ParseQuery("calls:Foo confidence:inferred depth:3")
	require.NoError(t, err)
	tr, err := Translate(q)
	require.NoError(t, err)
	require.Contains(t, tr.SQL, "WITH RECURSIVE call_chain")
	require.Contains(t, tr.SQL, "symbol_edges")
	require.Contains(t, tr.SQL, "e2.dst_symbol_id = cc.symbol_id",
		"recursion must join on resolved ids, not on names")
	require.NotContains(t, tr.SQL, "sr2.ref_name = cc.name",
		"recursive symbol_refs name-equality must NOT be in the edges path")
}
