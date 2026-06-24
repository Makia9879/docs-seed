package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/Makia9879/docs-seed/internal/gitx"
	"github.com/Makia9879/docs-seed/internal/model"
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

func (fakeGenerator) Write(_ context.Context, workDir, prompt string, _ ...string) (string, error) {
	output := workDir
	hash := "unknown"
	if strings.Contains(prompt, "写入目录：") {
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "写入目录：") {
				output = strings.TrimSpace(strings.TrimPrefix(line, "写入目录："))
				break
			}
		}
	}
	if strings.Contains(prompt, "当前 commit：") {
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "当前 commit：") {
				fields := strings.Fields(strings.TrimPrefix(line, "当前 commit："))
				if len(fields) > 0 {
					hash = fields[0]
				}
				break
			}
		}
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"README.md", "business-logic.md", "data-flow.md", "adr.md"} {
		if err := os.WriteFile(filepath.Join(output, name), []byte("# "+name+"\n\nprocessed "+hash+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	return "written", os.WriteFile(filepath.Join(output, "commit-evolution.md"), []byte("# commit-evolution.md\n\n## "+hash+" root business\n\n- 业务演进事实。\n"), 0o644)
}

type noOpWriteGenerator struct {
	fakeGenerator
}

func (noOpWriteGenerator) Write(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	return "I did not edit files.", nil
}

type countingWriteGenerator struct {
	count int
}

func (g *countingWriteGenerator) Generate(ctx context.Context, workDir, prompt string) (model.Fact, error) {
	return fakeGenerator{}.Generate(ctx, workDir, prompt)
}

func (g *countingWriteGenerator) Write(ctx context.Context, workDir, prompt string, addDirs ...string) (string, error) {
	g.count++
	return fakeGenerator{}.Write(ctx, workDir, prompt, addDirs...)
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

	changed, err := instance.LearnChainEvolution(context.Background(), chain, false)
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
	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0))

	output := filepath.Join(root, ".docs-seed", "docs", "branches", "main")
	require.FileExists(t, filepath.Join(output, "README.md"))
	evolution := readFile(t, filepath.Join(output, "commit-evolution.md"))
	require.Contains(t, evolution, "root business")
	checkpoint := readFile(t, filepath.Join(root, ".docs-seed", "docs", directCheckpointFile))
	require.Contains(t, checkpoint, "root business")
	require.Contains(t, checkpoint, "processed_commits")
}

func TestGenerateChainDirectFailsWhenAgentDoesNotRecordCommit(t *testing.T) {
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
		Root: root, Config: cfg, Repo: gitx.Repository{Root: root}, Generator: noOpWriteGenerator{},
	}
	graph, err := instance.SyncBranches(context.Background(), false)
	require.NoError(t, err)
	chain, err := instance.CurrentChain(context.Background(), graph, "main")
	require.NoError(t, err)

	err = instance.GenerateChainDirect(context.Background(), chain, 0)
	require.ErrorContains(t, err, "Agent 未在 commit-evolution.md 记录当前提交")
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

	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0))
	require.Equal(t, 1, generator.count)
	require.NoError(t, instance.GenerateChainDirect(context.Background(), chain, 0))
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
