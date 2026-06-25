package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/Makia9879/docs-seed/internal/gitx"
	"github.com/Makia9879/docs-seed/internal/model"
	"github.com/Makia9879/docs-seed/internal/storage"
	"github.com/stretchr/testify/require"
)

type fakeGenerator struct{}

func (fakeGenerator) Generate(_ context.Context, _ string, prompt string) (model.Fact, error) {
	mode := "全量"
	if strings.Contains(prompt, "增量 文档事实") {
		mode = "增量"
	}
	if strings.Contains(prompt, "只处理一个 commit") {
		mode = "提交"
	}
	return model.Fact{
		BusinessLogic:         []string{mode + "业务规则"},
		DataFlow:              []string{mode + "数据从入口流向存储"},
		ArchitectureDecisions: []string{mode + "架构决策来自源码边界"},
		Evidence:              []model.Evidence{{Path: "service/order.go", Description: "业务证据"}},
	}, nil
}

func (fakeGenerator) GenerateCommits(_ context.Context, _ string, prompt string) ([]model.CommitFact, error) {
	var result []model.CommitFact
	for _, hash := range hashesFromPromptOrMaterial(prompt) {
		result = append(result, model.CommitFact{
			Commit: model.Commit{Hash: hash},
			Fact: model.Fact{
				BusinessLogic:         []string{"提交业务规则"},
				DataFlow:              []string{"提交数据从入口流向存储"},
				ArchitectureDecisions: []string{"提交架构决策来自源码边界"},
				Evidence:              []model.Evidence{{Path: "service/order.go", Description: "业务证据"}},
			},
		})
	}
	return result, nil
}

func (fakeGenerator) Write(_ context.Context, workDir, prompt string, _ ...string) (string, error) {
	output := workDir
	hashes := hashesFromPromptOrMaterial(prompt)
	if strings.Contains(prompt, "写入目录：") {
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "写入目录：") {
				output = strings.TrimSpace(strings.TrimPrefix(line, "写入目录："))
				break
			}
		}
	}
	if len(hashes) == 0 && strings.Contains(prompt, "当前 commit：") {
		hashes = append(hashes, hashFromCurrentCommitLine(prompt))
	}
	if len(hashes) == 0 {
		hashes = append(hashes, "unknown")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"README.md", "business-logic.md", "data-flow.md", "adr.md"} {
		if err := os.WriteFile(filepath.Join(output, name), []byte("# "+name+"\n\nprocessed "+strings.Join(hashes, "\nprocessed ")+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	var evolution strings.Builder
	evolution.WriteString("# commit-evolution.md\n\n")
	for _, hash := range hashes {
		fmt.Fprintf(&evolution, "## %s root business\n\n- 业务演进事实。\n\n", hash)
	}
	return "written", os.WriteFile(filepath.Join(output, "commit-evolution.md"), []byte(evolution.String()), 0o644)
}

type noOpWriteGenerator struct {
	fakeGenerator
}

func (noOpWriteGenerator) Write(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	return "I did not edit files.", nil
}

type countingNoRecordWriteGenerator struct {
	fakeGenerator
	calls atomic.Int32
}

func (g *countingNoRecordWriteGenerator) Write(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	g.calls.Add(1)
	return "I did not edit files.", nil
}

type countingWriteGenerator struct {
	count              int
	archiveSummaries   int
	archiveSummaryText string
	batches            []int
}

func (g *countingWriteGenerator) Generate(ctx context.Context, workDir, prompt string) (model.Fact, error) {
	return fakeGenerator{}.Generate(ctx, workDir, prompt)
}

func (g *countingWriteGenerator) GenerateCommits(ctx context.Context, workDir, prompt string) ([]model.CommitFact, error) {
	return fakeGenerator{}.GenerateCommits(ctx, workDir, prompt)
}

func (g *countingWriteGenerator) Write(ctx context.Context, workDir, prompt string, addDirs ...string) (string, error) {
	g.count++
	if strings.Contains(prompt, "归档汇总校准 Agent") {
		g.archiveSummaries++
		g.archiveSummaryText = prompt
		requireArchiveSummaryDocs(workDir)
		return "archive summary updated", nil
	}
	hashes := hashesFromPromptOrMaterial(prompt)
	if len(hashes) == 0 && strings.Contains(prompt, "当前 commit：") {
		hashes = append(hashes, hashFromCurrentCommitLine(prompt))
	}
	g.batches = append(g.batches, len(hashes))
	return fakeGenerator{}.Write(ctx, workDir, prompt, addDirs...)
}

func requireArchiveSummaryDocs(workDir string) {
	for _, name := range []string{"business-logic.md", "data-flow.md", "adr.md"} {
		path := filepath.Join(workDir, name)
		data, _ := os.ReadFile(path)
		_ = os.WriteFile(path, append(data, []byte("\n归档历史已纳入最终总结。\n")...), 0o644)
	}
}

type countingCommitGenerator struct {
	fakeGenerator
	batches []int
}

func (g *countingCommitGenerator) GenerateCommits(ctx context.Context, workDir, prompt string) ([]model.CommitFact, error) {
	hashes := hashesFromPromptOrMaterial(prompt)
	g.batches = append(g.batches, len(hashes))
	return fakeGenerator{}.GenerateCommits(ctx, workDir, prompt)
}

type flakyCommitGenerator struct {
	fakeGenerator
	failures int
	calls    int
}

func (g *flakyCommitGenerator) GenerateCommits(ctx context.Context, workDir, prompt string) ([]model.CommitFact, error) {
	g.calls++
	if g.calls <= g.failures {
		return nil, fmt.Errorf("temporary agent failure %d", g.calls)
	}
	return fakeGenerator{}.GenerateCommits(ctx, workDir, prompt)
}

type alwaysFailWriteGenerator struct {
	fakeGenerator
	calls atomic.Int32
}

func (g *alwaysFailWriteGenerator) Write(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	g.calls.Add(1)
	return "", errors.New("agent write failed")
}

func TestLearnAndGenerateCurrentChain(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")
	runGit(t, root, "checkout", "-b", "llm/order-v2")
	writeFile(t, root, "service/order.go", "package service\n// order v2\n")
	runGit(t, root, "commit", "-am", "order v2")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main", "llm/**"}
	require.NoError(t, config.Save(root, cfg))
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: fakeGenerator{},
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "llm/order-v2")
	require.NoError(t, err)
	require.Len(t, chain, 2)

	changed, err := instance.LearnChain(context.Background(), chain, false, true)
	require.NoError(t, err)
	require.Equal(t, 2, changed)
	require.NoError(t, instance.GenerateChain(chain))

	output := filepath.Join(root, ".docs-seed", "docs")
	rootDoc := readFile(t, filepath.Join(output, "branches", "main", "README.md"))
	childDoc := readFile(t, filepath.Join(output, "branches", "llm__order-v2", "README.md"))
	business := readFile(t, filepath.Join(output, "branches", "llm__order-v2", "business-logic.md"))
	adr := readFile(t, filepath.Join(output, "branches", "llm__order-v2", "adr.md"))
	require.Contains(t, rootDoc, "全量基线")
	require.Contains(t, childDoc, "相对父主分支的增量")
	require.Contains(t, childDoc, "main → llm/order-v2")
	require.Contains(t, childDoc, "ADR")
	require.Contains(t, business, "增量业务规则")
	require.Contains(t, business, "证据附录")
	require.Contains(t, adr, "增量架构决策来自源码边界")
	require.Contains(t, adr, "证据附录")
	require.NotContains(t, business, "调用方式")

	changed, err = instance.LearnChain(context.Background(), chain, false, true)
	require.NoError(t, err)
	require.Zero(t, changed)
}

func TestValidateFactRejectsInvocationInstructions(t *testing.T) {
	err := validateFact(model.Fact{
		BusinessLogic:         []string{"使用 curl /orders 调用接口"},
		DataFlow:              []string{"数据进入订单系统"},
		ArchitectureDecisions: []string{"调用方式由控制器暴露"},
	})
	require.ErrorContains(t, err, "curl")
}

func TestLearnEvolutionCachesCommitsAndGeneratesEvolutionDoc(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")
	writeFile(t, root, "service/order.go", "package service\n// approve order\n")
	runGit(t, root, "commit", "-am", "approve order")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: fakeGenerator{},
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	changed, err := instance.LearnChainEvolution(context.Background(), chain, false, 0)
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	require.NoError(t, instance.GenerateChain(chain))

	output := filepath.Join(root, ".docs-seed", "docs", "branches", "main")
	evolution := readFile(t, filepath.Join(output, "commit-evolution.md"))
	require.Contains(t, evolution, "提交演进")
	require.Contains(t, evolution, "root business")
	require.Contains(t, evolution, "approve order")
	require.Contains(t, evolution, "提交业务规则")
}

func TestLearnEvolutionUsesConfiguredCommitBatchSize(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	for i := 1; i <= 5; i++ {
		writeFile(t, root, "service/order.go", strings.Repeat("package service\n", i))
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-m", "order batch")
	}

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &countingCommitGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	changed, err := instance.LearnChainEvolution(context.Background(), chain, false, 3)
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	require.Equal(t, []int{3, 2}, generator.batches)
}

func TestLearnEvolutionSplitsOversizedCommitBatch(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	for i := 1; i <= 5; i++ {
		writeFile(t, root, "service/order.go", "package service\n"+strings.Repeat(fmt.Sprintf("// order rule %d\n", i), i*80))
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-m", fmt.Sprintf("order batch %d", i))
	}

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	cfg.Evolution.MaxBatchBytes = 1
	require.NoError(t, config.Save(root, cfg))
	generator := &countingCommitGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	changed, err := instance.LearnChainEvolution(context.Background(), chain, false, 5)
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	require.Equal(t, []int{1, 1, 1, 1, 1}, generator.batches)
}

func TestLearnEvolutionRetriesAgentBatchInFreshGoroutines(t *testing.T) {
	oldRetryDelay := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = oldRetryDelay })

	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &flakyCommitGenerator{failures: 1}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	changed, err := instance.LearnChainEvolution(context.Background(), chain, false, 0)
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	require.Equal(t, 2, generator.calls)
}

func TestCommitBatchPromptPointsToMaterialFile(t *testing.T) {
	commits := []model.Commit{{
		Hash:    strings.Repeat("a", 40),
		Parent:  strings.Repeat("b", 40),
		Subject: "add settlement workflow",
		Files:   []string{"service/settlement.go"},
		Diff:    "diff --git a/service/settlement.go b/service/settlement.go\n+secret inline diff should stay in material\n",
	}}
	materialPath := "/repo/.docs-seed-agent-material/main-0001-0001-commits.md"
	prompt := buildCommitBatchPrompt(model.BranchNode{Name: "main"}, "full", "", materialPath, commits, []int{0}, 1)

	require.Contains(t, prompt, materialPath)
	require.Contains(t, prompt, "请先读取这个材料文件")
	require.NotContains(t, prompt, "secret inline diff")
	require.NotContains(t, prompt, "diff --git")
}

func TestCommitBatchMaterialContainsOnlyCurrentBatch(t *testing.T) {
	node := model.BranchNode{Name: "main", Tip: strings.Repeat("f", 40)}
	first := model.Commit{
		Hash:    strings.Repeat("a", 40),
		Parent:  strings.Repeat("0", 40),
		Subject: "first batch commit",
		Files:   []string{"service/first.go"},
		Diff:    "+first batch detail\n",
	}
	second := model.Commit{
		Hash:    strings.Repeat("b", 40),
		Parent:  first.Hash,
		Subject: "second batch commit",
		Files:   []string{"service/second.go"},
		Diff:    "+second batch detail\n",
	}

	material := buildCommitBatchMaterial(node, "full", "", []model.Commit{second}, []int{1}, 2)
	prompt := buildCommitBatchPrompt(node, "full", "", "/repo/.docs-seed-agent-material/main-0002-0002-commits.md", []model.Commit{second}, []int{1}, 2)

	require.Contains(t, material, second.Hash)
	require.Contains(t, material, "second batch detail")
	require.NotContains(t, material, "first batch detail")
	require.NotContains(t, material, "first batch commit")
	require.NotContains(t, material, "service/first.go")
	require.Contains(t, prompt, second.Hash)
	require.NotContains(t, prompt, first.Hash)
}

func TestDirectWriteBatchMaterialIncludesArchivePaths(t *testing.T) {
	output := t.TempDir()
	node := model.BranchNode{Name: "main", Tip: strings.Repeat("f", 40)}
	dir := storage.BranchDocDir(output, node.Name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "archive"), 0o755))
	require.NoError(t, os.MkdirAll(directCheckpointArchiveDir(output), 0o755))
	require.NoError(t, os.WriteFile(commitEvolutionArchivePath(dir), []byte("# archive\n\n## "+strings.Repeat("a", 40)+"\n\n- archived fact\n"), 0o644))
	require.NoError(t, os.WriteFile(directCheckpointArchivePath(output, node.Name), []byte(`{"branch":"main","commit":{"hash":"`+strings.Repeat("b", 40)+`"}}`+"\n"), 0o644))
	item := directCommitBatchItem{
		Commit: model.Commit{
			Hash:    strings.Repeat("c", 40),
			Parent:  strings.Repeat("b", 40),
			Subject: "current change",
			Files:   []string{"service/current.go"},
			Diff:    "+current\n",
		},
		Index: 2,
	}

	material := buildDirectWriteCommitBatchMaterial(output, node, "full", "", []model.BranchNode{node}, []directCommitBatchItem{item}, 3)
	prompt := buildDirectWriteCommitBatchPrompt(output, node, "full", "", []model.BranchNode{node}, "/tmp/material.md", []directCommitBatchItem{item}, 3)

	require.Contains(t, material, "## Archived material")
	require.Contains(t, material, "commit_evolution_archive: "+commitEvolutionArchivePath(dir))
	require.Contains(t, material, "checkpoint_archive: "+directCheckpointArchivePath(output, node.Name))
	require.Contains(t, material, item.Commit.Hash)
	require.Contains(t, prompt, "archived material")
	require.Contains(t, prompt, "查重/追溯索引")
	require.Contains(t, prompt, "不要批量读取完整归档文件")
	require.Contains(t, prompt, "保留已有最终总结中的历史业务事实")
}

func TestDirectArchiveSummaryMaterialIncludesBoundedArchiveExcerpts(t *testing.T) {
	output := t.TempDir()
	node := model.BranchNode{Name: "main", Tip: strings.Repeat("f", 40)}
	dir := storage.BranchDocDir(output, node.Name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "archive"), 0o755))
	require.NoError(t, os.MkdirAll(directCheckpointArchiveDir(output), 0o755))
	archivePath := commitEvolutionArchivePath(dir)
	archiveContent := strings.Repeat("older archive fact\n", 5000) + "recent archive fact\n"
	require.NoError(t, os.WriteFile(archivePath, []byte(archiveContent), 0o644))
	require.NoError(t, os.WriteFile(directCheckpointArchivePath(output, node.Name), []byte("recent checkpoint\n"), 0o644))

	material, err := buildDirectArchiveSummaryMaterial(output, node, "full", "", []model.BranchNode{node})
	require.NoError(t, err)

	require.Contains(t, material, "## commit_evolution_archive")
	require.Contains(t, material, fmt.Sprintf("excerpt: last %d bytes only", directArchiveExcerptMaxBytes))
	require.Contains(t, material, "older archive content intentionally omitted")
	require.Contains(t, material, "recent archive fact")
	require.Contains(t, material, "## checkpoint_archive")
	require.Contains(t, material, "recent checkpoint")
	require.Less(t, len(material), len(archiveContent))
	require.Less(t, len(material), 30*1024)
}

func TestDirectArchiveSummaryPromptRequiresSmallRangeReads(t *testing.T) {
	output := t.TempDir()
	node := model.BranchNode{Name: "main", Tip: strings.Repeat("f", 40)}

	prompt := buildDirectArchiveSummaryPrompt(output, node, "full", "", []model.BranchNode{node}, "/tmp/main-archive-summary.md")

	require.Contains(t, prompt, "每次 Read 的 limit 不超过 120 行")
	require.Contains(t, prompt, "优先使用 Grep")
	require.Contains(t, prompt, "禁止读取完整 archive/commit-evolution.md")
	require.Contains(t, prompt, "禁止整文件回读")
	require.Contains(t, prompt, "编辑成功后不要回读整文件")
	require.Contains(t, prompt, "工具成功返回即视为写入完成")
	require.Contains(t, prompt, "Read limit<=120")
}

func TestBranchPromptPointsToMaterialFile(t *testing.T) {
	materialPath := "/repo/.docs-seed-agent-material/main-branch.md"
	prompt := buildPrompt(config.Default("sample"), model.BranchNode{Name: "main"}, "full", "", materialPath, true)

	require.Contains(t, prompt, materialPath)
	require.Contains(t, prompt, "请先读取这个材料文件")
	require.NotContains(t, prompt, "commit log")
	require.NotContains(t, prompt, "service/secret.go")
}

func TestWorkspaceSyncUsesRootEvolutionBatchSize(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "service-a")
	require.NoError(t, os.MkdirAll(project, 0o755))
	runGit(t, project, "init", "-b", "main")
	runGit(t, project, "config", "user.email", "docs-seed@example.com")
	runGit(t, project, "config", "user.name", "Docs Seed")
	for i := 1; i <= 5; i++ {
		writeFile(t, project, "service/order.go", strings.Repeat("package service\n", i))
		runGit(t, project, "add", ".")
		runGit(t, project, "commit", "-m", "order batch")
	}

	rootCfg := config.Default("workspace")
	rootCfg.Workspace.Projects = []string{"service-a"}
	rootCfg.Evolution.BatchSize = 2
	require.NoError(t, config.Save(root, rootCfg))
	childCfg := config.Default("service-a")
	childCfg.Branches.MainPatterns = []string{"main"}
	childCfg.Evolution.BatchSize = 8
	require.NoError(t, config.Save(project, childCfg))

	generator := &countingCommitGenerator{}
	instance := &App{
		Root: root, Config: rootCfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	require.NoError(t, instance.SyncWorkspace(context.Background(), false, true, false, 0, 2))

	require.Equal(t, []int{2, 2, 1}, generator.batches)
}

func TestGenerateChainDirectWritesDocumentsWithoutParsingJSON(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: fakeGenerator{},
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)
	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 0))

	output := filepath.Join(root, ".docs-seed", "docs", "branches", "main")
	require.FileExists(t, filepath.Join(output, "README.md"))
	evolution := readFile(t, filepath.Join(output, "commit-evolution.md"))
	require.Contains(t, evolution, "root business")
	checkpoint := readFile(t, filepath.Join(root, ".docs-seed", "docs", directCheckpointFile))
	require.Contains(t, checkpoint, "root business")
	require.Contains(t, checkpoint, "processed_commits")
}

func TestGenerateChainDirectUsesConfiguredCommitBatchSize(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	for i := 1; i <= 5; i++ {
		writeFile(t, root, "service/order.go", strings.Repeat("package service\n", i))
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-m", fmt.Sprintf("direct batch %d", i))
	}

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &countingWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 3))
	require.Equal(t, []int{3, 2}, generator.batches)
}

func TestGenerateChainDirectSummarizesArchivesWhenCompactionHappensAtEnd(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	for i := 1; i <= 3; i++ {
		writeFile(t, root, "service/order.go", strings.Repeat("package service\n", i))
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-m", fmt.Sprintf("direct archive %d", i))
	}

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	cfg.Evolution.DirectKeepRecent = 1
	require.NoError(t, config.Save(root, cfg))
	generator := &countingWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 3))

	output := filepath.Join(root, ".docs-seed", "docs")
	dir := filepath.Join(output, "branches", "main")
	require.Equal(t, []int{3}, generator.batches)
	require.GreaterOrEqual(t, generator.archiveSummaries, 1)
	require.Contains(t, generator.archiveSummaryText, "归档汇总校准 Agent")
	require.Contains(t, generator.archiveSummaryText, "有界尾部片段")
	require.Contains(t, generator.archiveSummaryText, "不要一次性读取完整归档文件")
	require.FileExists(t, filepath.Join(dir, "archive", "commit-evolution.md"))
	require.FileExists(t, directCheckpointArchivePath(output, "main"))
	require.Contains(t, readFile(t, filepath.Join(dir, "business-logic.md")), "归档历史已纳入最终总结")
	require.Contains(t, readFile(t, filepath.Join(dir, "data-flow.md")), "归档历史已纳入最终总结")
	require.Contains(t, readFile(t, filepath.Join(dir, "adr.md")), "归档历史已纳入最终总结")
}

func TestGenerateChainDirectSplitsOversizedCommitBatch(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	for i := 1; i <= 5; i++ {
		writeFile(t, root, "service/order.go", "package service\n"+strings.Repeat(fmt.Sprintf("// direct rule %d\n", i), i*80))
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-m", fmt.Sprintf("direct batch %d", i))
	}

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	cfg.Evolution.MaxBatchBytes = 1
	require.NoError(t, config.Save(root, cfg))
	generator := &countingWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 5))
	require.Equal(t, []int{1, 1, 1, 1, 1}, generator.batches)
}

func TestValidateDirectWriteBatchAcceptsHashInAnyResultDoc(t *testing.T) {
	dir := t.TempDir()
	hash := strings.Repeat("a", 40)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "business-logic.md"), []byte("# business\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data-flow.md"), []byte("# data\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "adr.md"), []byte("# adr\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "commit-evolution.md"), []byte("# commits\n"), 0o644))
	before, err := snapshotDirectDocs(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "business-logic.md"), []byte("handled "+hash[:11]+"\n"), 0o644))

	err = validateDirectWriteBatchResult(dir, []directCommitBatchItem{{
		Commit: model.Commit{Hash: hash},
		Index:  0,
	}}, before)
	require.NoError(t, err)
}

func TestDirectRecordAppendsMissingHashes(t *testing.T) {
	dir := t.TempDir()
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "commit-evolution.md"), []byte("# commits\n\n## "+first+"\n\n"), 0o644))

	require.NoError(t, DirectRecord(dir, []string{first, second}, "test"))
	require.NoError(t, DirectRecord(dir, []string{second}, "test"))

	text := readFile(t, filepath.Join(dir, "commit-evolution.md"))
	require.Contains(t, text, first)
	require.Contains(t, text, second)
	require.Equal(t, 1, strings.Count(text, second))
}

func TestCompactCommitEvolutionArchivesOldSectionsAndPreservesLookup(t *testing.T) {
	dir := t.TempDir()
	hashes := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 40),
	}
	var body strings.Builder
	body.WriteString("# main：提交演进\n\n")
	for i, hash := range hashes {
		fmt.Fprintf(&body, "## %s commit %d\n\n- fact %d\n\n", hash, i+1, i+1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "commit-evolution.md"), []byte(body.String()), 0o644))

	archived, err := compactCommitEvolutionDoc(dir, "main", 1)
	require.NoError(t, err)
	require.True(t, archived)

	active := readFile(t, filepath.Join(dir, "commit-evolution.md"))
	archive := readFile(t, filepath.Join(dir, "archive", "commit-evolution.md"))
	require.NotContains(t, active, hashes[0])
	require.NotContains(t, active, hashes[1])
	require.Contains(t, active, hashes[2])
	require.Contains(t, active, "archive/commit-evolution.md")
	require.Contains(t, archive, hashes[0])
	require.Contains(t, archive, hashes[1])

	recorded, err := directCommitRecorded(dir, model.Commit{Hash: hashes[0]})
	require.NoError(t, err)
	require.True(t, recorded)
}

func TestSaveDirectCheckpointArchivesOldProcessedCommits(t *testing.T) {
	output := t.TempDir()
	hashes := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 40),
	}
	checkpoint := directCheckpoint{
		Version: 1,
		Branches: map[string]directCheckpointBranch{
			"main": {
				Branch:    "main",
				Processed: map[string]directCheckpointCommit{},
			},
		},
	}
	for i, hash := range hashes {
		checkpoint.Branches["main"].Processed[hash] = directCheckpointCommit{
			Hash: hash, ShortHash: short(hash), Subject: fmt.Sprintf("commit %d", i+1), Index: i + 1, Total: len(hashes),
		}
	}

	archived, err := saveDirectCheckpoint(output, &checkpoint, 1)
	require.NoError(t, err)
	require.True(t, archived)

	loaded, err := loadDirectCheckpoint(output)
	require.NoError(t, err)
	require.True(t, directCheckpointHas(loaded, "main", hashes[0]))
	require.True(t, directCheckpointHas(loaded, "main", hashes[1]))
	require.True(t, directCheckpointHas(loaded, "main", hashes[2]))
	require.Len(t, loaded.Branches["main"].Processed, 1)
	archive := readFile(t, directCheckpointArchivePath(output, "main"))
	require.Contains(t, archive, hashes[0])
	require.Contains(t, archive, hashes[1])
}

func TestSaveDirectCheckpointUpdatesInMemoryArchiveIndex(t *testing.T) {
	output := t.TempDir()
	hashes := []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
	}
	checkpoint := directCheckpoint{
		Version: 1,
		Branches: map[string]directCheckpointBranch{
			"main": {
				Branch:    "main",
				Processed: map[string]directCheckpointCommit{},
			},
		},
	}
	for i, hash := range hashes {
		checkpoint.Branches["main"].Processed[hash] = directCheckpointCommit{
			Hash: hash, ShortHash: short(hash), Subject: fmt.Sprintf("commit %d", i+1), Index: i + 1, Total: len(hashes),
		}
	}

	archived, err := saveDirectCheckpoint(output, &checkpoint, 1)
	require.NoError(t, err)
	require.True(t, archived)
	require.True(t, directCheckpointHas(checkpoint, "main", hashes[0]))
	require.True(t, directCheckpointHas(checkpoint, "main", hashes[1]))
}

func TestGenerateChainDirectFailsWhenAgentDoesNotRecordCommit(t *testing.T) {
	oldRetryDelay := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = oldRetryDelay })

	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &countingNoRecordWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if generator.calls.Load() >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	err = instance.GenerateChainDirect(ctx, chain, 0, 0)
	require.ErrorContains(t, err, "已停止重试")
	require.ErrorContains(t, err, "Agent 未在结果文档中记录当前提交 hash")
}

func TestGenerateChainDirectRetriesUntilContextCancellation(t *testing.T) {
	oldRetryDelay := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = oldRetryDelay })

	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &alwaysFailWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if generator.calls.Load() >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	err = instance.GenerateChainDirect(ctx, chain, 0, 0)
	require.ErrorContains(t, err, "已停止重试")
	require.GreaterOrEqual(t, int(generator.calls.Load()), 2)
}

func TestGenerateChainDirectResumesFromCheckpointAndExistingDocs(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &countingWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 0))
	require.Equal(t, 1, generator.count)
	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 0))
	require.Equal(t, 1, generator.count)
}

func TestGenerateChainDirectResumesFromCheckpointEvenWhenDocsMissHash(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "docs-seed@example.com")
	runGit(t, root, "config", "user.name", "Docs Seed")
	writeFile(t, root, "service/order.go", "package service\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "root business")

	cfg := config.Default("sample")
	cfg.Branches.MainPatterns = []string{"main"}
	require.NoError(t, config.Save(root, cfg))
	generator := &countingWriteGenerator{}
	instance := &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: generator,
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 0))
	require.Equal(t, 1, generator.count)

	output := filepath.Join(root, ".docs-seed", "docs", "branches", "main")
	for _, name := range directRecordDocNames() {
		require.NoError(t, os.WriteFile(filepath.Join(output, name), []byte("# "+name+"\n\nhash removed\n"), 0o644))
	}

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0, 0))
	require.Equal(t, 1, generator.count)
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func hashesFromPromptOrMaterial(prompt string) []string {
	if hashes := hashesFromPrompt(prompt); len(hashes) > 0 {
		return hashes
	}
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, ".docs-seed-agent-material") {
			continue
		}
		data, err := os.ReadFile(line)
		if err != nil {
			continue
		}
		if hashes := hashesFromPrompt(string(data)); len(hashes) > 0 {
			return hashes
		}
	}
	return nil
}

func hashesFromPrompt(prompt string) []string {
	var hashes []string
	seen := map[string]bool{}
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "hash: ") {
			hash := strings.TrimSpace(strings.TrimPrefix(line, "hash: "))
			if hash != "" && !seen[hash] {
				seen[hash] = true
				hashes = append(hashes, hash)
			}
		}
		if strings.HasPrefix(line, "- ") {
			fields := strings.Fields(strings.TrimPrefix(line, "- "))
			if len(fields) > 0 && isHexHash(fields[0]) && !seen[fields[0]] {
				seen[fields[0]] = true
				hashes = append(hashes, fields[0])
			}
		}
	}
	return hashes
}

func hashFromCurrentCommitLine(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "当前 commit：") {
			fields := strings.Fields(strings.TrimPrefix(line, "当前 commit："))
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return "unknown"
}

func isHexHash(value string) bool {
	if len(value) < 12 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
