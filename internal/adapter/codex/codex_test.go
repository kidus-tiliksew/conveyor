package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		kind adapter.EventKind
		text string
		tool string
	}{
		{
			name: "agent message",
			line: `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}`,
			kind: adapter.EventAssistantText,
			text: "done",
		},
		{
			name: "command start",
			line: `{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"ls"}}`,
			kind: adapter.EventToolCall,
			tool: "command_execution",
		},
		{
			name: "command end",
			line: `{"type":"item.completed","item":{"id":"item_2","type":"command_execution","exit_code":0}}`,
			kind: adapter.EventToolResult,
			tool: "command_execution",
		},
		{
			name: "usage on turn end",
			line: `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300}}`,
			kind: adapter.EventTokenUsage,
		},
		{
			name: "turn failure",
			line: `{"type":"turn.failed","error":{"message":"boom"}}`,
			kind: adapter.EventError,
		},
		{
			name: "session capture on thread start",
			line: `{"type":"thread.started","thread_id":"019f4c1c-95e9-7093-b910-d43afd08f268"}`,
			kind: adapter.EventSessionStart,
		},
		{
			name: "item_type field variant",
			line: `{"type":"item.completed","item":{"id":"item_3","item_type":"agent_message","text":"alt"}}`,
			kind: adapter.EventAssistantText,
			text: "alt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := parseLine([]byte(tc.line))
			if ev.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s", ev.Kind, tc.kind)
			}
			if tc.text != "" && ev.Text != tc.text {
				t.Fatalf("text = %q, want %q", ev.Text, tc.text)
			}
			if tc.tool != "" && ev.Tool != tc.tool {
				t.Fatalf("tool = %q, want %q", ev.Tool, tc.tool)
			}
			if len(ev.Payload) == 0 {
				t.Fatal("raw payload must always be preserved")
			}
		})
	}

	ev := parseLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}`))
	if ev.Usage == nil || ev.Usage.In != 12 || ev.Usage.Out != 3 {
		t.Fatalf("usage = %+v", ev.Usage)
	}

	ev = parseLine([]byte(`{"type":"thread.started","thread_id":"abc-123"}`))
	if ev.SessionRef != "abc-123" {
		t.Fatalf("session ref = %q, want abc-123", ev.SessionRef)
	}
}

func TestPreparePolicyWritesCodexExecPolicyRules(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	a := &Adapter{Home: home}
	policy := adapter.ToolPolicy{
		AllowedCommands: [][]string{{"git"}, {"go", "test"}},
		DeniedCommands:  [][]string{{"printenv"}, {"rm", "-rf"}},
	}
	if err := a.preparePolicy(policy); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "rules", "conveyor.rules")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{
		`pattern = ["go", "test"]`,
		`decision = "allow"`,
		`pattern = ["printenv"]`,
		`decision = "forbidden"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("rules missing %q:\n%s", expected, text)
		}
	}

	if binary, err := exec.LookPath("codex"); err == nil {
		out, err := exec.Command(binary, "execpolicy", "check", "--rules", path, "--", "printenv").CombinedOutput()
		if err != nil {
			t.Fatalf("Codex rejected generated rules: %v: %s", err, out)
		}
		if !strings.Contains(string(out), `"decision":"forbidden"`) && !strings.Contains(string(out), `"decision": "forbidden"`) {
			t.Fatalf("printenv was not forbidden: %s", out)
		}
	}
}

func TestRunOwnsUnattendedApprovalAndSandboxFlags(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'codex-cli ` + PinnedVersion + `\n'
  exit 0
fi
printf '%s\n' "$@" > "$CODEX_TEST_ARGS"
printf '{"type":"thread.started","thread_id":"test-session"}\n'
printf '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}\n'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_ARGS", argsPath)
	a := &Adapter{Binary: binary, Home: filepath.Join(dir, "home"), ContainerConfined: true}
	events, err := a.Run(context.Background(), adapter.RunSpec{
		Workdir: dir,
		Prompt:  "test",
		Policy:  adapter.ToolPolicy{DeniedCommands: [][]string{{"printenv"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	for _, expected := range []string{`approval_policy="never"`, "shell_environment_policy.ignore_default_excludes=true", "--sandbox", "danger-full-access"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("args missing %q: %s", expected, text)
		}
	}
}

func TestCheckVersionAcceptsPinnedVersion(t *testing.T) {
	a := &Adapter{Binary: versionBinary(t, "codex-cli "+PinnedVersion)}
	if err := a.checkVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersionRejectsDrift(t *testing.T) {
	a := &Adapter{Binary: versionBinary(t, "codex-cli 99.0.0")}
	err := a.checkVersion(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v, want unsupported-version error", err)
	}
}

func TestCheckVersionCanceledCallDoesNotPoisonAdapter(t *testing.T) {
	a := &Adapter{Binary: versionBinary(t, "codex-cli "+PinnedVersion)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.checkVersion(ctx); err == nil {
		t.Fatal("expected canceled version check")
	}
	if err := a.checkVersion(context.Background()); err != nil {
		t.Fatalf("retry after canceled check failed: %v", err)
	}
}

func versionBinary(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
