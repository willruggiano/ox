package symbols

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolve_BasicSameFile covers ADR-019 phase 1: a unique same-file
// definition produces one "inferred" edge from the containing symbol.
func TestResolve_BasicSameFile(t *testing.T) {
	syms := []Symbol{
		{Name: "helper", Kind: "function"},    // idx 0
		{Name: "caller", Kind: "function"},    // idx 1
		{Name: "unrelated", Kind: "function"}, // idx 2
	}
	refs := []Ref{
		{RefName: "helper", Kind: "call", Line: 10, Col: 5, ContainingSymIdx: 1}, // caller → helper
	}

	edges := Resolve(syms, refs)
	require.Len(t, edges, 1)
	require.Equal(t, 1, edges[0].SrcIdx)
	require.Equal(t, 0, edges[0].DstIdx)
	require.Equal(t, "helper", edges[0].DstName)
	require.Equal(t, "call", edges[0].Kind)
	require.Equal(t, "inferred", edges[0].Confidence)
	require.Equal(t, 10, edges[0].Line)
}

// TestResolve_AmbiguousFansOut covers the ambiguity policy: multiple same-file
// matches produce one edge per candidate, marked "ambiguous", capped.
func TestResolve_AmbiguousFansOut(t *testing.T) {
	syms := []Symbol{
		{Name: "Process", Kind: "function"}, // idx 0
		{Name: "Process", Kind: "method"},   // idx 1
		{Name: "Process", Kind: "function"}, // idx 2
		{Name: "caller", Kind: "function"},  // idx 3
	}
	refs := []Ref{
		{RefName: "Process", Kind: "call", Line: 7, ContainingSymIdx: 3},
	}

	edges := Resolve(syms, refs)
	require.Len(t, edges, 3, "one edge per same-name candidate")
	for _, e := range edges {
		require.Equal(t, "ambiguous", e.Confidence)
		require.Equal(t, 3, e.SrcIdx)
	}
}

// TestResolve_AmbiguityCapped guards the ambiguousEdgeCap so a pathological
// collision (e.g. 600+ defs of "string" in the wild) can't blow up the table.
func TestResolve_AmbiguityCapped(t *testing.T) {
	syms := make([]Symbol, 0, 20)
	for i := 0; i < 20; i++ {
		syms = append(syms, Symbol{Name: "string", Kind: "function"})
	}
	syms = append(syms, Symbol{Name: "caller", Kind: "function"})
	refs := []Ref{
		{RefName: "string", Kind: "reference", ContainingSymIdx: len(syms) - 1},
	}

	edges := Resolve(syms, refs)
	require.Len(t, edges, ambiguousEdgeCap,
		"ambiguous edge fan-out must be capped at ambiguousEdgeCap")
}

// TestResolve_FileScopeRefsDropped covers the "no containing symbol" case —
// file-scope refs have no caller to attribute the edge to. They stay in
// symbol_refs for the name-fallback path; they don't produce edges.
func TestResolve_FileScopeRefsDropped(t *testing.T) {
	syms := []Symbol{{Name: "Init", Kind: "function"}}
	refs := []Ref{
		{RefName: "Init", Kind: "call", ContainingSymIdx: -1}, // file scope
	}
	require.Nil(t, Resolve(syms, refs))
}

// TestResolve_UnknownNameProducesNoEdge — refs whose name doesn't match any
// same-file symbol leave no edge (they fall through to the name-match path).
func TestResolve_UnknownNameProducesNoEdge(t *testing.T) {
	syms := []Symbol{{Name: "helper", Kind: "function"}, {Name: "caller", Kind: "function"}}
	refs := []Ref{
		{RefName: "externalFunc", Kind: "call", ContainingSymIdx: 1},
	}
	require.Empty(t, Resolve(syms, refs))
}

// TestResolve_EmptyInputs — defensive zero-value behavior.
func TestResolve_EmptyInputs(t *testing.T) {
	require.Nil(t, Resolve(nil, nil))
	require.Nil(t, Resolve([]Symbol{{Name: "x"}}, nil))
	require.Nil(t, Resolve(nil, []Ref{{RefName: "x", ContainingSymIdx: 0}}))
}
