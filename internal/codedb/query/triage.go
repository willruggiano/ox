package query

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sageox/ox/internal/codedb/store"
)

// TriagedPR is the agent-facing summary of a pull request with deterministic
// ranking signals attached. All signals are computed from indexed GitHub data
// (pull_requests, pr_comments, pr_commits) — no LLM ranking, no external API.
type TriagedPR struct {
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Author      string   `json:"author,omitempty"`
	URL         string   `json:"url,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	State       string   `json:"state"`
	AgeDays     int      `json:"age_days"`     // days since created_at
	StalledDays int      `json:"stalled_days"` // days since max(updated_at, last comment created_at)
	Comments    int      `json:"comments"`     // total pr_comments rows
	Reviewers   int      `json:"reviewers"`    // distinct comment authors with path != NULL (code reviews)
	Discussants int      `json:"discussants"`  // distinct comment authors with path = NULL (discussion)
	Commits     int      `json:"commits"`      // pr_commits rows
}

// TriageOpts configures PR triage queries.
type TriageOpts struct {
	Limit int    // default 20
	Sort  string // "stalled" (default — most-idle first), "age" (oldest first), "activity" (most comments first)
	State string // "open" (default), "closed", "merged", "all"
}

const (
	defaultTriageLimit = 20
	defaultTriageSort  = "stalled"
	defaultTriageState = "open"
)

// TriagePRs returns ranked pull-request summaries for triage. Deterministic:
// the same inputs always produce the same ranking; no LLM scoring. Default
// behavior surfaces the most-stalled open PRs (those an agent or human should
// look at first because they've been idle longest).
func TriagePRs(ctx context.Context, s *store.Store, opts TriageOpts) ([]TriagedPR, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultTriageLimit
	}
	sort := opts.Sort
	if sort == "" {
		sort = defaultTriageSort
	}
	state := opts.State
	if state == "" {
		state = defaultTriageState
	}

	// "last activity" is the most-recent of pr.updated_at and any comment's
	// created_at. Computed in the SELECT so it's available for ORDER BY and
	// for the stalled_days return value without an extra round-trip.
	q := `
		SELECT
		  pr.number, pr.title, COALESCE(pr.author, ''), COALESCE(pr.url, ''),
		  COALESCE(pr.labels, ''), pr.state,
		  COALESCE(pr.created_at, 0) AS created_at,
		  COALESCE(
		    MAX(
		      COALESCE(pr.updated_at, 0),
		      COALESCE((SELECT MAX(created_at) FROM pr_comments WHERE pr_id = pr.id), 0)
		    ),
		    0
		  ) AS last_activity,
		  COALESCE((SELECT COUNT(*) FROM pr_comments WHERE pr_id = pr.id), 0) AS comments,
		  COALESCE((SELECT COUNT(DISTINCT author) FROM pr_comments WHERE pr_id = pr.id AND path IS NOT NULL), 0) AS reviewers,
		  COALESCE((SELECT COUNT(DISTINCT author) FROM pr_comments WHERE pr_id = pr.id AND path IS NULL), 0) AS discussants,
		  COALESCE((SELECT COUNT(*) FROM pr_commits WHERE pr_id = pr.id), 0) AS commits
		FROM pull_requests pr`

	var args []interface{}
	if state != "all" {
		q += " WHERE pr.state = ?"
		args = append(args, state)
	}
	q += " ORDER BY " + triageOrderBy(sort) + " LIMIT ?"
	args = append(args, limit)

	rows, err := s.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query triage PRs: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []TriagedPR
	for rows.Next() {
		var p TriagedPR
		var labels string
		var createdAt, lastActivity int64
		if err := rows.Scan(
			&p.Number, &p.Title, &p.Author, &p.URL,
			&labels, &p.State,
			&createdAt, &lastActivity,
			&p.Comments, &p.Reviewers, &p.Discussants, &p.Commits,
		); err != nil {
			return nil, fmt.Errorf("scan triage row: %w", err)
		}
		if labels != "" {
			p.Labels = splitAndTrim(labels, ",")
		}
		p.AgeDays = daysBetween(now, createdAt)
		p.StalledDays = daysBetween(now, lastActivity)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate triage rows: %w", err)
	}
	return out, nil
}

// triageOrderBy maps a sort key to the ORDER BY clause. Unknown keys fall
// back to the default ("stalled").
func triageOrderBy(sort string) string {
	switch sort {
	case "age":
		// oldest first; tie-break by number for stability
		return "created_at ASC, pr.number ASC"
	case "activity":
		// most-discussed first
		return "comments DESC, last_activity DESC, pr.number ASC"
	default:
		// stalled: most-idle first (least-recent activity at top)
		return "last_activity ASC, pr.number ASC"
	}
}

// daysBetween returns whole days between two Unix timestamps (both seconds).
// Returns 0 when either is zero (missing data) to avoid garbage age values
// like "55 years stale" for unindexed/null timestamps.
func daysBetween(nowUnix, thenUnix int64) int {
	if nowUnix == 0 || thenUnix == 0 {
		return 0
	}
	d := nowUnix - thenUnix
	if d < 0 {
		return 0
	}
	return int(d / 86400)
}

// splitAndTrim splits s on sep and trims whitespace from each piece, dropping
// empties. Used for the labels CSV column.
func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
