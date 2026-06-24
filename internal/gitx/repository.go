package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/Makia9879/docs-seed/internal/model"
)

type Repository struct {
	Root string
}

type Ref struct {
	Name       string
	FullName   string
	Hash       string
	Source     string
	CommitTime time.Time
}

func DiscoverRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("当前目录不在 Git 仓库中")
	}
	return strings.TrimSpace(string(out)), nil
}

func (r Repository) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r Repository) Fetch(ctx context.Context, remote string) error {
	_, err := r.run(ctx, "fetch", "--all", "--prune")
	return err
}

func (r Repository) CurrentBranch(ctx context.Context) (string, error) {
	return r.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
}

func (r Repository) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "status", "--porcelain")
	return out != "", err
}

func (r Repository) MatchingRefs(ctx context.Context, cfg config.BranchConfig) ([]Ref, error) {
	format := "%(refname)%00%(objectname)%00%(committerdate:unix)"
	out, err := r.run(ctx, "for-each-ref", "--format="+format, "refs/heads", "refs/remotes/"+cfg.Remote)
	if err != nil {
		return nil, err
	}
	refs := map[string]Ref{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), "\x00")
		if len(parts) != 3 || strings.HasSuffix(parts[0], "/HEAD") {
			continue
		}
		full := parts[0]
		name, source := "", ""
		switch {
		case strings.HasPrefix(full, "refs/heads/"):
			name, source = strings.TrimPrefix(full, "refs/heads/"), "local"
		case strings.HasPrefix(full, "refs/remotes/"+cfg.Remote+"/"):
			name, source = strings.TrimPrefix(full, "refs/remotes/"+cfg.Remote+"/"), "remote"
		}
		if name == "" || !matchesAny(name, cfg.MainPatterns) {
			continue
		}
		unix, _ := strconv.ParseInt(parts[2], 10, 64)
		ref := Ref{Name: name, FullName: full, Hash: parts[1], Source: source, CommitTime: time.Unix(unix, 0).UTC()}
		if old, ok := refs[name]; !ok || source == "local" || old.Source != "local" {
			refs[name] = ref
		}
	}
	result := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CommitTime.Equal(result[j].CommitTime) {
			return result[i].Name < result[j].Name
		}
		return result[i].CommitTime.Before(result[j].CommitTime)
	})
	return result, scanner.Err()
}

func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "**")
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func (r Repository) BuildGraph(ctx context.Context, cfg config.BranchConfig) (model.BranchGraph, error) {
	refs, err := r.MatchingRefs(ctx, cfg)
	if err != nil {
		return model.BranchGraph{}, err
	}
	if len(refs) == 0 {
		return model.BranchGraph{}, errors.New("没有分支匹配 branches.main_patterns")
	}
	nodes := make([]model.BranchNode, 0, len(refs))
	for _, child := range refs {
		parent, fork, err := r.parentFor(ctx, child, refs, cfg.ParentOverrides)
		if err != nil {
			return model.BranchGraph{}, err
		}
		nodes = append(nodes, model.BranchNode{
			Name: child.Name, Ref: child.FullName, Tip: child.Hash, Parent: parent,
			ForkPoint: fork, Source: child.Source, CommitTime: child.CommitTime.Format(time.RFC3339),
		})
	}
	if err := assignDepths(nodes); err != nil {
		return model.BranchGraph{}, err
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth == nodes[j].Depth {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].Depth < nodes[j].Depth
	})
	return model.NewGraph(cfg.Remote, cfg.MainPatterns, nodes), nil
}

func (r Repository) parentFor(ctx context.Context, child Ref, refs []Ref, overrides map[string]string) (string, string, error) {
	if parent := strings.TrimSpace(overrides[child.Name]); parent != "" {
		if parent == "__root__" {
			return "", "", nil
		}
		for _, ref := range refs {
			if ref.Name == parent {
				fork, err := r.run(ctx, "merge-base", child.Hash, ref.Hash)
				return parent, fork, err
			}
		}
		return "", "", fmt.Errorf("分支 %s 的 parent_overrides 指向未匹配分支 %s", child.Name, parent)
	}
	if child.Name == "main" || child.Name == "master" {
		return "", "", nil
	}

	type candidate struct {
		ref      Ref
		fork     string
		distance int
		direct   bool
	}
	var candidates []candidate
	for _, parent := range refs {
		if parent.Name == child.Name {
			continue
		}
		fork, err := r.run(ctx, "merge-base", child.Hash, parent.Hash)
		if err != nil || fork == "" {
			continue
		}
		countText, err := r.run(ctx, "rev-list", "--count", fork+".."+child.Hash)
		if err != nil {
			continue
		}
		distance, _ := strconv.Atoi(countText)
		direct := fork == parent.Hash
		candidates = append(candidates, candidate{ref: parent, fork: fork, distance: distance, direct: direct})
	}
	if len(candidates) == 0 {
		return "", "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].direct != candidates[j].direct {
			return candidates[i].direct
		}
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}
		return candidates[i].ref.CommitTime.After(candidates[j].ref.CommitTime)
	})
	best := candidates[0]
	if len(candidates) > 1 {
		next := candidates[1]
		if best.direct == next.direct && best.distance == next.distance && best.ref.CommitTime.Equal(next.ref.CommitTime) {
			return "", "", fmt.Errorf("无法唯一确定 %s 的父主分支，请在 parent_overrides 中指定", child.Name)
		}
	}
	return best.ref.Name, best.fork, nil
}

func assignDepths(nodes []model.BranchNode) error {
	byName := map[string]*model.BranchNode{}
	for i := range nodes {
		byName[nodes[i].Name] = &nodes[i]
	}
	var visit func(*model.BranchNode, map[string]bool) (int, error)
	visit = func(node *model.BranchNode, stack map[string]bool) (int, error) {
		if node.Parent == "" {
			node.Depth = 0
			return 0, nil
		}
		if stack[node.Name] {
			return 0, fmt.Errorf("分支谱系存在循环: %s", node.Name)
		}
		parent := byName[node.Parent]
		if parent == nil {
			return 0, fmt.Errorf("分支 %s 的父分支 %s 不存在", node.Name, node.Parent)
		}
		stack[node.Name] = true
		depth, err := visit(parent, stack)
		delete(stack, node.Name)
		if err != nil {
			return 0, err
		}
		node.Depth = depth + 1
		return node.Depth, nil
	}
	for i := range nodes {
		if _, err := visit(&nodes[i], map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func Chain(graph model.BranchGraph, branch string) ([]model.BranchNode, error) {
	byName := map[string]model.BranchNode{}
	for _, node := range graph.Branches {
		byName[node.Name] = node
	}
	current, ok := byName[branch]
	if !ok {
		return nil, fmt.Errorf("当前分支 %s 不匹配配置的主分支规则", branch)
	}
	var reverse []model.BranchNode
	seen := map[string]bool{}
	for {
		if seen[current.Name] {
			return nil, errors.New("分支谱系存在循环")
		}
		seen[current.Name] = true
		reverse = append(reverse, current)
		if current.Parent == "" {
			break
		}
		next, ok := byName[current.Parent]
		if !ok {
			return nil, fmt.Errorf("缺少父分支 %s", current.Parent)
		}
		current = next
	}
	for i, j := 0, len(reverse)-1; i < j; i, j = i+1, j-1 {
		reverse[i], reverse[j] = reverse[j], reverse[i]
	}
	return reverse, nil
}

func (r Repository) ChangedFiles(ctx context.Context, base, head string) ([]string, error) {
	if base == "" {
		out, err := r.run(ctx, "show", "--root", "--format=", "--name-only", "--diff-filter=ACMRT", head)
		if err != nil {
			return nil, err
		}
		return nonEmptyLines(out), nil
	}
	args := []string{"diff", "--name-only", "--diff-filter=ACMRT"}
	args = append(args, base+".."+head)
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

func (r Repository) AllFiles(ctx context.Context, head string) ([]string, error) {
	out, err := r.run(ctx, "ls-tree", "-r", "--name-only", head)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

func (r Repository) Log(ctx context.Context, base, head string) (string, error) {
	rangeArg := head
	if base != "" {
		rangeArg = base + ".." + head
	}
	return r.run(ctx, "log", "--reverse", "--format=%H%x09%s", rangeArg)
}

func (r Repository) Commits(ctx context.Context, base, head string) ([]model.Commit, error) {
	rangeArg := head
	if base != "" {
		rangeArg = base + ".." + head
	}
	format := "%H%x00%P%x00%ct%x00%s%x00%b%x1e"
	out, err := r.run(ctx, "log", "--reverse", "--format="+format, rangeArg)
	if err != nil {
		return nil, err
	}
	var commits []model.Commit
	for _, record := range strings.Split(out, "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x00", 5)
		if len(parts) != 5 {
			continue
		}
		parent := ""
		if fields := strings.Fields(parts[1]); len(fields) > 0 {
			parent = fields[0]
		}
		unix, _ := strconv.ParseInt(parts[2], 10, 64)
		files, err := r.ChangedFiles(ctx, parent, parts[0])
		if err != nil {
			return nil, err
		}
		commits = append(commits, model.Commit{
			Hash:      parts[0],
			Parent:    parent,
			Subject:   parts[3],
			Body:      strings.TrimSpace(parts[4]),
			Files:     files,
			Timestamp: time.Unix(unix, 0).UTC().Format(time.RFC3339),
		})
	}
	return commits, nil
}

func (r Repository) Diff(ctx context.Context, base, head string, maxBytes int) (string, error) {
	if base == "" {
		out, err := r.run(ctx, "show", "--root", "--find-renames", "--stat", "--patch", "--diff-filter=ACMRT", "--format=fuller", head)
		if err != nil {
			return "", err
		}
		if maxBytes > 0 && len(out) > maxBytes {
			return out[:maxBytes] + "\n\n[docs-seed: diff truncated]\n", nil
		}
		return out, nil
	}
	args := []string{"diff", "--find-renames", "--stat", "--patch", "--diff-filter=ACMRT"}
	args = append(args, base+".."+head)
	out, err := r.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		return out[:maxBytes] + "\n\n[docs-seed: diff truncated]\n", nil
	}
	return out, nil
}

func (r Repository) Snapshot(ctx context.Context, ref string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "docs-seed-snapshot-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	archive := exec.CommandContext(ctx, "git", "archive", ref)
	archive.Dir = r.Root
	tar := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	reader, err := archive.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	tar.Stdin = reader
	var archiveErr, tarErr bytes.Buffer
	archive.Stderr, tar.Stderr = &archiveErr, &tarErr
	if err := tar.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := archive.Start(); err != nil {
		_ = tar.Process.Kill()
		cleanup()
		return "", nil, err
	}
	if err := archive.Wait(); err != nil {
		_ = tar.Process.Kill()
		cleanup()
		return "", nil, fmt.Errorf("git archive: %w: %s", err, archiveErr.String())
	}
	if err := tar.Wait(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("tar: %w: %s", err, tarErr.String())
	}
	return filepath.Clean(dir), cleanup, nil
}

func nonEmptyLines(text string) []string {
	var result []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func CopyStream(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}
