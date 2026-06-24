package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Makia9879/docs-seed/internal/agent"
	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/Makia9879/docs-seed/internal/gitx"
	"github.com/Makia9879/docs-seed/internal/model"
	"github.com/Makia9879/docs-seed/internal/storage"
)

type App struct {
	Root      string
	Config    config.Config
	Repo      gitx.Repository
	Generator agent.Generator
}

func Open(root string) (*App, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	return &App{
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root},
		Generator: agent.Runner{Config: cfg.Agent},
	}, nil
}

func Init(root string, workspace bool) error {
	if _, err := os.Stat(filepath.Join(root, config.FileName)); err == nil {
		return fmt.Errorf("%s 已存在", config.FileName)
	}
	if _, err := gitx.DiscoverRoot(root); err != nil {
		return err
	}
	cfg := config.Default(filepath.Base(root))
	if workspace {
		cfg.Workspace.Projects = []string{}
	}
	if err := config.Save(root, cfg); err != nil {
		return err
	}
	return storage.Ensure(root)
}

func (a *App) SyncBranches(ctx context.Context, fetch bool) (model.BranchGraph, error) {
	if fetch {
		done := traceStep("git fetch")
		if err := a.Repo.Fetch(ctx, a.Config.Branches.Remote); err != nil {
			done()
			return model.BranchGraph{}, err
		}
		done()
	}
	done := traceStep("构建分支谱系")
	graph, err := a.Repo.BuildGraph(ctx, a.Config.Branches)
	if err != nil {
		done()
		return model.BranchGraph{}, err
	}
	done()
	if err := storage.Ensure(a.Root); err != nil {
		return model.BranchGraph{}, err
	}
	done = traceStep("保存分支谱系")
	if err := storage.SaveGraph(a.Root, graph); err != nil {
		done()
		return model.BranchGraph{}, err
	}
	done()
	return graph, nil
}

func (a *App) CurrentChain(ctx context.Context, graph model.BranchGraph, selected string) ([]model.BranchNode, error) {
	if selected == "" {
		var err error
		selected, err = a.Repo.CurrentBranch(ctx)
		if err != nil {
			return nil, err
		}
	}
	return gitx.Chain(graph, selected)
}

func (a *App) LearnNode(ctx context.Context, node model.BranchNode, force bool, history bool) (model.Fact, bool, error) {
	mode, base := "full", ""
	if node.Parent != "" {
		mode, base = "incremental", node.ForkPoint
	}
	if !force {
		if existing, err := storage.LoadFact(a.Root, node.Name); err == nil &&
			existing.HeadCommit == node.Tip && existing.BaseCommit == base && existing.Parent == node.Parent {
			return existing, false, nil
		}
	}

	var files []string
	var err error
	if mode == "full" {
		done := traceStep("  收集全量文件")
		files, err = a.Repo.AllFiles(ctx, node.Tip)
		done()
	} else {
		done := traceStep("  收集增量文件")
		files, err = a.Repo.ChangedFiles(ctx, base, node.Tip)
		done()
	}
	if err != nil {
		return model.Fact{}, false, err
	}
	files = filterFiles(files, a.Config.Exclude)
	done := traceStep("  读取提交日志")
	logText, err := a.Repo.Log(ctx, base, node.Tip)
	if err != nil {
		done()
		return model.Fact{}, false, err
	}
	done()
	done = traceStep("  创建源码快照")
	snapshot, cleanup, err := a.Repo.Snapshot(ctx, node.Tip)
	if err != nil {
		done()
		return model.Fact{}, false, err
	}
	done()
	defer cleanup()

	prompt := buildPrompt(a.Config, node, mode, base, files, logText, history)
	done = traceStep("  Agent 生成分支事实")
	fact, err := a.Generator.Generate(ctx, snapshot, prompt)
	if err != nil {
		done()
		return model.Fact{}, false, err
	}
	done()
	fact.Version = 1
	fact.Branch = node.Name
	fact.Parent = node.Parent
	fact.Mode = mode
	fact.BaseCommit = base
	fact.HeadCommit = node.Tip
	fact.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if err := validateFact(fact); err != nil {
		return model.Fact{}, false, err
	}
	done = traceStep("  保存分支事实")
	if err := storage.SaveFact(a.Root, fact); err != nil {
		done()
		return model.Fact{}, false, err
	}
	done()
	return fact, true, nil
}

func (a *App) LearnChain(ctx context.Context, chain []model.BranchNode, force bool, history bool) (int, error) {
	changed := 0
	for i, node := range chain {
		fmt.Printf("[%d/%d] 学习分支 %s\n", i+1, len(chain), node.Name)
		_, wrote, err := a.LearnNode(ctx, node, force, history)
		if err != nil {
			return changed, fmt.Errorf("学习分支 %s: %w", node.Name, err)
		}
		if wrote {
			changed++
			fmt.Printf("[%d/%d] 已更新分支事实 %s\n", i+1, len(chain), node.Name)
		} else {
			fmt.Printf("[%d/%d] 分支事实未变化，跳过 %s\n", i+1, len(chain), node.Name)
		}
	}
	return changed, nil
}

func (a *App) LearnNodeEvolution(ctx context.Context, node model.BranchNode, force bool, batchSize int) (model.Fact, bool, error) {
	mode, base := "full", ""
	if node.Parent != "" {
		mode, base = "incremental", node.ForkPoint
	}
	if batchSize <= 0 {
		batchSize = a.Config.Evolution.BatchSize
	}
	if batchSize <= 0 {
		batchSize = 8
	}
	if !force {
		if existing, err := storage.LoadFact(a.Root, node.Name); err == nil &&
			existing.HeadCommit == node.Tip && existing.BaseCommit == base && existing.Parent == node.Parent && existing.Mode == mode {
			return existing, false, nil
		}
	}
	commits, err := a.Repo.Commits(ctx, base, node.Tip)
	if err != nil {
		return model.Fact{}, false, err
	}
	if len(commits) == 0 {
		return emptyBranchFact(node, mode, base), false, storage.SaveFact(a.Root, emptyBranchFact(node, mode, base))
	}
	fmt.Printf("  分支 %s 提交数：%d\n", node.Name, len(commits))
	var commitFacts []model.CommitFact
	var pending []model.Commit
	var pendingIndexes []int
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		facts, err := a.learnCommitBatch(ctx, node, mode, base, pending, pendingIndexes, len(commits))
		if err != nil {
			return err
		}
		commitFacts = append(commitFacts, facts...)
		pending = nil
		pendingIndexes = nil
		return nil
	}
	for i, commit := range commits {
		prefix := fmt.Sprintf("  [%d/%d] %s %s", i+1, len(commits), short(commit.Hash), commit.Subject)
		if !force {
			if existing, err := storage.LoadCommitFact(a.Root, node.Name, commit.Hash); err == nil && existing.Commit.Hash == commit.Hash {
				if err := flushPending(); err != nil {
					return model.Fact{}, false, err
				}
				fmt.Printf("%s - 使用缓存\n", prefix)
				commitFacts = append(commitFacts, existing)
				continue
			}
		}
		filteredFiles := filterFiles(commit.Files, a.Config.Exclude)
		if len(filteredFiles) == 0 {
			if err := flushPending(); err != nil {
				return model.Fact{}, false, err
			}
			fmt.Printf("%s - 跳过，无有效业务文件变化\n", prefix)
			cf := model.CommitFact{
				Version: 1, Branch: node.Name, Commit: commit, Skipped: true,
				Reason: "all changed files are excluded", GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if err := storage.SaveCommitFact(a.Root, cf); err != nil {
				return model.Fact{}, false, err
			}
			commitFacts = append(commitFacts, cf)
			continue
		}
		commit.Files = filteredFiles
		fmt.Printf("%s - 加入批次，变更文件 %d 个\n", prefix, len(filteredFiles))
		pending = append(pending, commit)
		pendingIndexes = append(pendingIndexes, i)
		if len(pending) >= batchSize {
			if err := flushPending(); err != nil {
				return model.Fact{}, false, err
			}
		}
	}
	if err := flushPending(); err != nil {
		return model.Fact{}, false, err
	}
	fact := summarizeEvolution(node, mode, base, commitFacts)
	if err := validateFact(fact); err != nil {
		return model.Fact{}, false, err
	}
	if err := storage.SaveFact(a.Root, fact); err != nil {
		return model.Fact{}, false, err
	}
	return fact, true, nil
}

func (a *App) learnCommitBatch(ctx context.Context, node model.BranchNode, mode, base string, commits []model.Commit, indexes []int, total int) ([]model.CommitFact, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	start, end := indexes[0]+1, indexes[len(indexes)-1]+1
	fmt.Printf("  [%d-%d/%d] 批量分析 %d 个提交\n", start, end, total, len(commits))
	diffMax := a.Config.Evolution.DiffMaxBytes
	if diffMax <= 0 {
		diffMax = 120000
	}
	loaded := make([]model.Commit, 0, len(commits))
	for i, commit := range commits {
		prefix := fmt.Sprintf("    [%d/%d] %s %s", indexes[i]+1, total, short(commit.Hash), commit.Subject)
		done := traceStep(prefix + " 读取 commit diff")
		diff, err := a.Repo.Diff(ctx, commit.Parent, commit.Hash, diffMax)
		if err != nil {
			done()
			return nil, err
		}
		done()
		commit.Diff = diff
		loaded = append(loaded, commit)
	}
	done := traceStep("    创建批量快照")
	snapshot, cleanup, err := a.Repo.Snapshot(ctx, loaded[len(loaded)-1].Hash)
	if err != nil {
		done()
		return nil, err
	}
	done()
	defer cleanup()

	prompt := buildCommitBatchPrompt(node, mode, base, loaded, indexes, total)
	done = traceStep("    Agent 批量分析 commits")
	facts, err := a.generateCommitBatch(ctx, snapshot, prompt)
	if err != nil {
		done()
		return nil, fmt.Errorf("批量分析提交 %s..%s: %w", short(loaded[0].Hash), short(loaded[len(loaded)-1].Hash), err)
	}
	done()
	byHash := map[string]model.CommitFact{}
	for _, cf := range facts {
		byHash[cf.Commit.Hash] = cf
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result := make([]model.CommitFact, 0, len(loaded))
	for _, commit := range loaded {
		cf, ok := byHash[commit.Hash]
		if !ok {
			return nil, fmt.Errorf("Agent 批量输出缺少提交 %s", short(commit.Hash))
		}
		cf.Version = 1
		cf.Branch = node.Name
		cf.Commit = commit
		cf.GeneratedAt = now
		cf.Fact.Version = 1
		cf.Fact.Branch = node.Name
		cf.Fact.Parent = node.Parent
		cf.Fact.Mode = "commit"
		cf.Fact.BaseCommit = commit.Parent
		cf.Fact.HeadCommit = commit.Hash
		cf.Fact.GeneratedAt = now
		if len(cf.Fact.BusinessLogic) == 0 && len(cf.Fact.DataFlow) == 0 && len(cf.Fact.ArchitectureDecisions) == 0 {
			cf.Skipped = true
			if strings.TrimSpace(cf.Reason) == "" {
				cf.Reason = "no verifiable business impact"
			}
		}
		if !cf.Skipped {
			if err := validateFact(cf.Fact); err != nil {
				return nil, fmt.Errorf("提交 %s: %w", short(commit.Hash), err)
			}
		}
		done = traceStep("    保存 commit 事实 " + short(commit.Hash))
		if err := storage.SaveCommitFact(a.Root, cf); err != nil {
			done()
			return nil, err
		}
		done()
		fmt.Printf("    %s - 完成\n", short(commit.Hash))
		result = append(result, cf)
	}
	return result, nil
}

func (a *App) generateCommitBatch(ctx context.Context, snapshot, prompt string) ([]model.CommitFact, error) {
	return a.Generator.GenerateCommits(ctx, snapshot, prompt)
}

func (a *App) LearnChainEvolution(ctx context.Context, chain []model.BranchNode, force bool, batchSize int) (int, error) {
	changed := 0
	for i, node := range chain {
		fmt.Printf("[%d/%d] 演进学习分支 %s\n", i+1, len(chain), node.Name)
		_, wrote, err := a.LearnNodeEvolution(ctx, node, force, batchSize)
		if err != nil {
			return changed, fmt.Errorf("演进学习分支 %s: %w", node.Name, err)
		}
		if wrote {
			changed++
			fmt.Printf("[%d/%d] 已更新演进事实 %s\n", i+1, len(chain), node.Name)
		} else {
			fmt.Printf("[%d/%d] 演进事实未变化，跳过 %s\n", i+1, len(chain), node.Name)
		}
	}
	return changed, nil
}

func (a *App) GenerateChain(chain []model.BranchNode) error {
	output := a.Config.Docs.Output
	if !filepath.IsAbs(output) {
		output = filepath.Join(a.Root, output)
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	for _, node := range chain {
		fact, err := storage.LoadFact(a.Root, node.Name)
		if err != nil {
			return fmt.Errorf("缺少分支 %s 的学习事实，请先运行 learn 或 sync", node.Name)
		}
		if fact.HeadCommit != node.Tip {
			return fmt.Errorf("分支 %s 的事实已过期，请先重新学习", node.Name)
		}
		dir := storage.BranchDocDir(output, node.Name)
		if err := writeBranchDocs(dir, fact, chain); err != nil {
			return err
		}
		if err := writeCommitEvolution(dir, a.Root, fact); err != nil {
			return err
		}
	}
	return writeRootIndex(output, chain)
}

func (a *App) Sync(ctx context.Context, selected string, force bool, evolution bool, directWrite bool, directLimit int, evolutionBatchSize int) error {
	defer traceStep("docs-seed sync 总耗时")()
	if evolutionBatchSize <= 0 {
		evolutionBatchSize = a.Config.Evolution.BatchSize
	}
	if len(a.Config.Workspace.Projects) > 0 {
		return a.SyncWorkspace(ctx, force, evolution, directWrite, directLimit, evolutionBatchSize)
	}
	done := traceStep("检查工作树状态")
	dirty, err := a.Repo.IsDirty(ctx)
	if err != nil {
		done()
		return err
	}
	done()
	if dirty {
		fmt.Println("警告：工作树存在未提交修改；文档只基于已提交的分支 tip 生成。")
	}
	fmt.Println("同步分支谱系...")
	graph, err := a.SyncBranches(ctx, true)
	if err != nil {
		return err
	}
	done = traceStep("计算阅读链路")
	chain, err := a.CurrentChain(ctx, graph, selected)
	if err != nil {
		done()
		return err
	}
	done()
	fmt.Printf("阅读链路：%s\n", chainNames(chain))
	if directWrite {
		fmt.Println("启用 direct-write：Agent 将直接写 Markdown 文档，主进程不解析 JSON。")
		return a.GenerateChainDirect(ctx, chain, directLimit, evolutionBatchSize)
	}
	if evolution {
		if _, err := a.LearnChainEvolution(ctx, chain, force, evolutionBatchSize); err != nil {
			return err
		}
	} else {
		if _, err := a.LearnChain(ctx, chain, force, true); err != nil {
			return err
		}
	}
	fmt.Println("生成文档...")
	return a.GenerateChain(chain)
}

func (a *App) AddWorkspaceProjects(paths []string) ([]string, error) {
	if len(paths) == 0 {
		entries, err := os.ReadDir(a.Root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if _, err := os.Stat(filepath.Join(a.Root, entry.Name(), ".git")); err == nil {
					paths = append(paths, entry.Name())
				}
			}
		}
	}
	existing := map[string]bool{}
	for _, item := range a.Config.Workspace.Projects {
		existing[item] = true
	}
	var added []string
	for _, item := range paths {
		item = filepath.Clean(item)
		full := filepath.Join(a.Root, item)
		if _, err := os.Stat(filepath.Join(full, ".git")); err != nil {
			return nil, fmt.Errorf("%s 不是独立 Git 子项目", item)
		}
		if !existing[item] {
			a.Config.Workspace.Projects = append(a.Config.Workspace.Projects, item)
			existing[item] = true
			added = append(added, item)
		}
		if _, err := os.Stat(filepath.Join(full, config.FileName)); errors.Is(err, os.ErrNotExist) {
			if err := Init(full, false); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(a.Config.Workspace.Projects)
	return added, config.Save(a.Root, a.Config)
}

func (a *App) SyncWorkspace(ctx context.Context, force bool, evolution bool, directWrite bool, directLimit int, evolutionBatchSize int) error {
	var lines []string
	for _, project := range a.Config.Workspace.Projects {
		full := filepath.Join(a.Root, project)
		child, err := Open(full)
		if err != nil {
			return fmt.Errorf("打开子项目 %s: %w", project, err)
		}
		child.Generator = a.Generator
		if err := child.Sync(ctx, "", force, evolution, directWrite, directLimit, evolutionBatchSize); err != nil {
			return fmt.Errorf("同步子项目 %s: %w", project, err)
		}
		lines = append(lines, fmt.Sprintf("- [%s](../../%s/%s/README.md)", project, project, child.Config.Docs.Output))
	}
	output := a.Config.Docs.Output
	if !filepath.IsAbs(output) {
		output = filepath.Join(a.Root, output)
	}
	body := "# Workspace 文档索引\n\n每个子项目独立维护自己的 Git 分支谱系和人类阅读文档。\n\n" + strings.Join(lines, "\n") + "\n"
	return storage.AtomicWrite(filepath.Join(output, "README.md"), []byte(body))
}

type directCommitBatchItem struct {
	Commit        model.Commit
	Index         int
	RecordedInDoc bool
}

func (a *App) GenerateChainDirect(ctx context.Context, chain []model.BranchNode, limit int, batchSize int) error {
	output := a.Config.Docs.Output
	if !filepath.IsAbs(output) {
		output = filepath.Join(a.Root, output)
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	checkpoint, err := loadDirectCheckpoint(output)
	if err != nil {
		return err
	}
	if batchSize <= 0 {
		batchSize = a.Config.Evolution.BatchSize
	}
	if batchSize <= 0 {
		batchSize = 8
	}
	processed := 0
	for i, node := range chain {
		mode, base := "full", ""
		if node.Parent != "" {
			mode, base = "incremental", node.ForkPoint
		}
		done := traceStep("  读取分支提交列表")
		commits, err := a.Repo.Commits(ctx, base, node.Tip)
		if err != nil {
			done()
			return err
		}
		done()
		fmt.Printf("[%d/%d] direct-write 分支 %s，提交数 %d\n", i+1, len(chain), node.Name, len(commits))
		done = traceStep("  确保 direct-write 文档骨架")
		if err := ensureDirectBranchSkeleton(output, node, mode, base, chain); err != nil {
			done()
			return err
		}
		done()
		updateDirectCheckpointBranch(&checkpoint, node, mode, base, chain)
		done = traceStep("  保存 direct-write 存档点")
		if err := saveDirectCheckpoint(output, checkpoint); err != nil {
			done()
			return err
		}
		done()
		var pending []directCommitBatchItem
		flushPending := func() error {
			if len(pending) == 0 {
				return nil
			}
			count, err := a.writeDirectCommitBatch(ctx, output, node, mode, base, chain, pending, len(commits))
			if err != nil {
				return err
			}
			for _, item := range pending {
				markDirectCheckpointProcessed(&checkpoint, node, mode, base, chain, item.Commit, item.Index+1, len(commits), "agent")
			}
			done := traceStep("    保存 direct-write 存档点")
			if err := saveDirectCheckpoint(output, checkpoint); err != nil {
				done()
				return err
			}
			done()
			processed += count
			pending = nil
			return nil
		}
		for j, commit := range commits {
			prefix := fmt.Sprintf("  [%d/%d] %s %s", j+1, len(commits), short(commit.Hash), commit.Subject)
			commit.Files = filterFiles(commit.Files, a.Config.Exclude)
			if len(commit.Files) == 0 {
				if err := flushPending(); err != nil {
					return err
				}
				fmt.Printf("%s - 跳过，无有效业务文件变化\n", prefix)
				continue
			}
			dir := storage.BranchDocDir(output, node.Name)
			recordedInDoc, err := directCommitRecorded(dir, commit)
			if err != nil {
				return err
			}
			recordedInCheckpoint := directCheckpointHas(checkpoint, node.Name, commit.Hash)
			if recordedInCheckpoint && recordedInDoc {
				if err := flushPending(); err != nil {
					return err
				}
				fmt.Printf("%s - 跳过，存档点已记录且最终文档已沉淀\n", prefix)
				continue
			}
			if recordedInCheckpoint && !recordedInDoc {
				fmt.Printf("%s - 存档点已记录但最终文档缺少该提交，重新处理\n", prefix)
			}
			if !recordedInCheckpoint && recordedInDoc {
				if err := flushPending(); err != nil {
					return err
				}
				fmt.Printf("%s - 最终文档已有记录，补写存档点\n", prefix)
				markDirectCheckpointProcessed(&checkpoint, node, mode, base, chain, commit, j+1, len(commits), "existing-doc")
				if err := saveDirectCheckpoint(output, checkpoint); err != nil {
					return err
				}
				continue
			}
			fmt.Printf("%s - 加入 direct-write 批次，变更文件 %d 个\n", prefix, len(commit.Files))
			pending = append(pending, directCommitBatchItem{Commit: commit, Index: j, RecordedInDoc: recordedInDoc})
			remaining := batchSize
			if limit > 0 && limit-processed < remaining {
				remaining = limit - processed
			}
			if remaining <= 0 || len(pending) >= remaining {
				if err := flushPending(); err != nil {
					return err
				}
			}
			if limit > 0 && processed >= limit {
				fmt.Printf("direct-write 已达到 --limit-commits=%d，提前停止。\n", limit)
				return ensureDirectRootIndex(output, chain)
			}
		}
		if err := flushPending(); err != nil {
			return err
		}
		fmt.Printf("[%d/%d] direct-write 完成 %s\n", i+1, len(chain), node.Name)
	}
	return ensureDirectRootIndex(output, chain)
}

func (a *App) writeDirectCommitBatch(ctx context.Context, output string, node model.BranchNode, mode, base string, chain []model.BranchNode, items []directCommitBatchItem, total int) (int, error) {
	dir := storage.BranchDocDir(output, node.Name)
	first, last := items[0].Index+1, items[len(items)-1].Index+1
	fmt.Printf("  [%d-%d/%d] direct-write 批量处理 %d 个提交\n", first, last, total, len(items))
	done := traceStep("    读取 direct-write 当前文档")
	before, err := snapshotDirectDocs(dir)
	if err != nil {
		done()
		return 0, err
	}
	done()
	diffMax := a.Config.Evolution.DiffMaxBytes
	if diffMax <= 0 {
		diffMax = 120000
	}
	for i := range items {
		commit := items[i].Commit
		done = traceStep("    读取 commit diff " + short(commit.Hash))
		diff, err := a.Repo.Diff(ctx, commit.Parent, commit.Hash, diffMax)
		if err != nil {
			done()
			return 0, err
		}
		done()
		items[i].Commit.Diff = diff
	}
	done = traceStep("    写入 direct-write 批量材料")
	material, err := writeDirectWriteCommitBatchMaterial(a.Root, output, node, mode, base, chain, items, total)
	if err != nil {
		done()
		return 0, err
	}
	done()
	prompt := buildDirectWriteCommitBatchPrompt(output, node, mode, base, chain, material, items, total)
	done = traceStep("    Agent 批量直写文档")
	outputText, err := a.Generator.Write(ctx, dir, prompt, a.Root)
	if err != nil {
		done()
		return 0, fmt.Errorf("direct-write 分支 %s 提交批次 %s..%s: %w", node.Name, short(items[0].Commit.Hash), short(items[len(items)-1].Commit.Hash), err)
	}
	done()
	done = traceStep("    校验 direct-write 批量结果")
	if err := validateDirectWriteBatchResult(dir, items, before); err != nil {
		done()
		return 0, fmt.Errorf("direct-write 分支 %s 提交批次 %s..%s: %w\nAgent 输出：%s", node.Name, short(items[0].Commit.Hash), short(items[len(items)-1].Commit.Hash), err, trimForError(outputText, 2000))
	}
	done()
	for _, item := range items {
		fmt.Printf("    %s - 完成，已沉淀到 %s\n", short(item.Commit.Hash), dir)
	}
	return len(items), nil
}

func traceStep(label string) func() {
	start := time.Now()
	fmt.Printf("[timer] %s...\n", label)
	return func() {
		fmt.Printf("[timer] %s 完成，用时 %s\n", label, time.Since(start).Round(time.Millisecond))
	}
}

func buildPrompt(cfg config.Config, node model.BranchNode, mode, base string, files []string, logText string, history bool) string {
	scope := "该分支完整代码"
	if mode == "incremental" {
		scope = fmt.Sprintf("相对父主分支 %s、从 %s 到 %s 的增量", node.Parent, short(base), short(node.Tip))
	}
	historyHint := ""
	if history {
		historyHint = "提交历史只用于理解变化意图；结论仍需与当前源码一致。"
	}
	return fmt.Sprintf(`你是面向人类读者的软件业务分析员。请阅读当前目录源码，为分支 %q 生成 %s 文档事实。

只回答系统在业务上做什么、业务状态如何变化、数据从哪里进入、经过哪些处理、写到哪里、如何流向外部系统、失败/补偿路径，以及源码中已经体现的架构决策。
不要输出函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。不要把技术框架本身当作业务逻辑。
每条结论都必须能从源码或提交差异验证。evidence 只记录仓库相对文件路径和简短证据说明，不写行号或调用步骤。
architecture_decisions 只记录已经由源码结构、配置边界、数据所有权或流程编排体现出来的决策、取舍和后果；无法从证据确认时不要编造。
%s

分析范围：%s
相关文件：
%s

提交范围：
%s

严格输出以下 JSON，不要输出 Markdown 或额外解释：
{
  "business_logic": ["具体业务逻辑"],
  "data_flow": ["具体数据流转逻辑"],
  "architecture_decisions": ["已由源码体现的架构决策"],
  "evidence": [{"path": "relative/path", "description": "支持的结论"}]
}
`, node.Name, map[string]string{"full": "全量", "incremental": "增量"}[mode], historyHint, scope, bulletList(files), logText)
}

func buildCommitPrompt(node model.BranchNode, mode, base string, commit model.Commit) string {
	scope := "根主分支从初始提交开始的业务演进"
	if mode == "incremental" {
		scope = fmt.Sprintf("相对父主分支 %s、从 %s 到 %s 的增量业务演进", node.Parent, short(base), short(node.Tip))
	}
	return fmt.Sprintf(`你是面向人类读者的软件业务演进分析员。请只分析下面这个 Git commit 对分支 %q 的业务含义。

目标：从第一个提交开始逐步读取 commit message 和 diff，提取业务逻辑、数据流转、架构决策的演进事实。当前请求只处理一个 commit，后续工具会把每个 commit 的结果累积成分支文档。

只回答这个 commit 引入、改变或移除的业务事实。不要复述没有变化的旧事实。不要输出函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。
每条结论必须能从本 commit 的 message、diff 或当前快照验证。evidence 只记录仓库相对文件路径和简短证据说明，不写行号或调用步骤。
architecture_decisions 只记录这个 commit 已经由源码结构、配置边界、数据所有权或流程编排体现出来的决策、取舍和后果；无法从证据确认时不要编造。

分析范围：%s
提交：%s
提交时间：%s
提交信息：%s

变更文件：
%s

Diff：
%s

严格输出以下 JSON，不要输出 Markdown 或额外解释：
{
  "business_logic": ["这个 commit 引入或改变的业务逻辑"],
  "data_flow": ["这个 commit 引入或改变的数据流转逻辑"],
  "architecture_decisions": ["这个 commit 体现的架构决策"],
  "evidence": [{"path": "relative/path", "description": "支持的结论"}]
}
`, node.Name, scope, commit.Hash, commit.Timestamp, commit.Subject, bulletList(commit.Files), commit.Diff)
}

func buildCommitBatchPrompt(node model.BranchNode, mode, base string, commits []model.Commit, indexes []int, total int) string {
	scope := "根主分支从初始提交开始的业务演进"
	if mode == "incremental" {
		scope = fmt.Sprintf("相对父主分支 %s、从 %s 到 %s 的增量业务演进", node.Parent, short(base), short(node.Tip))
	}
	var body strings.Builder
	fmt.Fprintf(&body, `你是面向人类读者的软件业务演进分析员。请按顺序批量分析下面 %d 个 Git commit 对分支 %q 的业务含义。

目标：从第一个提交开始逐步读取 commit message 和 diff，提取业务逻辑、数据流转、架构决策的演进事实。本次请求处理一个 commit 批次，后续工具会把每个 commit 的结果累积成分支文档。

只回答每个 commit 引入、改变或移除的业务事实。不要复述没有变化的旧事实。不要输出函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。
每条结论必须能从对应 commit 的 message、diff 或当前快照验证。evidence 只记录仓库相对文件路径和简短证据说明，不写行号或调用步骤。
architecture_decisions 只记录对应 commit 已经由源码结构、配置边界、数据所有权或流程编排体现出来的决策、取舍和后果；无法从证据确认时不要编造。
如果某个 commit 没有可证实业务影响，仍必须为它输出一条 commit fact，设置 skipped=true，并写 reason；fact 内三个数组可以为空。

分析范围：%s
批次位置：第 %d 到第 %d 个提交，共 %d 个提交

`, len(commits), node.Name, scope, indexes[0]+1, indexes[len(indexes)-1]+1, total)
	for i, commit := range commits {
		fmt.Fprintf(&body, `--- COMMIT %d/%d ---
hash: %s
parent: %s
time: %s
subject: %s
`, indexes[i]+1, total, commit.Hash, commit.Parent, commit.Timestamp, commit.Subject)
		if commit.Body != "" {
			fmt.Fprintf(&body, "body:\n%s\n", commit.Body)
		}
		fmt.Fprintf(&body, `files:
%s

diff:
%s

`, bulletList(commit.Files), commit.Diff)
	}
	body.WriteString(`严格输出以下 JSON，不要输出 Markdown 或额外解释。commits 数组必须按输入顺序返回，并且每个输入 hash 都必须出现一次：
{
  "commits": [
    {
      "commit": {"hash": "输入 commit hash"},
      "skipped": false,
      "reason": "",
      "fact": {
        "business_logic": ["这个 commit 引入或改变的业务逻辑"],
        "data_flow": ["这个 commit 引入或改变的数据流转逻辑"],
        "architecture_decisions": ["这个 commit 体现的架构决策"],
        "evidence": [{"path": "relative/path", "description": "支持的结论"}]
      }
    }
  ]
}
`)
	return body.String()
}

func ensureDirectBranchSkeleton(output string, node model.BranchNode, mode, base string, chain []model.BranchNode) error {
	dir := storage.BranchDocDir(output, node.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	readme := fmt.Sprintf(`# %s

- 文档类型：%s
- 父主分支：%s
- 提交范围：%s
- 阅读链路：%s

本目录由 docs-seed direct-write 模式按提交顺序滚动更新。

- [业务逻辑](./business-logic.md)
- [数据流转](./data-flow.md)
- [ADR](./adr.md)
- [提交演进](./commit-evolution.md)
`, node.Name, map[string]string{"full": "全量基线", "incremental": "相对父主分支的增量"}[mode], emptyAs(node.Parent, "无"), commitRange(emptyBranchFact(node, mode, base)), chainNames(chain))
	files := map[string]string{
		"README.md":           readme,
		"business-logic.md":   "# " + node.Name + "：业务逻辑\n\n",
		"data-flow.md":        "# " + node.Name + "：数据流转\n\n",
		"adr.md":              "# " + node.Name + "：ADR\n\n",
		"commit-evolution.md": "# " + node.Name + "：提交演进\n\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := storage.AtomicWrite(path, []byte(content)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeDirectWriteCommitMaterial(root, output string, node model.BranchNode, mode, base string, chain []model.BranchNode, commit model.Commit, index, total int) (string, error) {
	dir := filepath.Join(storage.StateDir(root), "tmp", "direct-write")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%04d-%s.md", strings.ReplaceAll(node.Name, "/", "__"), index, short(commit.Hash)))
	var body strings.Builder
	fmt.Fprintf(&body, "# docs-seed direct-write commit material: %s %d/%d\n\n", node.Name, index, total)
	fmt.Fprintf(&body, "output_dir: %s\n", storage.BranchDocDir(output, node.Name))
	fmt.Fprintf(&body, "branch: %s\nmode: %s\nparent: %s\nbase: %s\nhead: %s\nchain: %s\n\n",
		node.Name, mode, emptyAs(node.Parent, "无"), base, node.Tip, chainNames(chain))
	fmt.Fprintf(&body, "commit_index: %d\ncommit_total: %d\n\n", index, total)
	fmt.Fprintf(&body, "hash: %s\nparent: %s\ntime: %s\nsubject: %s\n", commit.Hash, commit.Parent, commit.Timestamp, commit.Subject)
	if commit.Body != "" {
		fmt.Fprintf(&body, "body:\n%s\n", commit.Body)
	}
	fmt.Fprintf(&body, "files:\n%s\n", bulletList(commit.Files))
	fmt.Fprintf(&body, "diff:\n%s\n", commit.Diff)
	if err := storage.AtomicWrite(path, []byte(body.String())); err != nil {
		return "", err
	}
	return path, nil
}

func writeDirectWriteCommitBatchMaterial(root, output string, node model.BranchNode, mode, base string, chain []model.BranchNode, items []directCommitBatchItem, total int) (string, error) {
	dir := filepath.Join(storage.StateDir(root), "tmp", "direct-write")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	first, last := items[0].Index+1, items[len(items)-1].Index+1
	path := filepath.Join(dir, fmt.Sprintf("%s-%04d-%04d-batch.md", strings.ReplaceAll(node.Name, "/", "__"), first, last))
	var body strings.Builder
	fmt.Fprintf(&body, "# docs-seed direct-write commit batch material: %s %d-%d/%d\n\n", node.Name, first, last, total)
	fmt.Fprintf(&body, "output_dir: %s\n", storage.BranchDocDir(output, node.Name))
	fmt.Fprintf(&body, "branch: %s\nmode: %s\nparent: %s\nbase: %s\nhead: %s\nchain: %s\n\n",
		node.Name, mode, emptyAs(node.Parent, "无"), base, node.Tip, chainNames(chain))
	for _, item := range items {
		commit := item.Commit
		fmt.Fprintf(&body, "--- COMMIT %d/%d ---\n", item.Index+1, total)
		fmt.Fprintf(&body, "hash: %s\nparent: %s\ntime: %s\nsubject: %s\n", commit.Hash, commit.Parent, commit.Timestamp, commit.Subject)
		if commit.Body != "" {
			fmt.Fprintf(&body, "body:\n%s\n", commit.Body)
		}
		fmt.Fprintf(&body, "files:\n%s\n", bulletList(commit.Files))
		fmt.Fprintf(&body, "diff:\n%s\n\n", commit.Diff)
	}
	if err := storage.AtomicWrite(path, []byte(body.String())); err != nil {
		return "", err
	}
	return path, nil
}

func buildDirectWriteCommitPrompt(output string, node model.BranchNode, mode, base string, chain []model.BranchNode, materialPath string, commit model.Commit, index, total int) string {
	dir := storage.BranchDocDir(output, node.Name)
	modeLabel := "全量基线"
	scope := "从该分支可达历史的第一个提交开始，按提交顺序总结业务演进"
	if mode == "incremental" {
		modeLabel = "相对父主分支的增量"
		scope = fmt.Sprintf("只总结相对父主分支 %s、从 %s 到 %s 的增量业务演进", node.Parent, short(base), short(node.Tip))
	}
	return fmt.Sprintf(`你是 docs-seed 的滚动文档生成 Agent。请直接在当前工作目录更新 Markdown 文件，不要向 stdout 输出 JSON。

任务：处理主分支 %q 的第 %d/%d 个 commit，并基于已有文档滚动更新结果。

当前 commit：%s %s

重要：本次 commit 的详细材料不在本提示词中。请先读取这个材料文件：
%s

写入目录：%s
当前工作目录就是写入目录。必须只写这个目录下的文件，禁止修改源码、配置、Git 文件或其他目录。

必须读取并更新这些文件；如果不存在则创建：
- README.md
- business-logic.md
- data-flow.md
- adr.md
- commit-evolution.md

文档边界：
- 只写业务逻辑、业务状态变化、数据流转、失败/补偿路径、已经由源码体现的架构决策。
- 不写函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。
- 每条结论必须能从提交信息、diff 或当前源码验证。
- evidence 只写仓库相对文件路径和简短证据说明，不写行号或调用步骤。

滚动更新规则：
- 本次只根据材料文件中的当前 commit 更新文档。
- 对 business-logic.md、data-flow.md、adr.md：把当前 commit 带来的业务变化合并进已有总结；没有业务影响则不要强行新增。
- 对 commit-evolution.md：必须追加或更新一个当前 commit 小节，小节标题必须包含完整 hash %s 或短 hash %s；即使当前 commit 没有业务影响，也要记录“无可证实业务影响”的判断和证据。
- 不要删除前面 commit 已经总结出的有效内容，除非当前 commit 明确废弃或替代它。
- 保持文档简洁，合并重复结论。

分支信息：
- 文档类型：%s
- 父主分支：%s
- 提交范围：%s
- 阅读链路：%s
- 分析范围：%s

文件格式要求：
- README.md：列出文档类型、父主分支、提交范围、阅读链路，并链接另外四个文件。
- business-logic.md：按业务能力总结最终分支事实。
- data-flow.md：按数据入口、处理、持久化、外部流出总结。
- adr.md：写已经发生的架构决策、取舍、后果。
- commit-evolution.md：按提交顺序逐 commit 记录业务演进事实。

现在开始：读取材料文件，然后只更新写入目录下的 Markdown 文件。
`, node.Name, index, total, short(commit.Hash), commit.Subject, materialPath, dir, commit.Hash, short(commit.Hash), modeLabel, emptyAs(node.Parent, "无"), commitRange(emptyBranchFact(node, mode, base)), chainNames(chain), scope)
}

func buildDirectWriteCommitBatchPrompt(output string, node model.BranchNode, mode, base string, chain []model.BranchNode, materialPath string, items []directCommitBatchItem, total int) string {
	dir := storage.BranchDocDir(output, node.Name)
	modeLabel := "全量基线"
	scope := "从该分支可达历史的第一个提交开始，按提交顺序总结业务演进"
	if mode == "incremental" {
		modeLabel = "相对父主分支的增量"
		scope = fmt.Sprintf("只总结相对父主分支 %s、从 %s 到 %s 的增量业务演进", node.Parent, short(base), short(node.Tip))
	}
	first, last := items[0].Index+1, items[len(items)-1].Index+1
	var hashes []string
	for _, item := range items {
		hashes = append(hashes, fmt.Sprintf("- %s (%s) %s", item.Commit.Hash, short(item.Commit.Hash), item.Commit.Subject))
	}
	return fmt.Sprintf(`你是 docs-seed 的滚动文档生成 Agent。请直接在当前工作目录更新 Markdown 文件，不要向 stdout 输出 JSON。

任务：按顺序处理主分支 %q 的第 %d-%d/%d 个 commit，并基于已有文档滚动更新结果。

本批次 commit：
%s

重要：本批次 commit 的详细材料不在本提示词中。请先读取这个材料文件：
%s

写入目录：%s
当前工作目录就是写入目录。必须只写这个目录下的文件，禁止修改源码、配置、Git 文件或其他目录。

必须读取并更新这些文件；如果不存在则创建：
- README.md
- business-logic.md
- data-flow.md
- adr.md
- commit-evolution.md

文档边界：
- 只写业务逻辑、业务状态变化、数据流转、失败/补偿路径、已经由源码体现的架构决策。
- 不写函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。
- 每条结论必须能从提交信息、diff 或当前源码验证。
- evidence 只写仓库相对文件路径和简短证据说明，不写行号或调用步骤。

滚动更新规则：
- 必须严格按材料文件中的 commit 顺序处理本批次。
- 对 business-logic.md、data-flow.md、adr.md：把本批次 commit 带来的业务变化合并进已有总结；没有业务影响则不要强行新增。
- 对 commit-evolution.md：必须为本批次每个 commit 追加或更新一个小节，小节标题必须包含对应完整 hash 或短 hash；即使某个 commit 没有业务影响，也要记录“无可证实业务影响”的判断和证据。
- 不要删除前面 commit 已经总结出的有效内容，除非本批次 commit 明确废弃或替代它。
- 保持文档简洁，合并重复结论。

分支信息：
- 文档类型：%s
- 父主分支：%s
- 提交范围：%s
- 阅读链路：%s
- 分析范围：%s

文件格式要求：
- README.md：列出文档类型、父主分支、提交范围、阅读链路，并链接另外四个文件。
- business-logic.md：按业务能力总结最终分支事实。
- data-flow.md：按数据入口、处理、持久化、外部流出总结。
- adr.md：写已经发生的架构决策、取舍、后果。
- commit-evolution.md：按提交顺序逐 commit 记录业务演进事实。

现在开始：读取材料文件，然后只更新写入目录下的 Markdown 文件。
`, node.Name, first, last, total, strings.Join(hashes, "\n"), materialPath, dir, modeLabel, emptyAs(node.Parent, "无"), commitRange(emptyBranchFact(node, mode, base)), chainNames(chain), scope)
}

func snapshotDirectDocs(dir string) (map[string]string, error) {
	snapshot := map[string]string{}
	for _, name := range directDocNames() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				snapshot[name] = ""
				continue
			}
			return nil, err
		}
		snapshot[name] = string(data)
	}
	return snapshot, nil
}

func validateDirectWriteResult(dir string, commit model.Commit, before map[string]string, beforeRecorded bool) error {
	after, err := snapshotDirectDocs(dir)
	if err != nil {
		return err
	}
	changed := false
	for _, name := range directDocNames() {
		if after[name] != before[name] {
			changed = true
			break
		}
	}
	recorded, err := directCommitRecorded(dir, commit)
	if err != nil {
		return err
	}
	if !recorded {
		return fmt.Errorf("Agent 未在 commit-evolution.md 记录当前提交 %s；请检查提示词或 Agent 写文件权限", short(commit.Hash))
	}
	if !changed && !beforeRecorded {
		return fmt.Errorf("Agent 没有修改任何结果文档；当前提交 %s 未沉淀到最终文档", short(commit.Hash))
	}
	return nil
}

func validateDirectWriteBatchResult(dir string, items []directCommitBatchItem, before map[string]string) error {
	after, err := snapshotDirectDocs(dir)
	if err != nil {
		return err
	}
	changed := false
	for _, name := range directDocNames() {
		if after[name] != before[name] {
			changed = true
			break
		}
	}
	for _, item := range items {
		recorded, err := directCommitRecorded(dir, item.Commit)
		if err != nil {
			return err
		}
		if !recorded {
			return fmt.Errorf("Agent 未在 commit-evolution.md 记录当前提交 %s；请检查提示词或 Agent 写文件权限", short(item.Commit.Hash))
		}
	}
	needsChange := false
	for _, item := range items {
		if !item.RecordedInDoc {
			needsChange = true
			break
		}
	}
	if needsChange && !changed {
		return fmt.Errorf("Agent 没有修改任何结果文档；当前批次未沉淀到最终文档")
	}
	return nil
}

func directCommitRecorded(dir string, commit model.Commit) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "commit-evolution.md"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	text := string(data)
	return strings.Contains(text, commit.Hash) || strings.Contains(text, short(commit.Hash)), nil
}

func directDocNames() []string {
	return []string{"README.md", "business-logic.md", "data-flow.md", "adr.md", "commit-evolution.md"}
}

const directCheckpointFile = "docs-seed-checkpoint.json"

type directCheckpoint struct {
	Version   int                               `json:"version"`
	UpdatedAt string                            `json:"updated_at"`
	Chain     []string                          `json:"chain"`
	Branches  map[string]directCheckpointBranch `json:"branches"`
}

type directCheckpointBranch struct {
	Branch          string                            `json:"branch"`
	Parent          string                            `json:"parent,omitempty"`
	Mode            string                            `json:"mode"`
	BaseCommit      string                            `json:"base_commit,omitempty"`
	HeadCommit      string                            `json:"head_commit"`
	CommitTotal     int                               `json:"commit_total,omitempty"`
	LastProcessed   string                            `json:"last_processed_commit,omitempty"`
	LastProcessedAt string                            `json:"last_processed_at,omitempty"`
	Processed       map[string]directCheckpointCommit `json:"processed_commits"`
}

type directCheckpointCommit struct {
	Hash        string `json:"hash"`
	ShortHash   string `json:"short_hash"`
	Subject     string `json:"subject"`
	Index       int    `json:"index"`
	Total       int    `json:"total"`
	Source      string `json:"source"`
	ProcessedAt string `json:"processed_at"`
}

func loadDirectCheckpoint(output string) (directCheckpoint, error) {
	path := filepath.Join(output, directCheckpointFile)
	checkpoint := directCheckpoint{
		Version:  1,
		Branches: map[string]directCheckpointBranch{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpoint, nil
		}
		return directCheckpoint{}, err
	}
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return directCheckpoint{}, fmt.Errorf("解析存档点 %s: %w", path, err)
	}
	if checkpoint.Version == 0 {
		checkpoint.Version = 1
	}
	if checkpoint.Branches == nil {
		checkpoint.Branches = map[string]directCheckpointBranch{}
	}
	return checkpoint, nil
}

func saveDirectCheckpoint(output string, checkpoint directCheckpoint) error {
	checkpoint.Version = 1
	checkpoint.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return storage.AtomicWrite(filepath.Join(output, directCheckpointFile), data)
}

func updateDirectCheckpointBranch(checkpoint *directCheckpoint, node model.BranchNode, mode, base string, chain []model.BranchNode) {
	if checkpoint.Branches == nil {
		checkpoint.Branches = map[string]directCheckpointBranch{}
	}
	checkpoint.Chain = chainNameSlice(chain)
	branch := checkpoint.Branches[node.Name]
	branch.Branch = node.Name
	branch.Parent = node.Parent
	branch.Mode = mode
	branch.BaseCommit = base
	branch.HeadCommit = node.Tip
	if branch.Processed == nil {
		branch.Processed = map[string]directCheckpointCommit{}
	}
	checkpoint.Branches[node.Name] = branch
}

func markDirectCheckpointProcessed(checkpoint *directCheckpoint, node model.BranchNode, mode, base string, chain []model.BranchNode, commit model.Commit, index, total int, source string) {
	updateDirectCheckpointBranch(checkpoint, node, mode, base, chain)
	branch := checkpoint.Branches[node.Name]
	branch.CommitTotal = total
	now := time.Now().UTC().Format(time.RFC3339)
	branch.LastProcessed = commit.Hash
	branch.LastProcessedAt = now
	branch.Processed[commit.Hash] = directCheckpointCommit{
		Hash: commit.Hash, ShortHash: short(commit.Hash), Subject: commit.Subject,
		Index: index, Total: total, Source: source, ProcessedAt: now,
	}
	checkpoint.Branches[node.Name] = branch
}

func directCheckpointHas(checkpoint directCheckpoint, branch, hash string) bool {
	item, ok := checkpoint.Branches[branch]
	if !ok || item.Processed == nil {
		return false
	}
	_, ok = item.Processed[hash]
	return ok
}

func trimForError(text string, max int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "<empty>"
	}
	if max <= 0 || len(text) <= max {
		return text
	}
	return text[:max] + "\n[docs-seed: output truncated]"
}

func emptyBranchFact(node model.BranchNode, mode, base string) model.Fact {
	return model.Fact{
		Version:     1,
		Branch:      node.Name,
		Parent:      node.Parent,
		Mode:        mode,
		BaseCommit:  base,
		HeadCommit:  node.Tip,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func summarizeEvolution(node model.BranchNode, mode, base string, commits []model.CommitFact) model.Fact {
	fact := emptyBranchFact(node, mode, base)
	fact.BusinessLogic = uniqueStringsFrom(commits, func(f model.Fact) []string { return f.BusinessLogic })
	fact.DataFlow = uniqueStringsFrom(commits, func(f model.Fact) []string { return f.DataFlow })
	fact.ArchitectureDecisions = uniqueStringsFrom(commits, func(f model.Fact) []string { return f.ArchitectureDecisions })
	fact.Evidence = uniqueEvidence(commits)
	if len(fact.BusinessLogic) == 0 && len(fact.DataFlow) == 0 && len(fact.ArchitectureDecisions) == 0 {
		fact.BusinessLogic = []string{"该提交范围未提取到可证实的业务逻辑变化。"}
	}
	return fact
}

func uniqueStringsFrom(commits []model.CommitFact, selectItems func(model.Fact) []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, commit := range commits {
		if commit.Skipped {
			continue
		}
		for _, item := range selectItems(commit.Fact) {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func uniqueEvidence(commits []model.CommitFact) []model.Evidence {
	seen := map[string]bool{}
	var result []model.Evidence
	for _, commit := range commits {
		if commit.Skipped {
			continue
		}
		for _, evidence := range commit.Fact.Evidence {
			key := evidence.Path + "\x00" + evidence.Description
			if evidence.Path == "" || seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, evidence)
		}
	}
	return result
}

func validateFact(fact model.Fact) error {
	contents := append(append([]string{}, fact.BusinessLogic...), fact.DataFlow...)
	contents = append(contents, fact.ArchitectureDecisions...)
	joined := strings.ToLower(strings.Join(contents, "\n"))
	for _, forbidden := range []string{"```", "curl ", "npm ", "npx ", "go run ", "调用方式", "usage example", "how to call"} {
		if strings.Contains(joined, forbidden) {
			return fmt.Errorf("Agent 输出包含面向代码调用的内容 %q，请调整 prompt 后重试", forbidden)
		}
	}
	for _, evidence := range fact.Evidence {
		if filepath.IsAbs(evidence.Path) || strings.Contains(evidence.Path, "..") {
			return fmt.Errorf("非法证据路径: %s", evidence.Path)
		}
	}
	return nil
}

func writeBranchDocs(dir string, fact model.Fact, chain []model.BranchNode) error {
	modeLabel := "全量基线"
	if fact.Mode == "incremental" {
		modeLabel = "相对父主分支的增量"
	}
	var names []string
	for _, node := range chain {
		names = append(names, node.Name)
		if node.Name == fact.Branch {
			break
		}
	}
	readme := fmt.Sprintf(`# %s

- 文档类型：%s
- 父主分支：%s
- 提交范围：%s
- 阅读链路：%s

本目录只描述代码体现的业务逻辑和数据流转逻辑。实现细节、调用方式和运行命令应直接查看源码。

- [业务逻辑](./business-logic.md)
- [数据流转](./data-flow.md)
- [ADR](./adr.md)
`, fact.Branch, modeLabel, emptyAs(fact.Parent, "无"), commitRange(fact), strings.Join(names, " → "))
	if err := storage.AtomicWrite(filepath.Join(dir, "README.md"), []byte(readme)); err != nil {
		return err
	}
	if err := storage.AtomicWrite(filepath.Join(dir, "business-logic.md"), []byte(renderTopic("业务逻辑", fact, fact.BusinessLogic))); err != nil {
		return err
	}
	if err := storage.AtomicWrite(filepath.Join(dir, "data-flow.md"), []byte(renderTopic("数据流转", fact, fact.DataFlow))); err != nil {
		return err
	}
	return storage.AtomicWrite(filepath.Join(dir, "adr.md"), []byte(renderTopic("ADR", fact, fact.ArchitectureDecisions)))
}

func writeCommitEvolution(dir, root string, fact model.Fact) error {
	commits, err := loadBranchCommitFacts(root, fact.Branch)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(commits) == 0 {
		return nil
	}
	var body strings.Builder
	fmt.Fprintf(&body, "# %s：提交演进\n\n", fact.Branch)
	body.WriteString("本页按 Git 提交顺序记录 docs-seed 从 message 和 diff 中提取的业务演进事实。\n\n")
	for _, commit := range commits {
		fmt.Fprintf(&body, "## %s %s\n\n", short(commit.Commit.Hash), commit.Commit.Subject)
		if commit.Skipped {
			fmt.Fprintf(&body, "- 跳过：%s\n\n", emptyAs(commit.Reason, "无有效业务文件变化"))
			continue
		}
		writeEvolutionSection(&body, "业务逻辑", commit.Fact.BusinessLogic)
		writeEvolutionSection(&body, "数据流转", commit.Fact.DataFlow)
		writeEvolutionSection(&body, "ADR", commit.Fact.ArchitectureDecisions)
		if len(commit.Fact.Evidence) > 0 {
			body.WriteString("证据：\n")
			for _, evidence := range commit.Fact.Evidence {
				fmt.Fprintf(&body, "- `%s`", evidence.Path)
				if evidence.Description != "" {
					fmt.Fprintf(&body, "：%s", evidence.Description)
				}
				body.WriteByte('\n')
			}
			body.WriteByte('\n')
		}
	}
	return storage.AtomicWrite(filepath.Join(dir, "commit-evolution.md"), []byte(body.String()))
}

func writeEvolutionSection(body *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(body, "%s：\n", title)
	for _, item := range items {
		fmt.Fprintf(body, "- %s\n", item)
	}
	body.WriteByte('\n')
}

func loadBranchCommitFacts(root, branch string) ([]model.CommitFact, error) {
	dir := storage.CommitFactDir(root, branch)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []model.CommitFact
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var fact model.CommitFact
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &fact); err != nil {
			return nil, err
		}
		result = append(result, fact)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Commit.Timestamp < result[j].Commit.Timestamp
	})
	return result, nil
}

func renderTopic(title string, fact model.Fact, items []string) string {
	var body strings.Builder
	fmt.Fprintf(&body, "# %s：%s\n\n", fact.Branch, title)
	if fact.Mode == "incremental" {
		fmt.Fprintf(&body, "> 本页只记录相对 `%s` 的变化；完整理解需要按父链依次阅读。\n\n", fact.Parent)
	}
	for _, item := range items {
		fmt.Fprintf(&body, "- %s\n", item)
	}
	body.WriteString("\n## 证据附录\n\n")
	body.WriteString("提交范围：`" + commitRange(fact) + "`\n\n")
	for _, evidence := range fact.Evidence {
		fmt.Fprintf(&body, "- `%s`", evidence.Path)
		if evidence.Description != "" {
			fmt.Fprintf(&body, "：%s", evidence.Description)
		}
		body.WriteByte('\n')
	}
	return body.String()
}

func writeRootIndex(output string, chain []model.BranchNode) error {
	var body strings.Builder
	body.WriteString("# 分支文档阅读索引\n\n")
	body.WriteString("文档按主分支派生关系增量管理。请从根分支开始，沿链路依次阅读。\n\n")
	for _, node := range chain {
		fmt.Fprintf(&body, "- [%s](./branches/%s/README.md)", node.Name, strings.ReplaceAll(node.Name, "/", "__"))
		if node.Parent == "" {
			body.WriteString(" — 全量基线")
		} else {
			fmt.Fprintf(&body, " — 相对 `%s` 的增量", node.Parent)
		}
		body.WriteByte('\n')
	}
	return storage.AtomicWrite(filepath.Join(output, "README.md"), []byte(body.String()))
}

func ensureDirectRootIndex(output string, chain []model.BranchNode) error {
	return writeRootIndex(output, chain)
}

func chainNames(chain []model.BranchNode) string {
	return strings.Join(chainNameSlice(chain), " -> ")
}

func chainNameSlice(chain []model.BranchNode) []string {
	var names []string
	for _, node := range chain {
		names = append(names, node.Name)
	}
	return names
}

func filterFiles(files, excludes []string) []string {
	var result []string
	for _, file := range files {
		excluded := false
		for _, pattern := range excludes {
			prefix := strings.TrimSuffix(pattern, "**")
			if strings.HasSuffix(pattern, "/**") && strings.HasPrefix(file, prefix) {
				excluded = true
				break
			}
			if ok, _ := filepath.Match(pattern, file); ok {
				excluded = true
				break
			}
		}
		if !excluded {
			result = append(result, file)
		}
	}
	return result
}

func bulletList(items []string) string {
	if len(items) == 0 {
		return "- 无代码文件变化"
	}
	const max = 500
	if len(items) > max {
		items = items[:max]
	}
	var lines []string
	for _, item := range items {
		lines = append(lines, "- "+item)
	}
	return strings.Join(lines, "\n")
}

func commitRange(fact model.Fact) string {
	if fact.BaseCommit == "" {
		return short(fact.HeadCommit) + "（全量）"
	}
	return short(fact.BaseCommit) + ".." + short(fact.HeadCommit)
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
