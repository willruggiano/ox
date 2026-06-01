package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/sageox/ox/internal/codedb/store"
)

// buildWideRepo creates a single-commit repo with numFiles Go source files,
// each containing funcsPerFile symbol-bearing functions (plus a helper and call
// references). ParseSymbols parses only tip-visible blobs, so a wide tip — not a
// deep history — is what exercises the tree-sitter extraction phase.
func buildWideRepo(b *testing.B, numFiles, funcsPerFile int) string {
	b.Helper()
	dir := b.TempDir()

	repo, err := git.PlainInit(dir, false)
	require.NoError(b, err)
	wt, err := repo.Worktree()
	require.NoError(b, err)
	sig := &object.Signature{Name: "bench", Email: "b@b.com", When: time.Now()}

	for f := 0; f < numFiles; f++ {
		var sb strings.Builder
		sb.WriteString("package main\n\nimport \"fmt\"\n\n")
		for fn := 0; fn < funcsPerFile; fn++ {
			// Real signatures, params, return types, a call site, and a doc
			// comment so extraction (incl. type-info enrichment) has work to do.
			fmt.Fprintf(&sb,
				"// F%[1]d_%[2]d does work.\nfunc F%[1]d_%[2]d(a int, b string) (int, error) {\n\tx := helper%[1]d(a)\n\tfmt.Println(b, x)\n\treturn x, nil\n}\n\n",
				f, fn)
		}
		fmt.Fprintf(&sb, "func helper%d(n int) int { return n * 2 }\n", f)

		fname := fmt.Sprintf("file%04d.go", f)
		require.NoError(b, os.WriteFile(filepath.Join(dir, fname), []byte(sb.String()), 0o644))
		_, err := wt.Add(fname)
		require.NoError(b, err)
	}
	_, err = wt.Commit("wide commit", &git.CommitOptions{Author: sig})
	require.NoError(b, err)
	return dir
}

// BenchmarkParseSymbols_Wide measures the symbol-parsing pipeline (tree-sitter
// extraction + batched inserts) over many tip files. Each iteration re-indexes
// a fresh store (untimed) so blobs start unparsed, then times only ParseSymbols.
// Serves as a throughput/regression baseline for future symbol-parsing work.
//
//	go test -bench=BenchmarkParseSymbols_Wide -benchmem -count=5 ./internal/codedb/index/
func BenchmarkParseSymbols_Wide(b *testing.B) {
	repoDir := buildWideRepo(b, 300, 25)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, err := store.Open(b.TempDir())
		require.NoError(b, err)
		require.NoError(b, IndexLocalRepo(ctx, s, repoDir, IndexOptions{}))
		b.StartTimer()

		if _, err := ParseSymbols(ctx, s, nil); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		require.NoError(b, s.Close())
		b.StartTimer()
	}
}
