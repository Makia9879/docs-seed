package agent

import (
	"testing"
)

func TestClaudeWriteArgsDoNotBindSyntheticSessionID(t *testing.T) {
	args := claudeWriteArgs("/repo")
	if containsArg(args, "--session-id") {
		t.Fatalf("claude write args must not bind a synthetic session id: %#v", args)
	}
	if !containsArg(args, "--no-session-persistence") {
		t.Fatalf("claude write args must disable session persistence per action: %#v", args)
	}
	if !containsArgPair(args, "--add-dir", "/repo") {
		t.Fatalf("claude write args should keep add-dir support: %#v", args)
	}
}

func TestSessionInfoFromOutputUsesClaudeReportedSessionID(t *testing.T) {
	sessionID := "7c4adc4d-89b7-478b-9759-cb5a5389a44c"
	output := `{"type":"result","session_id":"` + sessionID + `","result":"done"}`

	if got := sessionInfoFromOutput(output); got != " session_id="+sessionID {
		t.Fatalf("sessionInfoFromOutput() = %q", got)
	}
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
