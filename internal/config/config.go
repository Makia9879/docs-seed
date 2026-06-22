package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const FileName = ".docs-seed.yml"

type Config struct {
	Version   int             `yaml:"version"`
	Project   ProjectConfig   `yaml:"project"`
	Branches  BranchConfig    `yaml:"branches"`
	Agent     AgentConfig     `yaml:"agent"`
	Docs      DocsConfig      `yaml:"docs"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Exclude   []string        `yaml:"exclude"`
}

type ProjectConfig struct {
	Name          string `yaml:"name"`
	Locale        string `yaml:"locale"`
	InitializedAt string `yaml:"initialized_at"`
}

type BranchConfig struct {
	Remote          string            `yaml:"remote"`
	MainPatterns    []string          `yaml:"main_patterns"`
	ParentOverrides map[string]string `yaml:"parent_overrides,omitempty"`
}

type AgentConfig struct {
	Engine  string            `yaml:"engine"`
	Command map[string]string `yaml:"commands"`
	Timeout int               `yaml:"timeout_seconds"`
}

type DocsConfig struct {
	Output string `yaml:"output"`
}

type WorkspaceConfig struct {
	Projects []string `yaml:"projects,omitempty"`
}

func Default(name string) Config {
	return Config{
		Version: 1,
		Project: ProjectConfig{
			Name:          name,
			Locale:        "zh-CN",
			InitializedAt: time.Now().UTC().Format(time.RFC3339),
		},
		Branches: BranchConfig{
			Remote:          "origin",
			MainPatterns:    []string{"main", "master", "llm/**"},
			ParentOverrides: map[string]string{},
		},
		Agent: AgentConfig{
			Engine: "claude",
			Command: map[string]string{
				"claude": "claude",
				"codex":  "codex",
			},
			Timeout: 1800,
		},
		Docs: DocsConfig{Output: ".docs-seed/docs"},
		Exclude: []string{
			".git/**", ".docs-seed/**", "node_modules/**", "vendor/**",
			"dist/**", "build/**", "target/**", "coverage/**",
		},
	}
}

func Load(root string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("未初始化：请先运行 docs-seed init")
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析 %s: %w", FileName, err)
	}
	if len(cfg.Branches.MainPatterns) == 0 {
		return Config{}, errors.New("branches.main_patterns 不能为空")
	}
	if cfg.Branches.Remote == "" {
		cfg.Branches.Remote = "origin"
	}
	if cfg.Docs.Output == "" {
		cfg.Docs.Output = ".docs-seed/docs"
	}
	if cfg.Agent.Timeout <= 0 {
		cfg.Agent.Timeout = 1800
	}
	return cfg, nil
}

func Save(root string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := []byte("# docs-seed 项目配置。主分支匹配决定哪些分支形成可阅读的文档谱系。\n")
	return os.WriteFile(filepath.Join(root, FileName), append(header, data...), 0o644)
}
