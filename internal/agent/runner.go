package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Makia9879/docs-seed/internal/config"
	"github.com/Makia9879/docs-seed/internal/model"
)

type Generator interface {
	Generate(ctx context.Context, workDir, prompt string) (model.Fact, error)
	Write(ctx context.Context, workDir, prompt string) error
}

type Runner struct {
	Config config.AgentConfig
}

func (r Runner) Generate(ctx context.Context, workDir, prompt string) (model.Fact, error) {
	command := r.Config.Command[r.Config.Engine]
	if command == "" {
		command = r.Config.Engine
	}
	if command == "" {
		return model.Fact{}, errors.New("agent.engine 未配置")
	}
	timeout := time.Duration(r.Config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var args []string
	switch r.Config.Engine {
	case "codex":
		args = []string{"--ask-for-approval", "never", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-rules", "--sandbox", "read-only", "--color", "never", "--json", "-"}
	default:
		args = []string{"--print", "--no-session-persistence", "--disable-slash-commands", "--output-format", "json", "--tools", "Read,Glob,Grep,LS"}
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return model.Fact{}, fmt.Errorf("调用 %s 失败: %w: %s", r.Config.Engine, err, strings.TrimSpace(stderr.String()))
	}
	content := extractContent(r.Config.Engine, stdout.String())
	var fact model.Fact
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &fact); err != nil {
		return model.Fact{}, fmt.Errorf("Agent 输出不是有效文档事实 JSON: %w", err)
	}
	if len(fact.BusinessLogic) == 0 && len(fact.DataFlow) == 0 && len(fact.ArchitectureDecisions) == 0 {
		return model.Fact{}, errors.New("Agent 未生成业务逻辑、数据流或 ADR 内容")
	}
	return fact, nil
}

func (r Runner) Write(ctx context.Context, workDir, prompt string) error {
	command := r.Config.Command[r.Config.Engine]
	if command == "" {
		command = r.Config.Engine
	}
	if command == "" {
		return errors.New("agent.engine 未配置")
	}
	timeout := time.Duration(r.Config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var args []string
	switch r.Config.Engine {
	case "codex":
		args = []string{"--ask-for-approval", "never", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-rules", "--sandbox", "workspace-write", "--color", "never", "-"}
	default:
		args = []string{"--print", "--no-session-persistence", "--disable-slash-commands", "--tools", "Read,Glob,Grep,LS,Write,Edit,MultiEdit"}
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("调用 %s 直写失败: %w: %s", r.Config.Engine, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractContent(engine, raw string) string {
	if engine == "claude" {
		var envelope map[string]any
		if json.Unmarshal([]byte(raw), &envelope) == nil {
			for _, key := range []string{"result", "content", "text"} {
				if value, ok := envelope[key].(string); ok {
					return value
				}
			}
		}
	}
	var last string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 1024), 8*1024*1024)
	for scanner.Scan() {
		var value any
		if json.Unmarshal(scanner.Bytes(), &value) == nil {
			findText(value, &last)
		}
	}
	if last != "" {
		return last
	}
	return raw
}

func findText(value any, last *string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if (key == "text" || key == "content" || key == "result") && child != nil {
				if text, ok := child.(string); ok && strings.Contains(text, "{") {
					*last = text
				}
			}
			findText(child, last)
		}
	case []any:
		for _, child := range typed {
			findText(child, last)
		}
	}
}

func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}
