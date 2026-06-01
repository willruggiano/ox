package search

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2"
	blevesearch "github.com/blevesearch/bleve/v2/search"
	_ "github.com/blevesearch/bleve/v2/search/highlight/highlighter/ansi"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/sageox/ox/internal/codedb/language"
	"github.com/sageox/ox/internal/codedb/store"
)

// Result represents a single search result.
type Result struct {
	Repo        string  `json:"repo,omitempty"`
	FilePath    string  `json:"file_path,omitempty"`
	Content     string  `json:"content,omitempty"`
	Score       float64 `json:"score,omitempty"`
	Line        int     `json:"line,omitempty"`
	Language    string  `json:"language,omitempty"`
	CommitHash  string  `json:"commit_hash,omitempty"`
	Author      string  `json:"author,omitempty"`
	Message     string  `json:"message,omitempty"`
	SymbolName  string  `json:"symbol_name,omitempty"`
	SymbolKind  string  `json:"symbol_kind,omitempty"`
	CommentKind string  `json:"comment_kind,omitempty"`
	CommentText string  `json:"comment_text,omitempty"`

	// PR/issue-specific fields (populated for type:pr and type:issue results)
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Execute runs a parsed query against the store using the planner to determine
// the execution strategy (SQL only, Bleve only, or intersect).
func Execute(ctx context.Context, s *store.Store, query *ParsedQuery) ([]Result, error) {
	plan, err := Plan(query)
	if err != nil {
		return nil, err
	}

	switch plan.Strategy {
	case JoinSQLOnly:
		return executePlanSQL(ctx, s, plan)
	case JoinBleveOnly:
		return executePlanBleve(ctx, s, plan, nil)
	case JoinIntersect:
		return executePlanBleve(ctx, s, plan, &query.Filters)
	default:
		return executePlanSQL(ctx, s, plan)
	}
}

// executePlanSQL executes a plan that only needs SQL (commits, symbols, calls, PRs, issues).
func executePlanSQL(ctx context.Context, s *store.Store, plan *ExecutionPlan) ([]Result, error) {
	args := make([]interface{}, len(plan.SQLParams))
	for i, p := range plan.SQLParams {
		args[i] = p
	}

	rows, err := s.QueryContext(ctx, plan.SQL, args...)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// pre-allocate scan buffers once; reused across all rows
	values := make([]sql.NullString, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var results []Result
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			slog.Warn("scan error, skipping row", "err", err)
			continue
		}

		r := Result{}
		for i, col := range cols {
			val := values[i].String
			switch col {
			case "path":
				r.FilePath = val
			case "name":
				r.SymbolName = val
			case "kind":
				r.SymbolKind = val
			case "line":
				r.Line, _ = strconv.Atoi(val)
			case "hash":
				r.CommitHash = val
			case "author":
				r.Author = val
			case "message":
				r.Message = val
			case "score":
				r.Score, _ = strconv.ParseFloat(val, 64)
			case "snippet":
				r.Content = val
			case "language":
				r.Language = val
			case "number":
				r.Number, _ = strconv.Atoi(val)
			case "title":
				r.Title = val
				r.Content = val
			case "state":
				r.State = val
			case "url":
				r.URL = val
			}
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return results, nil
}

// executePlanBleve runs a Bleve full-text search and enriches results from SQL.
// When filters is non-nil (intersect strategy), metadata filters are applied.
// When filters is nil (bleve-only strategy), no additional filtering is done.
func executePlanBleve(ctx context.Context, s *store.Store, plan *ExecutionPlan, filters *Filters) ([]Result, error) {
	var idx bleve.Index
	switch plan.BleveIndex {
	case "diff":
		idx = s.DiffIndex
	case "comment":
		idx = s.CommentIndex
	default:
		idx = s.CombinedCodeIndex
	}

	var bleveQuery query.Query
	if plan.IsRegex {
		rq := bleve.NewRegexpQuery(plan.BleveQuery)
		rq.SetField("content")
		bleveQuery = rq
	} else {
		bleveQuery = bleve.NewQueryStringQuery(plan.BleveQuery)
	}
	searchReq := bleve.NewSearchRequestOptions(bleveQuery, plan.Limit*5, 0, false)
	// Per ADR-018, the committed code / comment / diff Bleve indexes are all
	// index-only (Store=false) — no stored fields to return, no fragments.
	// Snippet sources:
	//   - committed code: re-derived from git blob in assembleCodeHit (phase 1b)
	//   - comment: SQLite `comments.text` via assembleCommentHit (phase 1a)
	//   - diff: regenerated on read via Store.DiffSnippet from old/new blobs
	//     in assembleDiffHit (phase 2 Option A; LRU-cached, no SQLite bloat)
	// The CombinedCodeIndex aliases dirty overlays (still stored, in-memory),
	// so we keep Highlight on for the code path: stored-doc (dirty) hits return
	// fragments; index-only (committed) hits get empty fragments and fall
	// through to blob re-derivation.
	switch plan.BleveIndex {
	case "diff", "comment":
		// index-only, no overlays — skip highlight entirely
	default:
		// code path keeps highlight to surface dirty-overlay fragments
		searchReq.Fields = []string{"content"}
		searchReq.Highlight = bleve.NewHighlightWithStyle("ansi")
	}
	searchResult, err := idx.Search(searchReq)
	if err != nil {
		return nil, fmt.Errorf("bleve search: %w", err)
	}

	if searchResult.Total == 0 {
		return nil, nil
	}

	// Enrich all hits with a single batched SQL query per search rather than one
	// lookup per hit (avoids an N+1 against SQLite, which also reduces reader
	// contention with the daemon writer). Dirty code hits carry no SQL metadata
	// and are assembled inline.
	ids := make([]string, 0, len(searchResult.Hits))
	for _, hit := range searchResult.Hits {
		switch plan.BleveIndex {
		case "diff":
			ids = append(ids, strings.TrimPrefix(hit.ID, "diff_"))
		case "comment":
			ids = append(ids, strings.TrimPrefix(hit.ID, "comment_"))
		default:
			if !strings.HasPrefix(hit.ID, "dirty_") {
				ids = append(ids, strings.TrimPrefix(hit.ID, "blob_"))
			}
		}
	}

	var meta map[string][]Result
	var codeHashes map[string]string      // populated for code path only
	var diffBlobsMap map[string]diffBlobs // populated for diff path only (Option A snippet derivation)
	switch plan.BleveIndex {
	case "diff":
		meta, diffBlobsMap, err = enrichDiffHits(ctx, s, ids, filters)
	case "comment":
		meta, err = enrichCommentHits(ctx, s, ids, filters)
	default:
		meta, codeHashes, err = enrichCodeHits(ctx, s, ids, filters)
	}
	if err != nil {
		return nil, err
	}

	var results []Result
	seen := make(map[string]bool) // dedup by file:line to avoid worktree/committed duplicates
	for _, hit := range searchResult.Hits {
		if err := ctx.Err(); err != nil {
			return results, err
		}

		fragment := extractFragment(hit)

		var hitResults []Result
		switch plan.BleveIndex {
		case "diff":
			hitResults = assembleDiffHit(s, hit, plan, fragment, meta, diffBlobsMap)
		case "comment":
			hitResults = assembleCommentHit(hit, fragment, meta)
		default:
			if strings.HasPrefix(hit.ID, "dirty_") {
				hitResults, _ = enrichDirtyHit(hit, fragment)
			} else {
				hitResults = assembleCodeHit(s, hit, plan, fragment, meta, codeHashes)
			}
		}
		for _, r := range hitResults {
			key := r.FilePath + ":" + strconv.Itoa(r.Line) + ":" + r.Content
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, r)
		}

		if len(results) >= plan.Limit {
			results = results[:plan.Limit]
			break
		}
	}

	return results, nil
}

// sqlInPlaceholders returns "?,?,…" with n placeholders for a SQL IN clause.
func sqlInPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// extractFragment pulls the first highlighted content fragment from a Bleve hit.
func extractFragment(hit *blevesearch.DocumentMatch) string {
	if frags, ok := hit.Fragments["content"]; ok && len(frags) > 0 {
		return frags[0]
	}
	return ""
}

// enrichDiffHits batch-loads diff metadata for all Bleve diff hits, keyed by
// diff id. Score and content are per-hit and applied later in assembleDiffHit.
// diffBlobs carries the inputs needed to re-derive a diff snippet on read.
type diffBlobs struct {
	oldHash string
	newHash string
	path    string
}

func enrichDiffHits(ctx context.Context, s *store.Store, diffIDs []string, filters *Filters) (map[string][]Result, map[string]diffBlobs, error) {
	if len(diffIDs) == 0 {
		return nil, nil, nil
	}

	// LEFT JOIN on blobs for old/new content_hash so assembleDiffHit can call
	// Store.DiffSnippet (ADR-018 phase 2 Option A — regenerate on read).
	// old_blob_id and new_blob_id are nullable (file add/delete); the joined
	// hash columns come back as NULL on the missing side.
	sqlQ := `
		SELECT d.id, substr(c.hash, 1, 10), c.author, substr(c.message, 1, 80), d.path,
		       ob.content_hash, nb.content_hash
		FROM diffs d
		JOIN commits c ON c.id = d.commit_id
		LEFT JOIN blobs ob ON ob.id = d.old_blob_id
		LEFT JOIN blobs nb ON nb.id = d.new_blob_id
		WHERE d.id IN (` + sqlInPlaceholders(len(diffIDs)) + `)`
	args := make([]interface{}, 0, len(diffIDs)+4)
	for _, id := range diffIDs {
		args = append(args, id)
	}

	if filters != nil {
		addDiffFilters(&sqlQ, &args, filters)
	}

	rows, err := s.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("query diff hit metadata: %w", err)
	}
	defer rows.Close()

	meta := make(map[string][]Result, len(diffIDs))
	blobs := make(map[string]diffBlobs, len(diffIDs))
	for rows.Next() {
		var id int64
		var hash, author, message, path string
		var oldHash, newHash sql.NullString
		if err := rows.Scan(&id, &hash, &author, &message, &path, &oldHash, &newHash); err != nil {
			slog.Warn("diff hit scan error, skipping row", "err", err)
			continue
		}
		key := strconv.FormatInt(id, 10)
		meta[key] = append(meta[key], Result{
			CommitHash: hash, Author: author, Message: message, FilePath: path,
		})
		blobs[key] = diffBlobs{oldHash: oldHash.String, newHash: newHash.String, path: path}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate diff rows: %w", err)
	}
	return meta, blobs, nil
}

// assembleDiffHit applies per-hit score and re-derives a Content snippet by
// regenerating the diff text from the recorded blob hashes (ADR-018 phase 2
// Option A: no permanent diff-text storage; Store.DiffSnippet computes via
// diffformat.Format, cached in a per-Store LRU). Empty `Line` — the matched
// position within a synthetic diff isn't a meaningful file-line and the
// useful locator is already CommitHash + FilePath.
func assembleDiffHit(s *store.Store, hit *blevesearch.DocumentMatch, plan *ExecutionPlan, fragment string, meta map[string][]Result, blobs map[string]diffBlobs) []Result {
	id := strings.TrimPrefix(hit.ID, "diff_")
	rows := meta[id]
	if len(rows) == 0 {
		return nil
	}
	// Derive snippet once per diff (all rows share the same diff text).
	snippet := fragment
	if snippet == "" {
		if b, ok := blobs[id]; ok && (b.oldHash != "" || b.newHash != "") {
			text := s.DiffSnippet(b.oldHash, b.newHash, b.path)
			if text != "" {
				_, snippet = locateMatchAndSnippet([]byte(text), plan, codeSnippetMaxLen)
			}
		}
	}
	out := make([]Result, 0, len(rows))
	for _, r := range rows {
		r.Score = hit.Score
		r.Content = snippet
		out = append(out, r)
	}
	return out
}

// enrichCodeHits batch-loads file metadata + content_hash for every Bleve code
// hit in one round-trip (avoids N+1). The returned hashes map joins each blob
// id to its git content_hash so assembleCodeHit can re-derive the snippet from
// the blob (ADR-018 phase 1b: Bleve no longer stores committed code).
func enrichCodeHits(ctx context.Context, s *store.Store, blobIDs []string, filters *Filters) (map[string][]Result, map[string]string, error) {
	if len(blobIDs) == 0 {
		return nil, nil, nil
	}

	var revFilter string
	if filters != nil && filters.Rev != "" {
		revFilter = resolveRevRef(filters.Rev)
	}

	sqlQ := `
		SELECT b.id, fr.path, b.language, rp.name, b.content_hash
		FROM blobs b
		JOIN file_revs fr ON fr.blob_id = b.id
		LEFT JOIN refs r ON r.commit_id = fr.commit_id
		LEFT JOIN repos rp ON rp.id = r.repo_id
		WHERE b.id IN (` + sqlInPlaceholders(len(blobIDs)) + `)`
	args := make([]interface{}, 0, len(blobIDs)+4)
	for _, id := range blobIDs {
		args = append(args, id)
	}

	if revFilter != "" {
		sqlQ += " AND r.name = ?"
		args = append(args, revFilter)
	}

	if filters != nil {
		addCodeFilters(&sqlQ, &args, filters)
	}

	sqlQ += " GROUP BY b.id, fr.path"

	rows, err := s.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("query code hit metadata: %w", err)
	}
	defer rows.Close()

	meta := make(map[string][]Result, len(blobIDs))
	hashes := make(map[string]string, len(blobIDs))
	for rows.Next() {
		var id int64
		var path string
		var lang, repo sql.NullString
		var hash string
		if err := rows.Scan(&id, &path, &lang, &repo, &hash); err != nil {
			slog.Warn("code hit scan error, skipping row", "err", err)
			continue
		}
		key := strconv.FormatInt(id, 10)
		meta[key] = append(meta[key], Result{
			FilePath: path, Language: lang.String, Repo: repo.String,
		})
		hashes[key] = hash // identical across rows for the same blob; last write is fine
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate code rows: %w", err)
	}
	return meta, hashes, nil
}

// assembleCodeHit applies the per-hit score and Content/Line for one Bleve hit.
// A non-empty fragment means the hit came from a stored doc (dirty overlay) and
// is used as-is. Otherwise — the index-only committed code path — the blob is
// read once via Store.ReadBlob and the match located so Line + a plain-text
// snippet are re-derived (ADR-018 phase 1b). All rows for the same blob share
// the same derived snippet+line.
func assembleCodeHit(s *store.Store, hit *blevesearch.DocumentMatch, plan *ExecutionPlan, fragment string, meta map[string][]Result, hashes map[string]string) []Result {
	id := strings.TrimPrefix(hit.ID, "blob_")
	rows := meta[id]
	if len(rows) == 0 {
		return nil
	}
	line, snippet := 0, fragment
	if fragment == "" {
		if hash := hashes[id]; hash != "" {
			if content := s.ReadBlob(hash); content != nil {
				line, snippet = locateMatchAndSnippet(content, plan, codeSnippetMaxLen)
			}
		}
	}
	out := make([]Result, 0, len(rows))
	for _, r := range rows {
		r.Score = hit.Score
		r.Content = snippet
		r.Line = line
		out = append(out, r)
	}
	return out
}

// enrichCommentHits batch-loads comment metadata for all Bleve comment hits,
// keyed by comment id. Score and content are per-hit and applied later in
// assembleCommentHit.
func enrichCommentHits(ctx context.Context, s *store.Store, commentIDs []string, filters *Filters) (map[string][]Result, error) {
	if len(commentIDs) == 0 {
		return nil, nil
	}

	sqlQ := `
		SELECT cm.id, fr.path, b.language, rp.name, cm.kind, cm.text, cm.line
		FROM comments cm
		JOIN blobs b ON b.id = cm.blob_id
		JOIN file_revs fr ON fr.blob_id = b.id
		LEFT JOIN refs r ON r.commit_id = fr.commit_id
		LEFT JOIN repos rp ON rp.id = r.repo_id
		WHERE cm.id IN (` + sqlInPlaceholders(len(commentIDs)) + `)`
	args := make([]interface{}, 0, len(commentIDs)+4)
	for _, id := range commentIDs {
		args = append(args, id)
	}

	if filters != nil {
		addCommentFilters(&sqlQ, &args, filters)
	}

	sqlQ += " GROUP BY cm.id, fr.path"

	rows, err := s.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, fmt.Errorf("query comment hit metadata: %w", err)
	}
	defer rows.Close()

	meta := make(map[string][]Result, len(commentIDs))
	for rows.Next() {
		var id int64
		var path string
		var lang, repo sql.NullString
		var kind, text string
		var line int
		if err := rows.Scan(&id, &path, &lang, &repo, &kind, &text, &line); err != nil {
			slog.Warn("comment hit scan error, skipping row", "err", err)
			continue
		}
		key := strconv.FormatInt(id, 10)
		meta[key] = append(meta[key], Result{
			FilePath:    path,
			Language:    lang.String,
			Repo:        repo.String,
			Line:        line,
			CommentKind: kind,
			CommentText: text,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate comment rows: %w", err)
	}
	return meta, nil
}

// assembleCommentHit applies the per-hit score and fragment to the comment
// metadata rows for one Bleve hit, falling back to the stored comment text when
// the hit has no highlighted fragment.
func assembleCommentHit(hit *blevesearch.DocumentMatch, fragment string, meta map[string][]Result) []Result {
	rows := meta[strings.TrimPrefix(hit.ID, "comment_")]
	out := make([]Result, 0, len(rows))
	for _, r := range rows {
		content := fragment
		if content == "" {
			content = r.CommentText
		}
		r.Score = hit.Score
		r.Content = content
		out = append(out, r)
	}
	return out
}

// enrichDirtyHit creates a result for a dirty worktree Bleve hit (no SQL metadata).
// The doc ID encodes the relative path: "dirty_<relPath>".
func enrichDirtyHit(hit *blevesearch.DocumentMatch, fragment string) ([]Result, error) {
	relPath := strings.TrimPrefix(hit.ID, "dirty_")
	lang := language.Detect(relPath)
	ext := filepath.Ext(relPath)
	if lang == "" && ext != "" {
		lang = strings.TrimPrefix(ext, ".")
	}
	return []Result{{
		FilePath: relPath,
		Score:    hit.Score,
		Content:  fragment,
		Language: lang,
	}}, nil
}

// addDiffFilters appends SQL WHERE clauses for diff metadata filtering.
func addDiffFilters(sqlQ *string, args *[]interface{}, f *Filters) {
	if f.Repo != "" {
		*sqlQ += " AND c.repo_id IN (SELECT id FROM repos WHERE " + likeOrGlob("name", f.Repo, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.Repo))
	}
	if f.NegRepo != "" {
		*sqlQ += " AND c.repo_id NOT IN (SELECT id FROM repos WHERE " + likeOrGlob("name", f.NegRepo, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegRepo))
	}
	if f.File != "" {
		*sqlQ += " AND " + likeOrGlob("d.path", f.File, f.Case)
		*args = append(*args, likeOrGlobParam(f.File))
	}
	if f.NegFile != "" {
		*sqlQ += " AND NOT (" + likeOrGlob("d.path", f.NegFile, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegFile))
	}
	if f.Author != "" {
		*sqlQ += " AND c.author LIKE ?"
		*args = append(*args, "%"+f.Author+"%")
	}
	if f.NegAuthor != "" {
		*sqlQ += " AND c.author NOT LIKE ?"
		*args = append(*args, "%"+f.NegAuthor+"%")
	}
	if f.Before != "" {
		*sqlQ += " AND c.timestamp < CAST(strftime('%s', ?) AS INTEGER)"
		*args = append(*args, f.Before)
	}
	if f.After != "" {
		*sqlQ += " AND c.timestamp > CAST(strftime('%s', ?) AS INTEGER)"
		*args = append(*args, f.After)
	}
}

// addCodeFilters appends SQL WHERE clauses for code metadata filtering.
func addCodeFilters(sqlQ *string, args *[]interface{}, f *Filters) {
	if f.Repo != "" {
		*sqlQ += " AND " + likeOrGlob("rp.name", f.Repo, f.Case)
		*args = append(*args, likeOrGlobParam(f.Repo))
	}
	if f.NegRepo != "" {
		*sqlQ += " AND NOT (" + likeOrGlob("rp.name", f.NegRepo, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegRepo))
	}
	if f.File != "" {
		*sqlQ += " AND " + likeOrGlob("fr.path", f.File, f.Case)
		*args = append(*args, likeOrGlobParam(f.File))
	}
	if f.NegFile != "" {
		*sqlQ += " AND NOT (" + likeOrGlob("fr.path", f.NegFile, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegFile))
	}
	if f.Lang != "" {
		*sqlQ += " AND b.language = ?"
		*args = append(*args, f.Lang)
	}
	if f.NegLang != "" {
		*sqlQ += " AND b.language != ?"
		*args = append(*args, f.NegLang)
	}
}

// addCommentFilters appends SQL WHERE clauses for comment metadata filtering.
func addCommentFilters(sqlQ *string, args *[]interface{}, f *Filters) {
	if f.Repo != "" {
		*sqlQ += " AND " + likeOrGlob("rp.name", f.Repo, f.Case)
		*args = append(*args, likeOrGlobParam(f.Repo))
	}
	if f.NegRepo != "" {
		*sqlQ += " AND NOT (" + likeOrGlob("rp.name", f.NegRepo, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegRepo))
	}
	if f.File != "" {
		*sqlQ += " AND " + likeOrGlob("fr.path", f.File, f.Case)
		*args = append(*args, likeOrGlobParam(f.File))
	}
	if f.NegFile != "" {
		*sqlQ += " AND NOT (" + likeOrGlob("fr.path", f.NegFile, f.Case) + ")"
		*args = append(*args, likeOrGlobParam(f.NegFile))
	}
	if f.Lang != "" {
		*sqlQ += " AND b.language = ?"
		*args = append(*args, f.Lang)
	}
	if f.NegLang != "" {
		*sqlQ += " AND b.language != ?"
		*args = append(*args, f.NegLang)
	}
	if f.CommentKind != "" {
		*sqlQ += " AND cm.kind = ?"
		*args = append(*args, f.CommentKind)
	}
}

// likeOrGlob returns a SQL clause using GLOB for wildcard patterns, LIKE otherwise.
func likeOrGlob(column, pattern string, caseSensitive bool) string {
	if strings.ContainsAny(pattern, "*?") {
		if caseSensitive {
			return column + " GLOB ?"
		}
		return "lower(" + column + ") GLOB ?"
	}
	return column + " LIKE ?"
}

// likeOrGlobParam returns the appropriate parameter value for likeOrGlob.
func likeOrGlobParam(pattern string) string {
	if strings.ContainsAny(pattern, "*?") {
		return pattern
	}
	return "%" + pattern + "%"
}
