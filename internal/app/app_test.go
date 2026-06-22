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
	return model.Fact{
		BusinessLogic: []string{mode + "业务规则"},
		DataFlow:      []string{mode + "数据从入口流向存储"},
		Evidence:      []model.Evidence{{Path: "service/order.go", Description: "业务证据"}},
	}, nil
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
	require.Contains(t, rootDoc, "全量基线")
	require.Contains(t, childDoc, "相对父主分支的增量")
	require.Contains(t, childDoc, "main → llm/order-v2")
	require.Contains(t, business, "增量业务规则")
	require.Contains(t, business, "证据附录")
	require.NotContains(t, business, "调用方式")

	changed, err = instance.LearnChain(context.Background(), chain, false, true)
	require.NoError(t, err)
	require.Zero(t, changed)
}

func TestValidateFactRejectsInvocationInstructions(t *testing.T) {
	err := validateFact(model.Fact{
		BusinessLogic: []string{"使用 curl /orders 调用接口"},
		DataFlow:      []string{"数据进入订单系统"},
	})
	require.ErrorContains(t, err, "curl")
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
