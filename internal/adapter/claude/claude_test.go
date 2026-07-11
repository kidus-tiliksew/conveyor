package claude

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
)

func TestParseStreamJSON(t *testing.T) {
	tests := []struct {
		line  string
		kind  adapter.EventKind
		check func(*testing.T, adapter.Event)
	}{
		{`{"type":"system","subtype":"init","session_id":"session-1"}`, adapter.EventSessionStart, func(t *testing.T, ev adapter.Event) {
			if ev.SessionRef != "session-1" {
				t.Fatalf("session=%q", ev.SessionRef)
			}
		}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}],"usage":{"input_tokens":12,"output_tokens":4}}}`, adapter.EventToolCall, func(t *testing.T, ev adapter.Event) {
			if ev.Tool != "Bash" || ev.Usage == nil || ev.Usage.In != 12 {
				t.Fatalf("event=%+v", ev)
			}
		}},
		{`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok"}]}}`, adapter.EventToolResult, func(t *testing.T, ev adapter.Event) {
			if ev.Tool != "tool-1" {
				t.Fatalf("tool=%q", ev.Tool)
			}
		}},
		{`{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.42,"session_id":"session-1"}`, adapter.EventAssistantText, func(t *testing.T, ev adapter.Event) {
			if ev.Text != "done" || ev.CostUSD != 0.42 {
				t.Fatalf("event=%+v", ev)
			}
		}},
	}
	for _, test := range tests {
		ev := parseLine([]byte(test.line))
		if ev.Kind != test.kind {
			t.Fatalf("kind=%q want %q for %s", ev.Kind, test.kind, test.line)
		}
		test.check(t, ev)
	}
}

func TestStreamParserEmitsUsageDeltaOncePerMessage(t *testing.T) {
	parser := newStreamParser()
	first := parser.parseLine([]byte(`{"type":"assistant","message":{"id":"message-1","content":[{"type":"text","text":"partial"}],"usage":{"input_tokens":12,"output_tokens":4}}}`))
	repeated := parser.parseLine([]byte(`{"type":"assistant","message":{"id":"message-1","content":[{"type":"tool_use","name":"Bash"}],"usage":{"input_tokens":12,"output_tokens":4}}}`))
	updated := parser.parseLine([]byte(`{"type":"assistant","message":{"id":"message-1","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":14,"output_tokens":7}}}`))
	if first.Usage == nil || first.Usage.In != 12 || first.Usage.Out != 4 {
		t.Fatalf("first usage = %+v", first.Usage)
	}
	if repeated.Usage != nil {
		t.Fatalf("repeated usage = %+v, want nil", repeated.Usage)
	}
	if updated.Usage == nil || updated.Usage.In != 2 || updated.Usage.Out != 3 {
		t.Fatalf("updated usage = %+v, want delta 2/3", updated.Usage)
	}
}

func TestCommandPolicyMapping(t *testing.T) {
	got := appendPolicy([]string{"base"}, adapter.ToolPolicy{
		AllowedCommands: [][]string{{"git", "status"}},
		DeniedCommands:  [][]string{{"printenv"}},
	})
	want := []string{"base", "--allowedTools", "Bash(git status)", "Bash(git status *)", "--disallowedTools", "Bash(printenv)", "Bash(printenv *)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want %q", got, want)
	}
}

func TestRunInvokesPinnedStreamInterface(t *testing.T) {
	tmp := t.TempDir()
	capture := filepath.Join(tmp, "args")
	t.Setenv("CAPTURE", capture)
	binary := filepath.Join(tmp, "claude")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo '2.1.206 (Claude Code)'
  exit 0
fi
printf '%s\n' "$@" > "$CAPTURE"
printf 'HOME=%s\n' "$CLAUDE_CONFIG_DIR" >> "$CAPTURE"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"session-1"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.01}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	a := New()
	a.Binary = binary
	a.Home = home
	a.ContainerConfined = true
	events, err := a.Run(context.Background(), adapter.RunSpec{
		Workdir: tmp, Prompt: "implement", BudgetUSD: 2,
		Policy: adapter.ToolPolicy{DeniedCommands: [][]string{{"printenv"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []adapter.EventKind
	for event := range events {
		kinds = append(kinds, event.Kind)
	}
	if !reflect.DeepEqual(kinds, []adapter.EventKind{adapter.EventSessionStart, adapter.EventAssistantText, adapter.EventDone}) {
		t.Fatalf("kinds = %v", kinds)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stream-json", "bypassPermissions", "Bash(printenv *)", "HOME=" + home} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
}

func TestResumeRetainsJobToolPolicy(t *testing.T) {
	a := New()
	a.DisableVersionCheck = true
	a.Binary = filepath.Join(t.TempDir(), "missing")
	a.policy = adapter.ToolPolicy{DeniedCommands: [][]string{{"env"}}}
	args, err := a.args("handoff", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	args = appendPolicy(args, a.policy)
	if !strings.Contains(strings.Join(args, "\n"), "Bash(env *)") {
		t.Fatalf("resume args lost policy: %v", args)
	}
}

func TestResumeUsesMainRunWorkdir(t *testing.T) {
	tmp := t.TempDir()
	capture := filepath.Join(tmp, "pwd")
	t.Setenv("CAPTURE", capture)
	binary := filepath.Join(tmp, "claude")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo '2.1.206 (Claude Code)'
  exit 0
fi
pwd > "$CAPTURE"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"session-1"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.Binary = binary
	events, err := a.Run(context.Background(), adapter.RunSpec{Workdir: tmp, Prompt: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	events, err = a.Resume(context.Background(), "session-1", "handoff")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != tmp {
		t.Fatalf("resume workdir = %q, want %q", strings.TrimSpace(string(got)), tmp)
	}
}
