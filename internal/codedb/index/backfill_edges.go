package index

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sageox/ox/internal/codedb/store"
	"github.com/sageox/ox/internal/codedb/symbols"
)

// BackfillStats holds counts from a BackfillSymbolEdges pass.
type BackfillStats struct {
	BlobsProcessed uint64
	EdgesInserted  uint64
}

// backfillChunkSize bounds how many blobs are loaded and resolved per
// transaction. Backfill reads symbols + symbol_refs from SQL (no tree-sitter
// re-parsing) so the per-blob cost is small; the chunk size still caps peak
// memory and tx duration on huge repos.
const backfillChunkSize = 256

// BackfillSymbolEdges resolves and inserts symbol_edges for parsed blobs that
// don't yet have edges at the current resolver version (ADR-019 phase 1).
//
// Idempotent: blobs with edge_version >= ResolverVersion are skipped. Runs
// pure-SQL (load symbols + symbol_refs, in-memory resolve, batched insert);
// no tree-sitter, no blob reads. Safe and cheap to run after every index
// pass — the daemon does so as part of doIndex so codedbs upgrade
// automatically without operator intervention.
func BackfillSymbolEdges(ctx context.Context, s *store.Store, progress ProgressFunc) (BackfillStats, error) {
	report := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}

	var stats BackfillStats

	rows, err := s.QueryContext(ctx,
		"SELECT id FROM blobs WHERE parsed = 1 AND edge_version < ?",
		symbols.ResolverVersion)
	if err != nil {
		return stats, fmt.Errorf("query blobs needing edge backfill: %w", err)
	}
	var blobIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return stats, fmt.Errorf("scan blob id: %w", err)
		}
		blobIDs = append(blobIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("iterate blobs for edge backfill: %w", err)
	}
	rows.Close()

	if len(blobIDs) == 0 {
		report("Edge backfill: nothing to do (all parsed blobs at current resolver version).")
		return stats, nil
	}
	report(fmt.Sprintf("Edge backfill: %d blobs to process", len(blobIDs)))

	for start := 0; start < len(blobIDs); start += backfillChunkSize {
		end := min(start+backfillChunkSize, len(blobIDs))
		bp, ei, err := backfillEdgeChunk(ctx, s, blobIDs[start:end])
		stats.BlobsProcessed += bp
		stats.EdgesInserted += ei
		if err != nil {
			return stats, err
		}
		report(fmt.Sprintf("Edge backfill: %d/%d blobs", end, len(blobIDs)))
	}

	report(fmt.Sprintf("Edge backfill complete: %d blobs, %d edges inserted",
		stats.BlobsProcessed, stats.EdgesInserted))
	return stats, nil
}

// backfillEdgeChunk processes one chunk's worth of blobs in a single tx.
// Returns counts only on a successful commit; on any error the tx rolls back
// and (0, 0, err) is returned so accumulated stats stay consistent.
func backfillEdgeChunk(ctx context.Context, s *store.Store, blobIDs []int64) (blobsProcessed, edgesInserted uint64, err error) {
	tx, err := s.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, blobID := range blobIDs {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}

		syms, symDBIDs, err := loadBlobSymbols(ctx, tx, blobID)
		if err != nil {
			return 0, 0, err
		}
		refs, err := loadBlobRefs(ctx, tx, blobID, symDBIDs)
		if err != nil {
			return 0, 0, err
		}

		edgesBefore, err := countBlobEdges(ctx, tx, blobID)
		if err != nil {
			return 0, 0, err
		}
		if err := insertSymbolEdges(tx, blobID, syms, refs, symDBIDs); err != nil {
			return 0, 0, fmt.Errorf("backfill edges for blob %d: %w", blobID, err)
		}
		edgesAfter, err := countBlobEdges(ctx, tx, blobID)
		if err != nil {
			return 0, 0, err
		}
		edgesInserted += uint64(edgesAfter - edgesBefore)

		if _, err := tx.ExecContext(ctx,
			"UPDATE blobs SET edge_version = ? WHERE id = ?",
			symbols.ResolverVersion, blobID,
		); err != nil {
			return 0, 0, fmt.Errorf("update edge_version for blob %d: %w", blobID, err)
		}
		blobsProcessed++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit backfill chunk: %w", err)
	}
	return blobsProcessed, edgesInserted, nil
}

// loadBlobSymbols reads all symbols for a blob in their insertion order and
// returns the reconstructed symbols.Symbol slice + the parallel slice of DB
// ids needed for edge insertion. ParentIdx is left at -1 because the
// resolver doesn't consult it for same-file scope today.
func loadBlobSymbols(ctx context.Context, tx *sql.Tx, blobID int64) ([]symbols.Symbol, []int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, kind, line, col, COALESCE(end_line, 0), COALESCE(end_col, 0),
		        COALESCE(signature, ''), COALESCE(return_type, ''), COALESCE(params, '')
		 FROM symbols WHERE blob_id = ? ORDER BY id`,
		blobID)
	if err != nil {
		return nil, nil, fmt.Errorf("load symbols for blob %d: %w", blobID, err)
	}
	defer rows.Close()

	var syms []symbols.Symbol
	var ids []int64
	for rows.Next() {
		var id int64
		var name, kind, sig, retType, params string
		var line, col, endLine, endCol int
		if err := rows.Scan(&id, &name, &kind, &line, &col, &endLine, &endCol, &sig, &retType, &params); err != nil {
			return nil, nil, fmt.Errorf("scan symbol row for blob %d: %w", blobID, err)
		}
		ids = append(ids, id)
		syms = append(syms, symbols.Symbol{
			Name: name, Kind: kind, Line: line, Col: col,
			EndLine: endLine, EndCol: endCol,
			Signature: sig, ReturnType: retType, Params: params,
			ParentIdx: -1,
		})
	}
	return syms, ids, rows.Err()
}

// loadBlobRefs reads symbol_refs for a blob and recovers ContainingSymIdx by
// mapping each row's stored symbol_id back to its index in symDBIDs (which is
// the order loadBlobSymbols returned). Refs whose stored symbol_id is NULL or
// not in the map get ContainingSymIdx = -1 (file-scope; resolver drops them).
func loadBlobRefs(ctx context.Context, tx *sql.Tx, blobID int64, symDBIDs []int64) ([]symbols.Ref, error) {
	idToIdx := make(map[int64]int, len(symDBIDs))
	for i, id := range symDBIDs {
		idToIdx[id] = i
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT ref_name, kind, line, col, symbol_id FROM symbol_refs WHERE blob_id = ?",
		blobID)
	if err != nil {
		return nil, fmt.Errorf("load refs for blob %d: %w", blobID, err)
	}
	defer rows.Close()

	var refs []symbols.Ref
	for rows.Next() {
		var refName, kind string
		var line, col int
		var containingSymID sql.NullInt64
		if err := rows.Scan(&refName, &kind, &line, &col, &containingSymID); err != nil {
			return nil, fmt.Errorf("scan ref row for blob %d: %w", blobID, err)
		}
		containingIdx := -1
		if containingSymID.Valid {
			if idx, ok := idToIdx[containingSymID.Int64]; ok {
				containingIdx = idx
			}
		}
		refs = append(refs, symbols.Ref{
			RefName: refName, Kind: kind, Line: line, Col: col,
			ContainingSymIdx: containingIdx,
		})
	}
	return refs, rows.Err()
}

// countBlobEdges returns the number of symbol_edges rows currently in the
// transaction for the given blob. Used by backfill to attribute the
// per-blob edge insertion count (insertSymbolEdges is shared with the
// inline ParseSymbols path and doesn't return a count).
func countBlobEdges(ctx context.Context, tx *sql.Tx, blobID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM symbol_edges WHERE src_blob_id = ?", blobID,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
