package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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
	GenerateCommits(ctx context.Context, workDir, prompt string) ([]model.CommitFact, error)
	Write(ctx context.Context, workDir, prompt string, addDirs ...string) (string, error)
}

type Runner struct {
	Config config.AgentConfig
}

type commitFactBatch struct {
	Commits []model.CommitFact `json:"commits"`
}

func (r Runner) Generate(ctx context.Context, workDir, prompt string) (model.Fact, error) {
	content, err := r.run(ctx, workDir, prompt)
	if err != nil {
		return model.Fact{}, err
	}
	var fact model.Fact
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &fact); err != nil {
		return model.Fact{}, fmt.Errorf("Agent 输出不是有效文档事实 JSON: %w", err)
	}
	if len(fact.BusinessLogic) == 0 && len(fact.DataFlow) == 0 && len(fact.ArchitectureDecisions) == 0 {
		return model.Fact{}, errors.New("Agent 未生成业务逻辑、数据流或 ADR 内容")
	}
	return fact, nil
}

func (r Runner) GenerateCommits(ctx context.Context, workDir, prompt string) ([]model.CommitFact, error) {
	content, err := r.run(ctx, workDir, prompt)
	if err != nil {
		return nil, err
	}
	var batch commitFactBatch
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &batch); err != nil {
		return nil, fmt.Errorf("Agent 输出不是有效批量 commit facts JSON: %w", err)
	}
	if len(batch.Commits) == 0 {
		return nil, errors.New("Agent 未生成任何 commit facts")
	}
	return batch.Commits, nil
}

func (r Runner) run(ctx context.Context, workDir, prompt string) (string, error) {
	command := r.Config.Command[r.Config.Engine]
	if command == "" {
		command = r.Config.Engine
	}
	if command == "" {
		return "", errors.New("agent.engine 未配置")
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
	start := time.Now()
	fmt.Printf("      agent %s generate start: dir=%s timeout=%s\n", r.Config.Engine, workDir, timeout)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("调用 %s 失败: %w: %s", r.Config.Engine, err, strings.TrimSpace(stderr.String()))
	}
	fmt.Printf("      agent %s generate done: %s\n", r.Config.Engine, time.Since(start).Round(time.Millisecond))
	return extractContent(r.Config.Engine, stdout.String()), nil
}

func (r Runner) Write(ctx context.Context, workDir, prompt string, addDirs ...string) (string, error) {
	command := r.Config.Command[r.Config.Engine]
	if command == "" {
		command = r.Config.Engine
	}
	if command == "" {
		return "", errors.New("agent.engine 未配置")
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
		sessionID, err := newSessionID()
		if err != nil {
			return "", fmt.Errorf("生成 %s session id 失败: %w", r.Config.Engine, err)
		}
		args = []string{
			"--print",
			"--disable-slash-commands",
			"--dangerously-skip-permissions",
			"--permission-mode", "bypassPermissions",
			"--session-id", sessionID,
			"--allowedTools", "Read,Glob,Grep,LS,Write,Edit,MultiEdit,Bash",
			"--tools", "Read,Glob,Grep,LS,Write,Edit,MultiEdit,Bash",
		}
		for _, dir := range addDirs {
			if strings.TrimSpace(dir) != "" {
				args = append(args, "--add-dir", dir)
			}
		}
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	sessionInfo := sessionInfoFromArgs(args)
	fmt.Printf("      agent %s write start: dir=%s timeout=%s%s\n", r.Config.Engine, workDir, timeout, sessionInfo)
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("调用 %s 直写失败%s: %w: %s", r.Config.Engine, sessionInfo, err, combinedOutput(stderr.String(), stdout.String()))
	}
	fmt.Printf("      agent %s write done: %s%s\n", r.Config.Engine, time.Since(start).Round(time.Millisecond), sessionInfo)
	return stdout.String(), nil
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func sessionInfoFromArgs(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--session-id" && strings.TrimSpace(args[i+1]) != "" {
			return " session_id=" + args[i+1]
		}
	}
	return ""
}

func combinedOutput(stderrText, stdoutText string) string {
	stderrText = strings.TrimSpace(stderrText)
	stdoutText = strings.TrimSpace(stdoutText)
	switch {
	case stderrText != "" && stdoutText != "":
		return "stderr: " + stderrText + "\nstdout: " + stdoutText
	case stderrText != "":
		return stderrText
	case stdoutText != "":
		return stdoutText
	default:
		return "无 stdout/stderr 输出"
	}
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
