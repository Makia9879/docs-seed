package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Makia9879/docs-seed/internal/model"
)

func StateDir(root string) string {
	return filepath.Join(root, ".docs-seed")
}

func Ensure(root string) error {
	for _, dir := range []string{
		StateDir(root),
		filepath.Join(StateDir(root), "state", "facts"),
		filepath.Join(StateDir(root), "state", "commits"),
		filepath.Join(StateDir(root), "memory", "agent-outputs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	ignore := "state/\nmemory/\nlogs/\ntmp/\n"
	return os.WriteFile(filepath.Join(StateDir(root), ".gitignore"), []byte(ignore), 0o644)
}

func SaveGraph(root string, graph model.BranchGraph) error {
	return writeJSON(filepath.Join(StateDir(root), "branch-graph.json"), graph)
}

func LoadGraph(root string) (model.BranchGraph, error) {
	var graph model.BranchGraph
	err := readJSON(filepath.Join(StateDir(root), "branch-graph.json"), &graph)
	return graph, err
}

func FactPath(root, branch string) string {
	return filepath.Join(StateDir(root), "state", "facts", safeName(branch)+".json")
}

func SaveFact(root string, fact model.Fact) error {
	return writeJSON(FactPath(root, fact.Branch), fact)
}

func LoadFact(root, branch string) (model.Fact, error) {
	var fact model.Fact
	err := readJSON(FactPath(root, branch), &fact)
	return fact, err
}

func CommitFactPath(root, branch, hash string) string {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return filepath.Join(CommitFactDir(root, branch), safeName(hash)+".json")
}

func CommitFactDir(root, branch string) string {
	return filepath.Join(StateDir(root), "state", "commits", safeName(branch))
}

func SaveCommitFact(root string, fact model.CommitFact) error {
	return writeJSON(CommitFactPath(root, fact.Branch, fact.Commit.Hash), fact)
}

func LoadCommitFact(root, branch, hash string) (model.CommitFact, error) {
	var fact model.CommitFact
	err := readJSON(CommitFactPath(root, branch, hash), &fact)
	return fact, err
}

func BranchDocDir(output, branch string) string {
	return filepath.Join(output, "branches", safeName(branch))
}

func safeName(value string) string {
	replacer := strings.NewReplacer("/", "__", "\\", "__", "..", "_")
	value = replacer.Replace(strings.TrimSpace(value))
	if value == "" {
		return "branch"
	}
	return value
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("解析 %s: %w", path, err)
	}
	return nil
}

func AtomicWrite(path string, data []byte) error {
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".docs-seed-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
