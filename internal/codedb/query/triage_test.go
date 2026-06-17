package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sageox/ox/internal/codedb/store"
)

// seedTriageDB inserts four pull_requests with controlled created_at /
// updated_at and a varied comment+commit distribution so each sort key has a
// distinct expected ordering. Returns the store; the caller controls cleanup.
func seedTriageDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().Unix()
	day := int64(86400)

	// PRs: (number, created_days_ago, updated_days_ago, state)
	// #1 — created 30d ago, updated 1d ago     → newest activity (least stalled)
	// #2 — created 60d ago, updated 30d ago    → most stalled
	// #3 — created 10d ago, updated 5d ago     → newest PR, middle stalled
	// #4 — created 90d ago, updated 20d ago    → oldest PR
	prs := []struct {
		id      int
		num     int
		created int64
		updated int64
		state   string
		title   string
	}{
		{1, 101, now - 30*day, now - 1*day, "open", "small fix"},
		{2, 102, now - 60*day, now - 30*day, "open", "big refactor"},
		{3, 103, now - 10*day, now - 5*day, "open", "in flight"},
		{4, 104, now - 90*day, now - 20*day, "open", "ancient"},
	}
	for _, p := range prs {
		_, err := s.Exec(`INSERT INTO pull_requests (id, number, title, author, state, labels, created_at, updated_at, url)
		                  VALUES (?, ?, ?, 'alice', ?, '', ?, ?, ?)`,
			p.id, p.num, p.title, p.state, p.created, p.updated,
			"https://example/pr/"+itoa(p.num))
		require.NoError(t, err)
	}

	// Comments: PR #3 gets the most chatter (3 comments, 2 reviewers, 1 discussant).
	// PR #1 gets 1 review comment. Others none.
	comments := []struct {
		id     int
		prID   int
		author string
		path   *string
	}{
		{1, 1, "bob", strPtr("a.go")},   // review (path set)
		{2, 3, "bob", strPtr("b.go")},   // review
		{3, 3, "carol", strPtr("c.go")}, // review (distinct reviewer)
		{4, 3, "dave", nil},             // discussion (path NULL)
	}
	for _, c := range comments {
		_, err := s.Exec(`INSERT INTO pr_comments (id, pr_id, author, body, path, created_at)
		                  VALUES (?, ?, ?, 'msg', ?, ?)`,
			c.id, c.prID, c.author, c.path, now-2*day)
		require.NoError(t, err)
	}

	// Commits: PR #2 has 5, PR #1 has 2.
	commitID := 1
	for prID, n := range map[int]int{2: 5, 1: 2} {
		for i := 0; i < n; i++ {
			_, err := s.Exec("INSERT INTO pr_commits (id, pr_id, sha) VALUES (?, ?, ?)", commitID, prID, "sha"+itoa(commitID))
			require.NoError(t, err)
			commitID++
		}
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestTriagePRs_DefaultStalledOrder verifies the default sort surfaces the
// most-idle PR first. Failure prevented: triage that doesn't actually surface
// stalled PRs is useless to agents.
func TestTriagePRs_DefaultStalledOrder(t *testing.T) {
	s := seedTriageDB(t)
	got, err := TriagePRs(context.Background(), s, TriageOpts{})
	require.NoError(t, err)
	require.Len(t, got, 4)

	// Expected stalled-first order: PR #2 (30d) → #4 (20d) → #3 (2d via last
	// comment, beating its older updated_at) → #1 (1d, most recent activity).
	// This validates that MAX(updated_at, last_comment_at) drives the ranking,
	// not raw updated_at alone.
	require.Equal(t, []int{102, 104, 103, 101}, prNumbers(got),
		"stalled-first: most-idle PR at the top, freshest activity at the bottom")
}

// TestTriagePRs_AgeSort orders by created_at ascending — oldest PR first.
func TestTriagePRs_AgeSort(t *testing.T) {
	s := seedTriageDB(t)
	got, err := TriagePRs(context.Background(), s, TriageOpts{Sort: "age"})
	require.NoError(t, err)
	require.Equal(t, []int{104, 102, 101, 103}, prNumbers(got))
}

// TestTriagePRs_ActivitySort surfaces most-commented PRs first (PR #3 has 3
// comments; PR #1 has 1; others have 0).
func TestTriagePRs_ActivitySort(t *testing.T) {
	s := seedTriageDB(t)
	got, err := TriagePRs(context.Background(), s, TriageOpts{Sort: "activity"})
	require.NoError(t, err)
	require.Equal(t, 103, got[0].Number, "most comments first")
	require.Equal(t, 3, got[0].Comments)
	require.Equal(t, 2, got[0].Reviewers, "review comments have path set")
	require.Equal(t, 1, got[0].Discussants, "discussion comments have path NULL")
}

// TestTriagePRs_SignalsPopulated verifies the per-PR signal fields land
// correctly for downstream agents.
func TestTriagePRs_SignalsPopulated(t *testing.T) {
	s := seedTriageDB(t)
	got, err := TriagePRs(context.Background(), s, TriageOpts{Sort: "activity"})
	require.NoError(t, err)

	byNum := make(map[int]TriagedPR, len(got))
	for _, p := range got {
		byNum[p.Number] = p
	}

	require.Equal(t, 5, byNum[102].Commits, "PR #2 has 5 commits")
	require.Equal(t, 2, byNum[101].Commits, "PR #1 has 2 commits")
	require.GreaterOrEqual(t, byNum[104].AgeDays, 89, "PR #4 is ~90 days old")
}

// TestTriagePRs_StateFilter — "closed" includes only closed/merged.
func TestTriagePRs_StateFilter(t *testing.T) {
	s := seedTriageDB(t)
	// Flip PR #4 to closed.
	_, err := s.Exec("UPDATE pull_requests SET state = 'closed' WHERE number = 104")
	require.NoError(t, err)

	got, err := TriagePRs(context.Background(), s, TriageOpts{State: "closed"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 104, got[0].Number)
}

// TestTriagePRs_LimitRespected — caller-supplied limit caps the result set.
func TestTriagePRs_LimitRespected(t *testing.T) {
	s := seedTriageDB(t)
	got, err := TriagePRs(context.Background(), s, TriageOpts{Limit: 2})
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func prNumbers(prs []TriagedPR) []int {
	out := make([]int, len(prs))
	for i, p := range prs {
		out[i] = p.Number
	}
	return out
}
