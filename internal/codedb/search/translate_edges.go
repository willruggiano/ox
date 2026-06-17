package search

import (
	"fmt"
	"strings"
)

// confidenceClause returns "AND e.confidence IN (?, ?, ...)" expanded for the
// minimum-threshold semantics: "extracted" → just extracted; "inferred" →
// extracted+inferred; "ambiguous" → all three. Empty string for invalid input
// (treated as no extra filter).
func confidenceClause(p *paramCollector, minLevel string) string {
	var levels []string
	switch minLevel {
	case "extracted":
		levels = []string{"extracted"}
	case "inferred":
		levels = []string{"extracted", "inferred"}
	case "ambiguous":
		levels = []string{"extracted", "inferred", "ambiguous"}
	default:
		return ""
	}
	placeholders := make([]string, len(levels))
	for i, l := range levels {
		placeholders[i] = p.add(l)
	}
	return "AND e.confidence IN (" + strings.Join(placeholders, ", ") + ")"
}

// translateCallersEdges resolves `calls:Name` via the symbol_edges table when
// the user requested a confidence threshold (ADR-019 phase 1 read path).
// Edges carry the resolved src_symbol_id, so the caller list is precise even
// when many same-name targets exist; dst_name supports the same pattern/regex
// syntax as the legacy symbol_refs path for backward compatibility.
func translateCallersEdges(query *ParsedQuery) (*TranslatedQuery, error) {
	f := query.Filters
	p := newParamCollector(f.Case)

	if f.Depth > 1 {
		return translateCallersEdgesRecursive(query, p)
	}

	joins := []string{
		"JOIN symbols s ON s.id = e.src_symbol_id",
		"JOIN blobs b ON b.id = e.src_blob_id",
		"JOIN file_revs fr ON fr.blob_id = b.id",
		"JOIN refs r ON r.commit_id = fr.commit_id",
	}
	var conditions []string

	var clause string
	if query.IsRegex {
		clause = regexMatchClause("e.dst_name", f.Calls, p)
	} else {
		clause = patternMatchClause("e.dst_name", f.Calls, p)
	}
	conditions = append(conditions, "AND "+clause)
	conditions = append(conditions, "AND e.kind = 'call'")
	conditions = append(conditions, "AND s.kind = 'function'")
	conditions = append(conditions, confidenceClause(p, f.Confidence))

	addRepoFilter(p, &joins, &conditions, f.Repo, f.NegRepo, "r.repo_id")
	addFileFilter(p, &conditions, f.File, f.NegFile, "fr.path")
	addLangFilter(p, &conditions, f.Lang, f.NegLang)
	addRevFilter(p, &conditions, f.Rev)

	limit := resolveLimit(f.Count)

	sql := fmt.Sprintf(
		"SELECT DISTINCT fr.path, s.name, s.kind, s.line\nFROM symbol_edges e\n%s\nWHERE 1=1%s\nORDER BY fr.path, s.line\nLIMIT %d",
		strings.Join(joins, "\n"), conditionsSQL(conditions), limit)

	return &TranslatedQuery{
		SQL:        sql,
		Params:     p.params,
		SearchType: SearchTypeSymbol,
	}, nil
}

// translateCallersEdgesRecursive walks the caller chain via dst_symbol_id ↔
// src_symbol_id (RESOLVED ids — no name-collision conflation, unlike the
// legacy symbol_refs recursive variant which joins by ref_name string).
func translateCallersEdgesRecursive(query *ParsedQuery, p *paramCollector) (*TranslatedQuery, error) {
	f := query.Filters
	targetParam := p.add(f.Calls)
	depthParam := p.add(fmt.Sprintf("%d", f.Depth))
	confSeedClause := confidenceClause(p, f.Confidence)
	confRecurseClause := confidenceClause(p, f.Confidence) // separate params for the recursive arm

	var revConditions []string
	addRevFilter(p, &revConditions, f.Rev)
	revSQL := conditionsSQL(revConditions)

	limit := resolveLimit(f.Count)

	sql := fmt.Sprintf(`WITH RECURSIVE call_chain(symbol_id, depth) AS (
  SELECT e.src_symbol_id, 1
  FROM symbol_edges e
  JOIN symbols s ON s.id = e.src_symbol_id
  WHERE e.dst_name LIKE '%%' || %s || '%%' AND e.kind = 'call' AND s.kind = 'function'
        %s
  UNION
  SELECT e2.src_symbol_id, cc.depth + 1
  FROM call_chain cc
  JOIN symbol_edges e2 ON e2.dst_symbol_id = cc.symbol_id AND e2.kind = 'call'
  WHERE cc.depth < %s %s
)
SELECT DISTINCT fr.path, s.name, s.kind, cc.depth
FROM call_chain cc
JOIN symbols s ON s.id = cc.symbol_id
JOIN blobs b ON b.id = s.blob_id
JOIN file_revs fr ON fr.blob_id = b.id
JOIN refs r ON r.commit_id = fr.commit_id
WHERE 1=1%s
ORDER BY cc.depth, fr.path
LIMIT %d`, targetParam, confSeedClause, depthParam, confRecurseClause, revSQL, limit)

	return &TranslatedQuery{
		SQL:        sql,
		Params:     p.params,
		SearchType: SearchTypeSymbol,
	}, nil
}

// translateCalleesEdges resolves `calledby:Name` via symbol_edges when the
// caller requested a confidence threshold. dst_symbol_id is resolved so the
// callee list is precise.
func translateCalleesEdges(query *ParsedQuery) (*TranslatedQuery, error) {
	f := query.Filters
	p := newParamCollector(f.Case)

	if f.Depth > 1 {
		return translateCalleesEdgesRecursive(query, p)
	}

	joins := []string{
		"JOIN symbol_edges e ON e.src_symbol_id = s.id",
		"JOIN symbols s_dst ON s_dst.id = e.dst_symbol_id",
		"JOIN blobs b ON b.id = s_dst.blob_id",
		"JOIN file_revs fr ON fr.blob_id = b.id",
		"JOIN refs r ON r.commit_id = fr.commit_id",
	}
	var conditions []string

	var clause string
	if query.IsRegex {
		clause = regexMatchClause("s.name", f.CalledBy, p)
	} else {
		clause = patternMatchClause("s.name", f.CalledBy, p)
	}
	conditions = append(conditions, "AND "+clause)
	conditions = append(conditions, "AND e.kind = 'call'")
	conditions = append(conditions, "AND s.kind = 'function'")
	conditions = append(conditions, confidenceClause(p, f.Confidence))

	addRepoFilter(p, &joins, &conditions, f.Repo, f.NegRepo, "r.repo_id")
	addFileFilter(p, &conditions, f.File, f.NegFile, "fr.path")
	addLangFilter(p, &conditions, f.Lang, f.NegLang)
	addRevFilter(p, &conditions, f.Rev)

	limit := resolveLimit(f.Count)

	sql := fmt.Sprintf(
		"SELECT DISTINCT fr.path, s_dst.name, s_dst.kind, s_dst.line\nFROM symbols s\n%s\nWHERE 1=1%s\nORDER BY fr.path, s_dst.line\nLIMIT %d",
		strings.Join(joins, "\n"), conditionsSQL(conditions), limit)

	return &TranslatedQuery{
		SQL:        sql,
		Params:     p.params,
		SearchType: SearchTypeSymbol,
	}, nil
}

// translateCalleesEdgesRecursive walks the callee chain via resolved
// dst_symbol_id → src_symbol_id, freed from the name-equality conflation of
// the legacy symbol_refs recursion.
func translateCalleesEdgesRecursive(query *ParsedQuery, p *paramCollector) (*TranslatedQuery, error) {
	f := query.Filters
	targetParam := p.add(f.CalledBy)
	depthParam := p.add(fmt.Sprintf("%d", f.Depth))
	confSeedClause := confidenceClause(p, f.Confidence)
	confRecurseClause := confidenceClause(p, f.Confidence)

	var revConditions []string
	addRevFilter(p, &revConditions, f.Rev)
	revSQL := conditionsSQL(revConditions)

	limit := resolveLimit(f.Count)

	sql := fmt.Sprintf(`WITH RECURSIVE call_chain(symbol_id, depth) AS (
  SELECT e.dst_symbol_id, 1
  FROM symbols s
  JOIN symbol_edges e ON e.src_symbol_id = s.id AND e.kind = 'call'
  WHERE s.name LIKE '%%' || %s || '%%' AND s.kind = 'function'
        %s
  UNION
  SELECT e2.dst_symbol_id, cc.depth + 1
  FROM call_chain cc
  JOIN symbol_edges e2 ON e2.src_symbol_id = cc.symbol_id AND e2.kind = 'call'
  WHERE cc.depth < %s %s
)
SELECT DISTINCT fr.path, s.name, s.kind, cc.depth
FROM call_chain cc
JOIN symbols s ON s.id = cc.symbol_id
JOIN blobs b ON b.id = s.blob_id
JOIN file_revs fr ON fr.blob_id = b.id
JOIN refs r ON r.commit_id = fr.commit_id
WHERE 1=1%s
ORDER BY cc.depth, fr.path
LIMIT %d`, targetParam, confSeedClause, depthParam, confRecurseClause, revSQL, limit)

	return &TranslatedQuery{
		SQL:        sql,
		Params:     p.params,
		SearchType: SearchTypeSymbol,
	}, nil
}
