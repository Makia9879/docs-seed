package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Makia9879/docs-seed/internal/app"
	"github.com/Makia9879/docs-seed/internal/gitx"
	"github.com/Makia9879/docs-seed/internal/model"
	"github.com/Makia9879/docs-seed/internal/storage"
	"github.com/spf13/cobra"
)

func Execute() error {
	return newRoot().Execute()
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:          "docs-seed",
		Short:        "从代码和 Git 分支谱系生成面向人类的业务与数据流文档",
		SilenceUsage: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(initCommand(), branchesCommand(), learnCommand(), generateCommand(), syncCommand(), workspaceCommand(), previewCommand())
	return root
}

func initCommand() *cobra.Command {
	var workspace bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "初始化 .docs-seed.yml 和本地状态目录",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := gitx.DiscoverRoot(".")
			if err != nil {
				return err
			}
			if err := app.Init(root, workspace); err != nil {
				return err
			}
			fmt.Println("已初始化:", filepath.Join(root, ".docs-seed.yml"))
			fmt.Println("文档将按 branches.main_patterns 管理；默认 main、master 和 llm/** 都视为主分支。")
			return nil
		},
	}
	cmd.Flags().BoolVar(&workspace, "workspace", false, "初始化为多 Git 子项目 workspace")
	return cmd
}

func branchesCommand() *cobra.Command {
	var noFetch bool
	parent := &cobra.Command{Use: "branches", Short: "管理主分支谱系"}
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "拉取远端 refs 并重建 branch-graph.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			graph, err := instance.SyncBranches(cmd.Context(), !noFetch)
			if err != nil {
				return err
			}
			fmt.Printf("已记录 %d 个匹配主分支\n", len(graph.Branches))
			return nil
		},
	}
	syncCmd.Flags().BoolVar(&noFetch, "no-fetch", false, "仅使用现有本地和远端 refs")
	parent.AddCommand(syncCmd)
	return parent
}

func learnCommand() *cobra.Command {
	parent := &cobra.Command{Use: "learn", Short: "把代码事实学习为业务逻辑和数据流"}
	for _, history := range []bool{false, true} {
		history := history
		name := "current"
		short := "学习当前匹配主分支"
		if history {
			name, short = "history", "结合提交历史学习当前匹配主分支"
		}
		var force bool
		cmd := &cobra.Command{
			Use:   name,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				instance, err := openCurrent()
				if err != nil {
					return err
				}
				graph, err := loadOrSync(cmd.Context(), instance)
				if err != nil {
					return err
				}
				chain, err := instance.CurrentChain(cmd.Context(), graph, "")
				if err != nil {
					return err
				}
				node := chain[len(chain)-1]
				_, wrote, err := instance.LearnNode(cmd.Context(), node, force, history)
				if err != nil {
					return err
				}
				if wrote {
					fmt.Println("已学习分支:", node.Name)
				} else {
					fmt.Println("分支事实未变化，跳过:", node.Name)
				}
				return nil
			},
		}
		cmd.Flags().BoolVar(&force, "force", false, "忽略指纹并重新学习")
		parent.AddCommand(cmd)
	}
	var evolutionForce bool
	evolution := &cobra.Command{
		Use:   "evolution",
		Short: "从根主分支第一个提交开始逐提交学习业务演进",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			graph, err := loadOrSync(cmd.Context(), instance)
			if err != nil {
				return err
			}
			chain, err := instance.CurrentChain(cmd.Context(), graph, "")
			if err != nil {
				return err
			}
			changed, err := instance.LearnChainEvolution(cmd.Context(), chain, evolutionForce)
			if err != nil {
				return err
			}
			fmt.Printf("已按提交演进学习 %d 个分支节点\n", changed)
			return nil
		},
	}
	evolution.Flags().BoolVar(&evolutionForce, "force", false, "忽略已缓存的提交事实并重新学习")
	parent.AddCommand(evolution)
	return parent
}

func generateCommand() *cobra.Command {
	var branch string
	parent := &cobra.Command{Use: "generate", Short: "从已学习事实生成文档"}
	docs := &cobra.Command{
		Use:   "docs",
		Short: "生成当前分支完整阅读链",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			graph, err := storage.LoadGraph(instance.Root)
			if err != nil {
				return fmt.Errorf("缺少分支谱系，请先运行 docs-seed branches sync")
			}
			chain, err := instance.CurrentChain(cmd.Context(), graph, branch)
			if err != nil {
				return err
			}
			if err := instance.GenerateChain(chain); err != nil {
				return err
			}
			fmt.Printf("已生成 %d 个分支节点的文档\n", len(chain))
			return nil
		},
	}
	docs.Flags().StringVar(&branch, "branch", "", "指定匹配主分支；默认当前分支")
	parent.AddCommand(docs)
	return parent
}

func syncCommand() *cobra.Command {
	var branch string
	var force bool
	var evolution bool
	var directWrite bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "拉取分支、学习当前链路并生成文档",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			if err := instance.Sync(cmd.Context(), branch, force, evolution, directWrite); err != nil {
				return err
			}
			fmt.Println("文档同步完成")
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "指定匹配主分支；默认当前分支")
	cmd.Flags().BoolVar(&force, "force", false, "强制重新学习链路")
	cmd.Flags().BoolVar(&evolution, "evolution", false, "按提交顺序逐步学习业务演进")
	cmd.Flags().BoolVar(&directWrite, "direct-write", false, "让 Agent 直接写 Markdown 文档，主进程不解析 JSON")
	return cmd
}

func workspaceCommand() *cobra.Command {
	parent := &cobra.Command{Use: "workspace", Short: "管理多 Git 子项目"}
	add := &cobra.Command{
		Use:   "add [path...]",
		Short: "添加并初始化独立 Git 子项目；不传路径时扫描第一层目录",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			added, err := instance.AddWorkspaceProjects(args)
			if err != nil {
				return err
			}
			fmt.Printf("已添加 %d 个子项目\n", len(added))
			return nil
		},
	}
	parent.AddCommand(add)
	return parent
}

func previewCommand() *cobra.Command {
	parent := &cobra.Command{Use: "preview", Short: "只读预览分析范围"}
	branches := &cobra.Command{
		Use:   "branches",
		Short: "打印当前 branch graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			graph, err := instance.Repo.BuildGraph(cmd.Context(), instance.Config.Branches)
			if err != nil {
				return err
			}
			data, _ := json.MarshalIndent(graph, "", "  ")
			fmt.Println(string(data))
			return nil
		},
	}
	files := &cobra.Command{
		Use:   "files",
		Short: "打印当前链路各节点的提交范围",
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, err := openCurrent()
			if err != nil {
				return err
			}
			graph, err := loadOrSync(context.Background(), instance)
			if err != nil {
				return err
			}
			chain, err := instance.CurrentChain(cmd.Context(), graph, "")
			if err != nil {
				return err
			}
			for _, node := range chain {
				fmt.Printf("%s\tparent=%s\tfork=%s\ttip=%s\n", node.Name, node.Parent, node.ForkPoint, node.Tip)
			}
			return nil
		},
	}
	parent.AddCommand(branches, files)
	return parent
}

func openCurrent() (*app.App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err := gitx.DiscoverRoot(cwd)
	if err != nil {
		return nil, err
	}
	return app.Open(root)
}

func loadOrSync(ctx context.Context, instance *app.App) (graph model.BranchGraph, err error) {
	graph, err = storage.LoadGraph(instance.Root)
	if err == nil {
		return graph, nil
	}
	return instance.SyncBranches(ctx, false)
}
