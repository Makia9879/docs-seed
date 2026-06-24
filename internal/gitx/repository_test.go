package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestRootParentOverrideWins(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "develop_V1.0.0")
	git(t, root, "config", "user.email", "docs-seed@example.com")
	git(t, root, "config", "user.name", "Docs Seed")
	write(t, root, "app.txt", "v1\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "v1")
	git(t, root, "checkout", "-b", "develop_V2.0.0")
	write(t, root, "app.txt", "v2\n")
	git(t, root, "commit", "-am", "v2")

	repo := Repository{Root: root}
	graph, err := repo.BuildGraph(context.Background(), config.BranchConfig{
		Remote:       "origin",
		MainPatterns: []string{"develop_V*"},
		ParentOverrides: map[string]string{
			"develop_V1.0.0": "__root__",
			"develop_V2.0.0": "develop_V1.0.0",
		},
	})
	require.NoError(t, err)
	for _, node := range graph.Branches {
		if node.Name == "develop_V1.0.0" {
			require.Empty(t, node.Parent)
		}
		if node.Name == "develop_V2.0.0" {
			require.Equal(t, "develop_V1.0.0", node.Parent)
		}
	}
}

func TestCommitsReturnsReverseChronologicalRangeWithFiles(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "docs-seed@example.com")
	git(t, root, "config", "user.name", "Docs Seed")
	write(t, root, "app.txt", "v1\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "v1")
	base := gitOutput(t, root, "rev-parse", "HEAD")
	write(t, root, "app.txt", "v2\n")
	git(t, root, "commit", "-am", "v2")
	write(t, root, "more.txt", "v3\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "v3")
	head := gitOutput(t, root, "rev-parse", "HEAD")

	commits, err := Repository{Root: root}.Commits(context.Background(), base, head)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	require.Equal(t, "v2", commits[0].Subject)
	require.Equal(t, "v3", commits[1].Subject)
	require.Contains(t, commits[0].Files, "app.txt")
	require.Contains(t, commits[1].Files, "more.txt")
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.Output()
	require.NoError(t, err, "%s", output)
	return strings.TrimSpace(string(output))
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
}
