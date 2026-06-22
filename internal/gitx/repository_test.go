package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildGraphFollowsMatchedAncestorsThroughOrdinaryBranches(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "docs-seed@example.com")
	git(t, root, "config", "user.name", "Docs Seed")
	write(t, root, "app.txt", "A\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "A")

	git(t, root, "checkout", "-b", "feature/bootstrap")
	write(t, root, "app.txt", "A\nfeature\n")
	git(t, root, "commit", "-am", "feature")
	git(t, root, "checkout", "-b", "llm/B")
	write(t, root, "app.txt", "A\nfeature\nB\n")
	git(t, root, "commit", "-am", "B")
	git(t, root, "checkout", "-b", "llm/C")
	write(t, root, "app.txt", "A\nfeature\nB\nC\n")
	git(t, root, "commit", "-am", "C")

	repo := Repository{Root: root}
	graph, err := repo.BuildGraph(context.Background(), config.BranchConfig{
		Remote:       "origin",
		MainPatterns: []string{"main", "llm/**"},
	})
	require.NoError(t, err)
	require.Len(t, graph.Branches, 3)

	chain, err := Chain(graph, "llm/C")
	require.NoError(t, err)
	require.Equal(t, []string{"main", "llm/B", "llm/C"}, []string{chain[0].Name, chain[1].Name, chain[2].Name})
	require.Equal(t, 0, chain[0].Depth)
	require.Equal(t, 1, chain[1].Depth)
	require.Equal(t, 2, chain[2].Depth)
}

func TestParentOverrideWins(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "docs-seed@example.com")
	git(t, root, "config", "user.name", "Docs Seed")
	write(t, root, "app.txt", "root\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "root")
	git(t, root, "checkout", "-b", "llm/one")
	write(t, root, "one.txt", "one\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "one")
	git(t, root, "checkout", "main")
	git(t, root, "checkout", "-b", "llm/two")
	write(t, root, "two.txt", "two\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "two")

	repo := Repository{Root: root}
	graph, err := repo.BuildGraph(context.Background(), config.BranchConfig{
		Remote:          "origin",
		MainPatterns:    []string{"main", "llm/**"},
		ParentOverrides: map[string]string{"llm/two": "llm/one"},
	})
	require.NoError(t, err)
	for _, node := range graph.Branches {
		if node.Name == "llm/two" {
			require.Equal(t, "llm/one", node.Parent)
		}
	}
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
}
