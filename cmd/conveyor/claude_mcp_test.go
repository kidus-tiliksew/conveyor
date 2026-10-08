package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// jsonMCPCredentialCases are credential strings whose literal or JSON-escaped
// form must never appear in a JSON-file registration, including values equal
// to ordinary registration text (req-security-boundaries AC-2.6).
var jsonMCPCredentialCases = []string{
	"worker-secret",
	"",
	`quote"inside`,
	`back\slash`,
	"line\nfeed\r\ncarriage",
	"unicode-éß-秘密- ",
	`"escaped\`,
	"</script>&<>",
	"conveyor",
	"http",
	"Bearer ${CONVEYOR_API_TOKEN}",
	"mcpServers",
	"\x00control\x1f",
}

func TestJSONMCPConfigCredentialIndependent(t *testing.T) {
	const endpoint = "http://127.0.0.1:8080/mcp"
	want, err := jsonMCPConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]map[string]any
	if err = json.Unmarshal(want, &decoded); err != nil {
		t.Fatal(err)
	}
	wantDecoded := map[string]map[string]map[string]any{"mcpServers": {"conveyor": {
		"type": "http", "url": endpoint, "headers": map[string]any{"Authorization": "Bearer ${CONVEYOR_API_TOKEN}"},
	}}}
	if !reflect.DeepEqual(decoded, wantDecoded) {
		t.Fatalf("registration = %#v, want %#v", decoded, wantDecoded)
	}
	// The renderer takes no credential, so every launch writes the same bytes
	// whatever the child credential is. Drive the launcher-facing writer with
	// each credential in the child's environment and compare byte for byte.
	for _, credential := range jsonMCPCredentialCases {
		directory := t.TempDir()
		setCredentialEnvironment(t, credential)
		path, err := prepareMCPConfig(directory, "http://127.0.0.1:8080", config.MCPTransportJSONFile)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("registration for credential %q differs:\n%s\n%s", credential, got, want)
		}
		assertNoCredentialForm(t, got, credential, want)
	}
}

func FuzzJSONMCPConfigCredentialIndependent(f *testing.F) {
	for _, credential := range jsonMCPCredentialCases {
		f.Add(credential)
	}
	baseline, err := jsonMCPConfig("http://127.0.0.1:8080/mcp")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, credential string) {
		setCredentialEnvironment(t, credential)
		path, err := prepareMCPConfig(t.TempDir(), "http://127.0.0.1:8080", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, baseline) {
			t.Fatalf("registration depends on the environment credential %q", credential)
		}
		assertNoCredentialForm(t, got, credential, baseline)
	})
}

// setCredentialEnvironment places the credential where a launched child finds
// it, so a renderer that read the environment would be caught; a value the
// environment cannot hold (NUL) is left unset.
func setCredentialEnvironment(t *testing.T, credential string) {
	t.Helper()
	if strings.ContainsRune(credential, 0) {
		t.Setenv("CONVEYOR_API_TOKEN", "")
		return
	}
	t.Setenv("CONVEYOR_API_TOKEN", credential)
}

// assertNoCredentialForm checks the literal and JSON-escaped credential. A
// credential that is a substring of the credential-free baseline is ordinary
// registration text, which byte equality with the baseline already covers.
func assertNoCredentialForm(t *testing.T, data []byte, credential string, baseline []byte) {
	t.Helper()
	if credential == "" {
		return
	}
	escaped, _ := json.Marshal(credential)
	forms := [][]byte{[]byte(credential), bytes.Trim(escaped, `"`)}
	for _, form := range forms {
		if len(form) == 0 || bytes.Contains(baseline, form) {
			continue
		}
		if bytes.Contains(data, form) {
			t.Fatalf("registration carries credential form %q", form)
		}
	}
}

func claudeTestHarness(name string, command ...string) config.Harness {
	return config.Harness{Name: name, Command: command, ModelArgs: []string{"--model", "{model}"}, ResumeCommand: []string{"--resume", "{session_id}"},
		EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{"claude", "--version"}, ProbeTimeoutText: "10s"}
}

func TestClaudeJSONMCPAdapterAdmission(t *testing.T) {
	template := config.HarnessTemplates()[1].Harness
	supported := []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--strict-mcp-config", "--output-format", "stream-json", "--verbose"}
	legacy := []string{"claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--allowedTools", "mcp__conveyor__*", "--output-format", "stream-json", "--verbose"}
	withTransport := func(harness config.Harness, transport string) config.Harness {
		harness.MCPTransport = transport
		return harness
	}
	codex := config.HarnessTemplates()[0].Harness
	codex.MCPTransport = config.MCPTransportJSONFile
	codex.Command = []string{"codex", "exec", "{prompt}", "--mcp-config", "{mcp_config}"}
	tests := []struct {
		name    string
		harness config.Harness
		refuse  string
	}{
		{name: "built-in claude template", harness: template},
		{name: "renamed claude", harness: withTransport(claudeTestHarness("my-agent", supported...), config.MCPTransportJSONFile)},
		{name: "absolute claude path", harness: claudeTestHarness("abs", append([]string{"/usr/local/bin/claude"}, supported[1:]...)...)},
		{name: "omitted transport claude", harness: claudeTestHarness("omitted", supported...)},
		{name: "claude without strict loading is admitted for launch insertion", harness: claudeTestHarness("legacy", legacy...)},
		{name: "equals output format", harness: claudeTestHarness("eq", "claude", "--print", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format=stream-json", "--verbose")},
		{name: "arbitrary wrapper named claude", harness: claudeTestHarness("claude", "/opt/bin/claude-wrapper", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format", "stream-json", "--verbose"), refuse: "does not run the claude executable"},
		{name: "generic agent-cli", harness: withTransport(config.Harness{Name: "local-agent", Command: []string{"agent-cli", "--prompt", "{prompt}", "--mcp-config", "{mcp_config}"}}, config.MCPTransportJSONFile), refuse: "does not run the claude executable"},
		{name: "omitted transport wrapper", harness: config.Harness{Name: "wrapper", Command: []string{"sh", "-c", "exec claude", "{prompt}", "{mcp_config}"}}, refuse: "does not run the claude executable"},
		{name: "codex as json_file", harness: codex, refuse: "does not run the claude executable"},
		{name: "second mcp-config flag", harness: claudeTestHarness("dup", append(append([]string(nil), supported...), "--mcp-config", "/home/user/other.json")...), refuse: "must be followed by the generated"},
		{name: "mcp-config equals form", harness: claudeTestHarness("eqcfg", append(append([]string(nil), supported...), "--mcp-config=/home/user/other.json")...), refuse: "additional --mcp-config"},
		{name: "extra config after generated file", harness: claudeTestHarness("variadic", "claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "/home/user/other.json", "--output-format", "stream-json", "--verbose"), refuse: "must be a flag"},
		{name: "config flag in effort args", harness: func() config.Harness {
			h := claudeTestHarness("effort", supported...)
			h.EffortArgs = map[string][]string{"high": {"--mcp-config", "/tmp/x.json"}}
			return h
		}(), refuse: "must be followed by the generated"},
		{name: "text output", harness: claudeTestHarness("text", "claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format", "text", "--verbose"), refuse: "must be stream-json"},
		{name: "json output equals form", harness: claudeTestHarness("json", "claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format=json", "--verbose"), refuse: "must be stream-json"},
		{name: "missing output format", harness: claudeTestHarness("none", "claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--verbose"), refuse: "stream-json is required"},
		{name: "missing verbose", harness: claudeTestHarness("quiet", "claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format", "stream-json"), refuse: "--verbose is required"},
		{name: "interactive claude", harness: claudeTestHarness("interactive", "claude", "{prompt}", "--mcp-config", "{mcp_config}", "--output-format", "stream-json", "--verbose"), refuse: "headless -p"},
		{name: "toml transport is not inspected", harness: withTransport(config.Harness{Name: "codex", Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}}, config.MCPTransportTOMLOverride)},
		{name: "environment transport is not inspected", harness: config.HarnessTemplates()[2].Harness},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := admitJSONMCPHarness(test.harness)
			if test.refuse == "" {
				if err != nil {
					t.Fatalf("refused supported harness: %v", err)
				}
				return
			}
			var adapterErr *jsonMCPAdapterError
			if !errors.As(err, &adapterErr) || !strings.Contains(err.Error(), test.refuse) {
				t.Fatalf("err = %v, want refusal containing %q", err, test.refuse)
			}
			message := err.Error()
			for _, want := range []string{`harness "` + test.harness.Name + `"`, "recreate the harness from the built-in claude template", "conveyor config init-execution", "codex toml_override", "never writes a credential"} {
				if !strings.Contains(message, want) {
					t.Fatalf("refusal %q does not name %q", message, want)
				}
			}
			if strings.Contains(message, "Bearer") || strings.Contains(message, "mcp add") {
				t.Fatalf("refusal suggests a literal credential path: %q", message)
			}
		})
	}
}

func TestClaudeJSONMCPLaunchArgvInsertsStrictLoading(t *testing.T) {
	harness := claudeTestHarness("legacy")
	const configPath = "/attempt/mcp.json"
	legacy := []string{"claude", "-p", "prompt", "--mcp-config", configPath, "--allowedTools", "mcp__conveyor__*", "--output-format", "stream-json", "--verbose", "--resume", "native"}
	got, err := claudeJSONMCPLaunchArgv(harness, legacy, configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "-p", "prompt", "--mcp-config", configPath, "--strict-mcp-config", "--allowedTools", "mcp__conveyor__*", "--output-format", "stream-json", "--verbose", "--resume", "native"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if legacy[5] != "--allowedTools" {
		t.Fatalf("launch argv check mutated its input: %q", legacy)
	}
	again, err := claudeJSONMCPLaunchArgv(harness, got, configPath)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("strict argv changed on re-check: %q err=%v", again, err)
	}
	if _, err = claudeJSONMCPLaunchArgv(harness, []string{"claude", "-p", "prompt", "--mcp-config", "/other/mcp.json", "--output-format", "stream-json", "--verbose"}, configPath); err == nil {
		t.Fatal("launch argv accepted a configuration other than the generated file")
	}
	if _, err = claudeJSONMCPLaunchArgv(harness, []string{"wrapper", "-p", "prompt", "--mcp-config", configPath, "--output-format", "stream-json", "--verbose"}, configPath); err == nil {
		t.Fatal("launch argv accepted a non-claude executable")
	}
}

// claudeInitLine is a stream-json initialization event with the fields the
// receipt reads; extra mirrors fields Claude Code 2.1.284 also emits.
func claudeInitLine(servers []map[string]any, tools []string) string {
	data, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "init", "session_id": "native", "cwd": "/work", "model": "claude-opus-5-5",
		"mcp_servers": servers, "tools": tools, "claude_code_version": "2.1.284",
	})
	return string(data)
}

func claudeStageToolNames(stage core.Stage) []string {
	tools := []string{"Bash", "Read"}
	for _, tool := range claudeStageTools(stage) {
		tools = append(tools, "mcp__conveyor__"+tool)
	}
	return tools
}

func connectedConveyor() []map[string]any {
	return []map[string]any{{"name": "conveyor", "status": "connected", "source": "dynamic"}}
}

func feedReceipt(t *testing.T, stage core.Stage, chunks ...string) (error, bool) {
	t.Helper()
	receipt := newClaudeMCPReceipt(stage)
	for _, chunk := range chunks {
		if n, err := receipt.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("receipt write n=%d err=%v", n, err)
		}
	}
	select {
	case err := <-receipt.Result():
		return err, true
	default:
		return nil, false
	}
}

func TestClaudeMCPReceipt(t *testing.T) {
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		if err, decided := feedReceipt(t, stage, claudeInitLine(connectedConveyor(), claudeStageToolNames(stage))+"\n"); !decided || err != nil {
			t.Fatalf("stage %s valid receipt decided=%v err=%v", stage, decided, err)
		}
	}
	implementTools := claudeStageToolNames(core.StageImplement)
	valid := claudeInitLine(connectedConveyor(), implementTools)
	tests := []struct {
		name    string
		stage   core.Stage
		chunks  []string
		wantErr string // empty with decided means accepted
		pending bool
	}{
		{name: "split writes", stage: core.StageImplement, chunks: []string{valid[:7], valid[7:40], valid[40:] + "\n"}},
		{name: "crlf", stage: core.StageImplement, chunks: []string{valid + "\r\n"}},
		{name: "unrelated events first", stage: core.StageImplement, chunks: []string{"\n", `{"type":"system","subtype":"hook_started"}` + "\n", `{"type":"stream_event"}` + "\n", valid + "\n"}},
		{name: "server without source", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "connected"}}, implementTools) + "\n"}},
		{name: "other servers tolerated", stage: core.StageImplement, chunks: []string{claudeInitLine(append(connectedConveyor(), map[string]any{"name": "other", "status": "failed"}), implementTools) + "\n"}},
		{name: "null error", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "connected", "error": nil}}, implementTools) + "\n"}},
		{name: "unterminated line", stage: core.StageImplement, chunks: []string{valid}, pending: true},
		{name: "no init yet", stage: core.StageImplement, chunks: []string{`{"type":"assistant"}` + "\n"}, pending: true},
		{name: "missing server", stage: core.StageImplement, chunks: []string{claudeInitLine(nil, implementTools) + "\n"}, wantErr: "does not report the conveyor MCP server"},
		{name: "wrong server", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor-other", "status": "connected"}}, implementTools) + "\n"}, wantErr: "does not report the conveyor MCP server"},
		{name: "duplicate server", stage: core.StageImplement, chunks: []string{claudeInitLine(append(connectedConveyor(), connectedConveyor()...), implementTools) + "\n"}, wantErr: "more than once"},
		{name: "failed", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "failed", "error": "HTTP 401"}}, implementTools) + "\n"}, wantErr: "status is failed"},
		{name: "needs-auth", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "needs-auth"}}, implementTools) + "\n"}, wantErr: "status is needs-auth"},
		{name: "pending with cached tools", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "pending"}}, implementTools) + "\n"}, wantErr: "status is pending"},
		{name: "disabled", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "disabled"}}, implementTools) + "\n"}, wantErr: "status is disabled"},
		{name: "unrecognized status is not echoed", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "Bearer secret-value"}}, implementTools) + "\n"}, wantErr: "status is unrecognized"},
		{name: "connected with error", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "connected", "error": "skipped"}}, implementTools) + "\n"}, wantErr: "reports an error"},
		{name: "foreign source", stage: core.StageImplement, chunks: []string{claudeInitLine([]map[string]any{{"name": "conveyor", "status": "connected", "source": "user"}}, implementTools) + "\n"}, wantErr: "generated --mcp-config"},
		{name: "missing stage tool", stage: core.StageReview, chunks: []string{valid + "\n"}, wantErr: "lifecycle tool submit_review_verdict"},
		{name: "missing base tool", stage: core.StageImplement, chunks: []string{claudeInitLine(connectedConveyor(), []string{"mcp__conveyor__submit_for_review", "mcp__conveyor__get_work_order"}) + "\n"}, wantErr: "lifecycle tool release_work_order"},
		{name: "malformed json", stage: core.StageImplement, chunks: []string{"{\"type\":\"system\",\n"}, wantErr: "not stream-json"},
		{name: "plain text line", stage: core.StageImplement, chunks: []string{"Warning: something\n"}, wantErr: "not stream-json"},
		{name: "malformed server entry", stage: core.StageImplement, chunks: []string{`{"type":"system","subtype":"init","mcp_servers":["conveyor"],"tools":[]}` + "\n"}, wantErr: "malformed MCP server entry"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err, decided := feedReceipt(t, test.stage, test.chunks...)
			if test.pending {
				if decided {
					t.Fatalf("receipt decided early: %v", err)
				}
				return
			}
			if !decided {
				t.Fatal("receipt did not decide")
			}
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("valid receipt refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "HTTP 401") {
				t.Fatalf("receipt diagnostic echoed child output: %v", err)
			}
		})
	}
	// Only the first decision counts; a later valid init cannot overturn a
	// failure and nothing further is buffered.
	receipt := newClaudeMCPReceipt(core.StageImplement)
	_, _ = receipt.Write([]byte(claudeInitLine([]map[string]any{{"name": "conveyor", "status": "failed"}}, implementTools) + "\n" + valid + "\n"))
	if err := <-receipt.Result(); err == nil {
		t.Fatal("later valid init overturned the first decision")
	}
	_, _ = receipt.Write([]byte(valid + "\n"))
	select {
	case extra := <-receipt.Result():
		t.Fatalf("receipt delivered a second decision: %v", extra)
	default:
	}
}

// paddedInitLine returns a valid receipt line of exactly size bytes before
// its newline; JSON permits the trailing whitespace padding.
func paddedInitLine(t *testing.T, size int) string {
	t.Helper()
	line := claudeInitLine(connectedConveyor(), claudeStageToolNames(core.StageImplement))
	if len(line) > size {
		t.Fatalf("base line %d exceeds %d", len(line), size)
	}
	return line + strings.Repeat(" ", size-len(line))
}

func TestClaudeMCPReceiptBounds(t *testing.T) {
	exact := paddedInitLine(t, claudeReceiptLineLimit)
	if err, decided := feedReceipt(t, core.StageImplement, exact[:1000], exact[1000:], "\n"); !decided || err != nil {
		t.Fatalf("exactly 1 MiB receipt line decided=%v err=%v", decided, err)
	}
	over := paddedInitLine(t, claudeReceiptLineLimit+1)
	if err, decided := feedReceipt(t, core.StageImplement, over+"\n"); !decided || err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB") {
		t.Fatalf("1 MiB plus one byte decided=%v err=%v", decided, err)
	}
	// The unterminated overflow is refused as soon as it passes the limit,
	// without waiting for a newline or buffering further output.
	receipt := newClaudeMCPReceipt(core.StageImplement)
	_, _ = receipt.Write([]byte(over[:claudeReceiptLineLimit]))
	select {
	case err := <-receipt.Result():
		t.Fatalf("decided at exactly the limit: %v", err)
	default:
	}
	_, _ = receipt.Write([]byte(" "))
	if err := <-receipt.Result(); err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB") {
		t.Fatalf("overflow err = %v", err)
	}
	if len(receipt.buffer) != 0 {
		t.Fatalf("receipt retained %d bytes after deciding", len(receipt.buffer))
	}
	if got := jsonMCPReceiptTimeout(config.Harness{}); got != defaultJSONMCPReceiptTimeout {
		t.Fatalf("default receipt timeout = %s", got)
	}
	if got := jsonMCPReceiptTimeout(config.Harness{ProbeTimeoutText: "3s"}); got.String() != "3s" {
		t.Fatalf("probe-timeout receipt bound = %s", got)
	}
	// Launcher bounds: the probe timeout, EOF, and a zero exit without a
	// receipt all fail closed through the real child launcher.
	for _, mode := range []string{"silent-receipt-timeout", "eof-without-receipt", "zero-exit-without-receipt"} {
		t.Run(mode, func(t *testing.T) {
			result := runFakeClaudeLaunch(t, fakeClaudeLaunch{mode: mode, stage: core.StageImplement, probeTimeout: receiptBoundTimeout(mode)})
			if result.err == nil || !strings.Contains(result.err.Error(), "json_file MCP receipt failed") {
				t.Fatalf("err = %v", result.err)
			}
			if len(result.releases) != 1 || !strings.HasPrefix(result.releases[0].Reason, "json_file MCP receipt failed") || result.releases[0].Outcome != core.WorkOrderOutcomeChildFailure {
				t.Fatalf("releases = %+v", result.releases)
			}
			if result.launches > 1 {
				t.Fatalf("launches = %d", result.launches)
			}
		})
	}
}

// writeJSONFileWrapperConfig writes the example execution document with its
// Claude harness replaced by a generic json_file wrapper, as the pre-change
// example configured it.
func writeJSONFileWrapperConfig(t *testing.T) string {
	t.Helper()
	template, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Replace(string(template), exampleHarnessCommand, `command: [agent-cli, --prompt, "{prompt}", --mcp-config, "{mcp_config}"]`, 1)
	value = strings.Replace(value, "probe_command: [claude, --version]", "probe_command: [echo, agent-cli-probe-ok]", 1)
	if value == string(template) || !strings.Contains(value, "agent-cli, --prompt") {
		t.Fatal("example harness fixture was not replaced")
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestJSONMCPAdapterRefusedBeforeClaim proves an unsupported json_file
// harness never claims: conveyor run exits naming the harness and remedy, and
// the worker skips the order and leaves it queued (component-harness-execution;
// req-security-boundaries AC-2.4, AC-2.7).
func TestJSONMCPAdapterRefusedBeforeClaim(t *testing.T) {
	assertRefusal := func(t *testing.T, err error, configPath string) {
		t.Helper()
		for _, want := range []string{`harness "local-agent" cannot use the json_file MCP transport`, "does not run the claude executable", localExecutionSetupCommand + " --config " + configPath} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %v does not name %q", err, want)
			}
		}
	}
	t.Run("run", func(t *testing.T) {
		t.Setenv("CONVEYOR_TEST_RUN_PROBE_LOG", filepath.Join(t.TempDir(), "probes.log"))
		configPath := writeJSONFileWrapperConfig(t)
		result := runProbeScenario(t, t.Context(), configPath, "", nil)
		if result.claims != 0 || len(result.other) != 0 {
			t.Fatalf("unsupported json_file harness claimed=%d other=%v", result.claims, result.other)
		}
		assertRefusal(t, result.err, configPath)
		if !strings.Contains(result.output, "Next: spec work order target-spec-1") {
			t.Fatalf("pending order was not presented: %q", result.output)
		}
	})
	t.Run("worker", func(t *testing.T) {
		t.Setenv("CONVEYOR_WORKER_TOKEN", "worker-credential")
		t.Setenv(localGitTokenEnv, "")
		configPath := writeJSONFileWrapperConfig(t)
		var mu sync.Mutex
		var refusals []error
		previous := reportWorkerAdapterRefusal
		reportWorkerAdapterRefusal = func(_ workerservice.DispatchOrder, err error) {
			mu.Lock()
			refusals = append(refusals, err)
			mu.Unlock()
		}
		t.Cleanup(func() { reportWorkerAdapterRefusal = previous })
		item := workerservice.DispatchOrder{
			Task:       core.Task{ID: "task", Repo: "repo", BaseBranch: "main"},
			Repository: config.Repo{URL: "https://example.test/repo.git"},
			Order:      core.WorkOrder{ID: "order-implement", Stage: core.StageImplement, Claimable: true, State: core.WorkOrderQueued},
		}
		var claims int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch r.URL.Path {
			case "/v1/worker/heartbeat":
				_ = json.NewEncoder(w).Encode(core.Worker{ID: "worker"})
			case "/v1/worker/work-orders":
				_ = json.NewEncoder(w).Encode([]workerservice.DispatchOrder{item})
			case "/v1/worker/work-orders/" + item.Order.ID + "/claim":
				claims++
				http.Error(w, "unexpected claim", http.StatusConflict)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		c := &client{base: server.URL, workspace: "demo", gitPreflight: func(context.Context, workerservice.DispatchOrder, []string) error { return nil }}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := runWorkerWithPolicyAndConfig(ctx, c, "", "test", true, defaultWorkerReconnectPolicy, configPath); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if claims != 0 || len(refusals) == 0 {
			t.Fatalf("claims=%d refusals=%v", claims, refusals)
		}
		assertRefusal(t, refusals[0], configPath)
	})
}

type recordingWriter struct {
	mu   sync.Mutex
	data []byte
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *recordingWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.data)
}

// TestClaudeReceiptIngressGuard proves the raw-stdout bound ahead of the
// redactor: an unterminated line of exactly 1 MiB passes, one more byte trips
// the guard and fails the receipt at once, complete lines before the overflow
// still pass, nothing passes afterwards, and a confirmed receipt makes the
// guard a pass-through.
func TestClaudeReceiptIngressGuard(t *testing.T) {
	t.Run("exact limit then overflow", func(t *testing.T) {
		receipt := newClaudeMCPReceipt(core.StageImplement)
		sink := &recordingWriter{}
		guard := newClaudeReceiptIngressGuard(sink, receipt)
		half := bytes.Repeat([]byte("a"), claudeReceiptLineLimit/2)
		for _, chunk := range [][]byte{half, half} {
			if n, err := guard.Write(chunk); err != nil || n != len(chunk) {
				t.Fatalf("write n=%d err=%v", n, err)
			}
		}
		if sink.Len() != claudeReceiptLineLimit || !receipt.pending() {
			t.Fatalf("exactly 1 MiB: forwarded=%d pending=%v", sink.Len(), receipt.pending())
		}
		if n, err := guard.Write([]byte("b")); err != nil || n != 1 {
			t.Fatalf("overflow write n=%d err=%v", n, err)
		}
		select {
		case err := <-receipt.Result():
			if !errors.Is(err, errClaudeReceiptLineOverflow) {
				t.Fatalf("receipt err = %v", err)
			}
		default:
			t.Fatal("overflow did not fail the receipt immediately")
		}
		_, _ = guard.Write([]byte("more\n" + strings.Repeat("c", 4096)))
		if sink.Len() != claudeReceiptLineLimit {
			t.Fatalf("guard forwarded %d bytes after tripping", sink.Len()-claudeReceiptLineLimit)
		}
	})
	t.Run("complete lines before the overflow pass", func(t *testing.T) {
		receipt := newClaudeMCPReceipt(core.StageImplement)
		sink := &recordingWriter{}
		guard := newClaudeReceiptIngressGuard(sink, receipt)
		prefix := "{\"type\":\"system\",\"subtype\":\"hook_started\"}\n"
		chunk := append([]byte(prefix), bytes.Repeat([]byte("z"), claudeReceiptLineLimit+10)...)
		_, _ = guard.Write(chunk)
		if sink.Len() != len(prefix)+claudeReceiptLineLimit {
			t.Fatalf("forwarded %d, want %d", sink.Len(), len(prefix)+claudeReceiptLineLimit)
		}
		if err := <-receipt.Result(); !errors.Is(err, errClaudeReceiptLineOverflow) {
			t.Fatalf("receipt err = %v", err)
		}
	})
	t.Run("newline resets the line length", func(t *testing.T) {
		receipt := newClaudeMCPReceipt(core.StageImplement)
		sink := &recordingWriter{}
		guard := newClaudeReceiptIngressGuard(sink, receipt)
		line := append(bytes.Repeat([]byte("{"), 0), []byte(strings.Repeat(" ", claudeReceiptLineLimit)+"\n")...)
		_, _ = guard.Write(line)
		_, _ = guard.Write(line)
		if sink.Len() != 2*len(line) || !receipt.pending() {
			t.Fatalf("forwarded=%d pending=%v", sink.Len(), receipt.pending())
		}
	})
	t.Run("pass-through after a confirmed receipt", func(t *testing.T) {
		receipt := newClaudeMCPReceipt(core.StageImplement)
		sink := &recordingWriter{}
		guard := newClaudeReceiptIngressGuard(io.MultiWriter(receipt, sink), receipt)
		_, _ = guard.Write([]byte(claudeInitLine(connectedConveyor(), claudeStageToolNames(core.StageImplement)) + "\n"))
		if err := <-receipt.Result(); err != nil {
			t.Fatal(err)
		}
		before := sink.Len()
		big := bytes.Repeat([]byte("x"), 2*claudeReceiptLineLimit)
		_, _ = guard.Write(big)
		if sink.Len()-before != len(big) {
			t.Fatalf("confirmed receipt still bounded output: %d", sink.Len()-before)
		}
	})
}
