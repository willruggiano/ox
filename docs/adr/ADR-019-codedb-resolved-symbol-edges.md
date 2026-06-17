<!-- doc-audience: ai -->
# ADR-019: Resolved Symbol Edges for CodeDB

**Status**: Proposed (needs Ryan — new data structure + changes call-graph search semantics)
**Date**: 2026-05-27

## Context

CodeDB's "call graph" today is a **name-string match**, not a resolved edge
set. `symbol_refs(symbol_id, ref_name, kind, blob_id, line, col)` records the
*containing* symbol (where a call appears) and the *name* being called as a
string. The `calls:` / `calledby:` query DSL joins via `LIKE '%name%'` or
`ref_name = name` against `symbols.name` (`internal/codedb/search/translate.go`).

There is no resolved `src → dst` edge. Two consequences:

1. **Cross-file ambiguity.** Any name shared by multiple definitions
   conflates them. Measured on the biggest live codedb (`repo_019c6d2e`,
   40,250 symbols / 185,912 refs):

   ```
   top function-name collisions (defs per name):
     string  640   bool    371   error   248   int     72   main   71
     new      69   encode   60   decode   59   from    53   Register 33

   ambiguous call refs (name shared by >1 function): 18.7% of 185,912
   ```

   Nearly one in five call references resolves to >1 candidate. `calls:Register`
   returns hits from 33 unrelated definitions of `Register`.

2. **No graph algorithms possible.** Without resolved edges there is no
   well-formed directed graph to run centrality on, no shortest-path queries,
   no community detection. Every graphify-style feature we want next
   (`ox code path`, "god nodes" insights, architectural digest, module
   communities) is blocked on this.

[graphify](https://github.com/safishamsi/graphify) builds resolved, typed
edges with confidence scoring and runs Leiden community detection + centrality
+ shortest path on the result. It uses tree-sitter for code (same library
codedb uses via `gotreesitter`) plus LLM extraction for docs (out-of-scope per
the project's "deterministic only" call from the earlier session). Resolved
edges is the deterministic core that's portable.

## Decision

Add a **resolved-edge table** populated at index time by per-language
resolvers. Replace the call-graph queries' name-match joins with index lookups
on this table. Phase the rollout: same-file Go first (validates the model),
then cross-file Go, then other languages.

### Schema (`symbol_edges`)

```sql
CREATE TABLE symbol_edges (
    id              INTEGER PRIMARY KEY,
    src_blob_id     INTEGER NOT NULL REFERENCES blobs(id),
    src_symbol_id   INTEGER NOT NULL REFERENCES symbols(id),  -- containing symbol
    dst_blob_id     INTEGER          REFERENCES blobs(id),    -- nullable: external/stdlib
    dst_symbol_id   INTEGER          REFERENCES symbols(id),  -- nullable: external/unresolved
    dst_name        TEXT    NOT NULL,                         -- always known (the referenced name)
    kind            TEXT    NOT NULL,                         -- call | reference | implements | inherits | imports
    confidence      TEXT    NOT NULL,                         -- extracted | inferred | ambiguous
    line            INTEGER NOT NULL,
    col             INTEGER NOT NULL
);

CREATE INDEX idx_symbol_edges_src ON symbol_edges(src_symbol_id, kind);
CREATE INDEX idx_symbol_edges_dst ON symbol_edges(dst_symbol_id, kind);
CREATE INDEX idx_symbol_edges_dst_name ON symbol_edges(dst_name, kind);
```

Edges are **populated alongside `symbol_refs`** (which we keep for the
unresolved-name path used by other queries; resolved edges are an additional
layer, not a replacement). When a `symbol_refs` row can be resolved, an edge
is inserted with the appropriate confidence; when it can't, the `symbol_refs`
row alone records the unresolved name as today.

### Confidence model

| Level | Meaning |
|---|---|
| `extracted` | Tree-sitter + scope rules give a single definitive target (e.g., same-file private function called by an enclosing method) |
| `inferred` | Best-guess via import graph + package symbol table; unique target after scope resolution |
| `ambiguous` | Multiple plausible targets after resolution; we emit one edge per candidate with `ambiguous` so agents can see the spread |

Edges with `confidence=ambiguous` cap fan-out at a small constant (e.g., 8
candidates) to bound storage on pathological names.

### Per-language resolver phases

Resolver shape: given a blob's symbols + refs (output of `symbols.Extract`),
produce a list of `(src_symbol_id, dst_blob_id?, dst_symbol_id?, kind, confidence)`.

**Phase 1 — same-file scope (all 8 supported languages).** For each ref, walk
up the containing-symbol tree; if a same-file symbol with matching name is in
scope, emit an `extracted` edge. Otherwise leave unresolved. Cheap, language-
agnostic, doesn't need import graphs. Catches private helpers, local methods.

**Phase 2 — Go cross-file resolver.** Build a per-package symbol table from
the indexed blobs (Go is uniform: package directory = package, exported = uppercase,
qualified vs unqualified call distinguishable). For unresolved refs, look up
the referenced name in the same package's symbol table (`inferred`). Qualified
calls (`pkg.Func`) join to the package symbol table via the import path.

**Phase 3 — Other languages.** Python (per-file imports), TypeScript/JavaScript
(module imports), Rust (use statements + mod tree), C/C++ (per-file scope +
includes, deferred — preprocessor is hard).

Resolvers are pluggable per language (interface satisfied per-language) — Go
ships first; others added incrementally without touching the storage layer.

### Search read-path change

`calls:Name` and `calledby:Name` switch from name-match to edge lookup:

```sql
-- calls:Name (callers of Name) — today: LIKE '%Name%' join
-- proposed: index lookup on dst_name + dst_symbol_id resolution
SELECT DISTINCT s_src.name, s_src.kind, fr.path
FROM symbol_edges e
JOIN symbols s_src ON s_src.id = e.src_symbol_id
JOIN blobs b ON b.id = e.src_blob_id
JOIN file_revs fr ON fr.blob_id = b.id
WHERE e.dst_name = ? AND e.kind = 'call'
```

Multi-hop (`depth:N`) becomes a clean recursive CTE on `src_symbol_id ↔
dst_symbol_id` instead of the current `LIKE`-bounded recursion.

A query-time filter `confidence:extracted` (or `confidence:>=inferred`) lets
agents constrain by certainty when precision matters.

### Reindex / versioning

Add `.needs_reindex_symbols` marker semantics analogous to ADR-018's bleve
marker. When this ADR ships, existing codedbs need symbols re-parsed to
populate `symbol_edges`. The `blobs.parsed` flag already exists — bump its
meaning to "parsed at edge-resolver version N" via a small `blobs.edge_version`
column, defaulting 0; resolver only processes rows where
`edge_version < current_version`.

## Consequences

**Positive**
- **Precision**: `calls:Foo` returns just the callers of *this* `Foo`, not the
  18.7% spread we have today. Agents get correct call graphs.
- **Unlocks the graphify port**: centrality (#2), shortest-path (#3),
  architectural digest (#4), communities (#7) all depend on having a real
  edge set.
- **Fixes a slow query**: `LIKE '%name%'` in the call-graph CTE can't use an
  index; the new lookup uses `idx_symbol_edges_dst_name`.

**Negative / risk**
- **New write-path cost**: per-blob resolver runs after symbol extraction.
  Same-file phase is cheap (in-memory scope walk). Cross-file phase requires
  a per-package symbol-table build at index time.
- **Storage**: edges roughly proportional to `symbol_refs` (≤ same count;
  fewer when unresolved refs dropped). On the 186k-ref repo, expect ~150k
  edges → ~10–15 MB SQLite (with the three indexes).
- **Data-access ergonomics change**: search results for `calls:` and
  `calledby:` change semantics (correctly, but visibly). New `confidence`
  filter is additive.
- **Reindex required** on existing codedbs to populate edges.

## Alternatives considered

1. **Query-time resolution** (compute edges in the search planner from
   `symbol_refs` + scope tables on every query). Avoids storage, but per-query
   cost is high and rules out graph algorithms (need persistent edges for
   centrality/communities). Rejected.
2. **LLM-based resolution** (graphify's approach for non-tree-sitter content).
   Conflicts with the deterministic-only call from earlier session. Rejected.
3. **Keep status quo + add confidence as a hint** (don't resolve, just flag
   names that have >1 definition). Improves agent awareness but doesn't fix
   precision and doesn't unlock graph algorithms. Rejected.

## Phasing

| Phase | Scope | Why first / why later |
|---|---|---|
| **1** | Schema + same-file resolver (all 8 langs) + new search SQL + tests | Validates the model end-to-end on a cheap resolver; all 8 langs trivially because containment scope is language-agnostic at tree-sitter level |
| **2** | Go cross-file resolver (package symbol tables) | Go is uniform/strict (good first non-trivial case); also our own codebase, easy to validate qualitatively |
| **3** | Centrality / `god nodes` (graphify #2), `ox code path` (graphify #3), digest (graphify #4) | Built on the now-real edge set |
| **4** | Per-language resolvers for Python / TS / Rust / Java | Each is a focused, isolated addition once the framework is proven |
| **5** | Leiden community detection (graphify #7) | Defer until centrality/path prove the edge set is useful |

## Open questions for review

1. **Edges as a table vs. denormalized into `symbol_refs`?** Adding columns
   to `symbol_refs` (e.g., `dst_symbol_id`, `confidence`) would avoid a join
   but couples the resolved-edge concept to the existing schema. Proposed: a
   separate `symbol_edges` table — clearer model, easier rollback.
2. **Should `kind` distinguish `call` vs `reference` more finely** (e.g.,
   include `read` vs `write` vs `call`)? Today's `symbol_refs.kind` has
   `call`/`reference`/`definition`; proposed: edges carry the same vocabulary
   plus `implements` / `inherits` / `imports` over time.
3. **Ambiguity cap.** 8 candidates per ambiguous ref seems right for the
   biggest collisions (`string`→640 defs would otherwise blow up the table).
   Confirm or pick a different cap.
4. **Edge-version bump strategy.** Same `.needs_reindex_<name>` pattern from
   ADR-018, OR a separate per-blob version column? Proposed: per-blob column
   (`blobs.edge_version`) — fine-grained, incremental.
5. **Search-result schema.** Should `calls:`/`calledby:` results carry the
   `confidence` field (so agents can filter or weight)? Proposed: yes,
   plumbed through.
