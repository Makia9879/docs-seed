package app

import (
	"context"
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
		if err := a.Repo.Fetch(ctx, a.Config.Branches.Remote); err != nil {
			return model.BranchGraph{}, err
		}
	}
	graph, err := a.Repo.BuildGraph(ctx, a.Config.Branches)
	if err != nil {
		return model.BranchGraph{}, err
	}
	if err := storage.Ensure(a.Root); err != nil {
		return model.BranchGraph{}, err
	}
	if err := storage.SaveGraph(a.Root, graph); err != nil {
		return model.BranchGraph{}, err
	}
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
		files, err = a.Repo.AllFiles(ctx, node.Tip)
	} else {
		files, err = a.Repo.ChangedFiles(ctx, base, node.Tip)
	}
	if err != nil {
		return model.Fact{}, false, err
	}
	files = filterFiles(files, a.Config.Exclude)
	logText, err := a.Repo.Log(ctx, base, node.Tip)
	if err != nil {
		return model.Fact{}, false, err
	}
	snapshot, cleanup, err := a.Repo.Snapshot(ctx, node.Tip)
	if err != nil {
		return model.Fact{}, false, err
	}
	defer cleanup()

	prompt := buildPrompt(a.Config, node, mode, base, files, logText, history)
	fact, err := a.Generator.Generate(ctx, snapshot, prompt)
	if err != nil {
		return model.Fact{}, false, err
	}
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
	if err := storage.SaveFact(a.Root, fact); err != nil {
		return model.Fact{}, false, err
	}
	return fact, true, nil
}

func (a *App) LearnChain(ctx context.Context, chain []model.BranchNode, force bool, history bool) (int, error) {
	changed := 0
	for _, node := range chain {
		_, wrote, err := a.LearnNode(ctx, node, force, history)
		if err != nil {
			return changed, fmt.Errorf("学习分支 %s: %w", node.Name, err)
		}
		if wrote {
			changed++
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
	}
	return writeRootIndex(output, chain)
}

func (a *App) Sync(ctx context.Context, selected string, force bool) error {
	if len(a.Config.Workspace.Projects) > 0 {
		return a.SyncWorkspace(ctx, force)
	}
	dirty, err := a.Repo.IsDirty(ctx)
	if err != nil {
		return err
	}
	if dirty {
		fmt.Println("警告：工作树存在未提交修改；文档只基于已提交的分支 tip 生成。")
	}
	graph, err := a.SyncBranches(ctx, true)
	if err != nil {
		return err
	}
	chain, err := a.CurrentChain(ctx, graph, selected)
	if err != nil {
		return err
	}
	if _, err := a.LearnChain(ctx, chain, force, true); err != nil {
		return err
	}
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

func (a *App) SyncWorkspace(ctx context.Context, force bool) error {
	var lines []string
	for _, project := range a.Config.Workspace.Projects {
		full := filepath.Join(a.Root, project)
		child, err := Open(full)
		if err != nil {
			return fmt.Errorf("打开子项目 %s: %w", project, err)
		}
		if err := child.Sync(ctx, "", force); err != nil {
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

只回答系统在业务上做什么、业务状态如何变化、数据从哪里进入、经过哪些处理、写到哪里、如何流向外部系统，以及失败/补偿路径。
不要输出函数签名、类名清单、API 调用示例、CLI 命令、参数说明、安装步骤、代码调用方式、测试方法或代码块。不要把技术框架本身当作业务逻辑。
每条结论都必须能从源码或提交差异验证。evidence 只记录仓库相对文件路径和简短证据说明，不写行号或调用步骤。
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
  "evidence": [{"path": "relative/path", "description": "支持的结论"}]
}
`, node.Name, map[string]string{"full": "全量", "incremental": "增量"}[mode], historyHint, scope, bulletList(files), logText)
}

func validateFact(fact model.Fact) error {
	joined := strings.ToLower(strings.Join(append(append([]string{}, fact.BusinessLogic...), fact.DataFlow...), "\n"))
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
`, fact.Branch, modeLabel, emptyAs(fact.Parent, "无"), commitRange(fact), strings.Join(names, " → "))
	if err := storage.AtomicWrite(filepath.Join(dir, "README.md"), []byte(readme)); err != nil {
		return err
	}
	if err := storage.AtomicWrite(filepath.Join(dir, "business-logic.md"), []byte(renderTopic("业务逻辑", fact, fact.BusinessLogic))); err != nil {
		return err
	}
	return storage.AtomicWrite(filepath.Join(dir, "data-flow.md"), []byte(renderTopic("数据流转", fact, fact.DataFlow)))
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
