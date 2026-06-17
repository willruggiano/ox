package symbols

// Edge is a resolved reference from a containing source symbol to a target
// symbol identified by its index in the syms slice. ADR-019 phase 1 resolves
// only within the same file (the syms slice is one blob's worth); cross-file
// resolution comes in later phases with per-language import/scope handling.
type Edge struct {
	SrcIdx     int    // index in syms — the containing symbol (caller)
	DstIdx     int    // index in syms — the resolved target (callee)
	DstName    string // the referenced name, always populated
	Kind       string // "call" | "reference" (from the underlying Ref.Kind)
	Confidence string // "inferred" | "ambiguous" — see ADR-019 confidence model
	Line       int
	Col        int
}

// ambiguousEdgeCap bounds how many candidate edges we emit for a single
// ambiguous reference, so pathological collisions (e.g. a name shared by
// hundreds of definitions) don't blow up the edges table.
const ambiguousEdgeCap = 8

// ResolverVersion is the structural version of Resolve's output. Bump when
// changing the resolver semantics (new edge kinds, scope rules, confidence
// policy). ParseSymbols stamps `blobs.edge_version` at this version after
// inserting edges; BackfillSymbolEdges processes blobs whose stored version
// is below this one.
const ResolverVersion = 1

// Resolve produces same-file edges from refs to in-scope symbols (ADR-019
// phase 1). Drops refs that aren't inside any containing symbol (file-scope
// refs have no meaningful "caller"); drops refs whose name doesn't match any
// same-file symbol (they remain in symbol_refs for the name-fallback path).
//
// Confidence policy: one same-file match → "inferred" (heuristic but high
// signal in most languages); multiple matches → "ambiguous", emitted as one
// edge per candidate up to ambiguousEdgeCap. Reserve "extracted" for later
// phases that consult the language's actual scope/import rules.
func Resolve(syms []Symbol, refs []Ref) []Edge {
	if len(syms) == 0 || len(refs) == 0 {
		return nil
	}
	byName := make(map[string][]int, len(syms))
	for i := range syms {
		byName[syms[i].Name] = append(byName[syms[i].Name], i)
	}

	var edges []Edge
	for i := range refs {
		ref := &refs[i]
		if ref.ContainingSymIdx < 0 {
			continue
		}
		candidates, ok := byName[ref.RefName]
		if !ok {
			continue
		}
		confidence := "inferred"
		if len(candidates) > 1 {
			confidence = "ambiguous"
		}
		n := min(len(candidates), ambiguousEdgeCap)
		for k := range n {
			edges = append(edges, Edge{
				SrcIdx:     ref.ContainingSymIdx,
				DstIdx:     candidates[k],
				DstName:    ref.RefName,
				Kind:       ref.Kind,
				Confidence: confidence,
				Line:       ref.Line,
				Col:        ref.Col,
			})
		}
	}
	return edges
}
