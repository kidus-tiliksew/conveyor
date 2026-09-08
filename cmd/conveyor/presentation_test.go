package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestRunChildReapNoticeUsesPresentationAndRawOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		presented  bool
		persistent bool
	}{
		{name: "raw"},
		{name: "presented output", presented: true},
		{name: "persistent notice", presented: true, persistent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw, interactive bytes.Buffer
			var presentation *runOutputPresentation
			if test.presented {
				presentation = &runOutputPresentation{output: &interactive, styled: true}
				if test.persistent {
					presentation.notice = func(message string) { _, _ = interactive.WriteString(message) }
				}
			}
			if err := presentRunChildReapNotice(&raw, presentation, "implement", "submitted"); err != nil {
				t.Fatal(err)
			}
			output := raw.String() + interactive.String()
			for _, want := range []string{"work order is submitted", "ending lingering implement session", "run can advance"} {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q: %q", want, output)
				}
			}
		})
	}
}

func TestHarnessEventRendererSummarizesRecognizedCodexEvents(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	longPayload := strings.Repeat("embedded-javascript-bundle ", 100)
	events := strings.Join([]string{
		`{"type":"thread.started","thread_id":"019abcdef0123456789"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Implemented the renderer and kept the stream consumers intact."}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"go test ./cmd/conveyor","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./cmd/conveyor","status":"completed","exit_code":0,"aggregated_output":"` + longPayload + `"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":42,"output_tokens":7}}`,
	}, "\n") + "\n"
	if _, err := renderer.Write([]byte(events)); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"session started", "Implemented the renderer", "go test ./cmd/conveyor", "running", "completed", "tokens in 42, out 7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "embedded-javascript-bundle") || len(got) >= len(events) {
		t.Fatalf("rendered output leaked the embedded payload:\n%s", got)
	}
}

func TestHarnessEventRendererSummarizesRecognizedCursorEvents(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	events := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"f873ed73-0123-4567-89ab-cdef01234567"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"the full prompt"}]},"session_id":"f873ed73"}`,
		`{"type":"thinking","subtype":"delta","text":"Running the shell command"}`,
		`{"type":"thinking","subtype":"delta","text":" with more detail"}`,
		`{"type":"thinking","subtype":"completed"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` + string(mustJSON(t, "I'll run `echo probe-ok`, then reply with exactly `done`.")) + `}]}}`,
		`{"type":"tool_call","subtype":"started","call_id":"call-2b5b\nfc-46e5","tool_call":{"shellToolCall":{"args":{"command":"echo probe-ok"}}}}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-2b5b\nfc-46e5","tool_call":{"shellToolCall":{"args":{"command":"echo probe-ok"},"result":{"success":{"exitCode":0,"stdout":"probe-ok\n"}}}}}`,
		`{"type":"thinking","subtype":"delta","text":"Finishing"}`,
		`{"type":"thinking","subtype":"completed"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"usage":{"inputTokens":13630,"outputTokens":89}}`,
	}, "\n") + "\n"
	if _, err := renderer.Write([]byte(events)); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(output.String()), "\n")
	want := []string{
		"session started f873ed73-0123 … [elided]",
		"thinking…",
		"I'll run `echo probe-ok`, then reply with exactly `done`.",
		"› echo probe-ok · running",
		"✓ echo probe-ok · completed (exit 0)",
		"thinking…",
		"done",
		"✓ agent turn completed · tokens in 13630, out 89",
	}
	if len(got) != len(want) {
		t.Fatalf("rendered lines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rendered line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if strings.Contains(output.String(), "the full prompt") || strings.Contains(output.String(), "probe-ok\\n") {
		t.Fatalf("rendered output leaked prompt or tool result: %q", output.String())
	}
}

func TestHarnessEventRendererHandlesCursorVariantsAndBoundsValues(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	longText := "first line\n" + strings.Repeat("assistant payload ", 100)
	longCommand := "printf first\n" + strings.Repeat("command ", 100)
	longCallID := "call first\n" + strings.Repeat("id ", 100)
	events := strings.Join([]string{
		`{"type":"tool_call","subtype":"started","call_id":"unknown\ncall","tool_call":{"mcpToolCall":{"arguments":{"secret":"must-not-render"}}}}`,
		`{"type":"tool_call","subtype":"completed","call_id":"unknown\ncall","tool_call":{"mcpToolCall":{"result":{"secret":"must-not-render"}}}}`,
		`{"type":"tool_call","subtype":"completed","tool_call":{"shellToolCall":{"args":{"command":"false"},"result":{"failure":{"stderr":"must-not-render"}}}}}`,
		`{"type":"tool_call","subtype":"completed","tool_call":{"shellToolCall":{"args":{"command":"exit 9"},"result":{"success":{"exitCode":9,"stderr":"must-not-render"}}}}}`,
		`{"type":"result","subtype":"error","is_error":true,"result":"must-not-render"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(mustJSON(t, longText)) + `}]}}`,
		`{"type":"tool_call","subtype":"started","call_id":"` + strings.Repeat("id", 200) + `","tool_call":{"shellToolCall":{"args":{"command":` + string(mustJSON(t, longCommand)) + `}}}}`,
		`{"type":"tool_call","subtype":"started","call_id":` + string(mustJSON(t, longCallID)) + `,"tool_call":{"shellToolCall":{"args":{}}}}`,
	}, "\n") + "\n"
	if _, err := renderer.Write([]byte(events)); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"› mcpToolCall · running",
		"✓ mcpToolCall · completed",
		"! false · completed",
		"! exit 9 · completed (exit 9)",
		"! agent turn completed · error",
		"› call first id id",
		presentationElisionTag,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"must-not-render", "first line\nassistant", "printf first\ncommand", "call first\nid"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("rendered output leaked %q:\n%s", forbidden, got)
		}
	}
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestHarnessEventRendererBoundsUnknownAndOversizedLines(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	unknown := "not-json " + strings.Repeat("payload ", 100)
	if _, err := renderer.Write([]byte(unknown[:100])); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Write([]byte(unknown[100:])); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Flush(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, presentationElisionTag) || len([]rune(strings.TrimSpace(got))) > harnessFallbackLimit+20 {
		t.Fatalf("unknown output was not deterministically bounded: %q", got)
	}
}

func TestHarnessEventRendererSummarizesMCPToolCalls(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	events := strings.Join([]string{
		`{"type":"item.started","item":{"type":"mcp_tool_call","tool":"conveyor.get_work_order","status":"in_progress","arguments":{"secret":"must-not-render"}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","name":"conveyor.report_progress","status":"completed","result":{"content":"large-result-must-not-render"}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"conveyor.submit_for_review","status":"failed","error":"large-error-must-not-render"}}`,
	}, "\n") + "\n"
	if _, err := renderer.Write([]byte(events)); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"conveyor.get_work_order", "in_progress", "conveyor.report_progress", "completed", "conveyor.submit_for_review", "failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"must-not-render", "arguments", "large-result", "large-error"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("rendered output leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestHarnessStdoutFanoutPreservesRawConsumers(t *testing.T) {
	raw := []byte(`{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}` + "\n")
	item := workerservice.DispatchOrder{Dispatch: "run"}

	t.Run("non-terminal or raw", func(t *testing.T) {
		var console, tail, usage bytes.Buffer
		fanout, renderer := harnessStdoutFanout(&console, &tail, &usage, item, nil)
		if renderer != nil {
			t.Fatal("raw path installed a renderer")
		}
		if _, err := fanout.Write(raw); err != nil {
			t.Fatal(err)
		}
		for name, got := range map[string][]byte{"console": console.Bytes(), "failure tail": tail.Bytes(), "usage": usage.Bytes()} {
			if !bytes.Equal(got, raw) {
				t.Fatalf("%s bytes changed: %q", name, got)
			}
		}
	})

	t.Run("interactive presentation", func(t *testing.T) {
		var console, tail, usage bytes.Buffer
		fanout, renderer := harnessStdoutFanout(&bytes.Buffer{}, &tail, &usage, item, &runOutputPresentation{output: &console, presentEvents: true})
		if renderer == nil {
			t.Fatal("interactive run did not install a renderer")
		}
		if _, err := fanout.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := renderer.Flush(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(console.String(), "agent turn completed") || bytes.Equal(console.Bytes(), raw) {
			t.Fatalf("console was not presented: %q", console.String())
		}
		if !bytes.Equal(tail.Bytes(), raw) || !bytes.Equal(usage.Bytes(), raw) {
			t.Fatalf("internal consumers changed: tail=%q usage=%q", tail.Bytes(), usage.Bytes())
		}
	})
}

func TestInteractiveHarnessPresentationReceivesOnlyRedactedBytes(t *testing.T) {
	const secret = "credential-that-must-not-render"
	raw := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"safe ` + secret + `"}}` + "\n")
	var console, tail bytes.Buffer
	item := workerservice.DispatchOrder{Dispatch: "run"}
	fanout, renderer := harnessStdoutFanout(&bytes.Buffer{}, &tail, nil, item, &runOutputPresentation{output: &console, presentEvents: true})
	redacted := &redact.Writer{Destination: fanout, Redactor: redact.New([]string{secret})}
	if _, err := redacted.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := redacted.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(console.String(), secret) || strings.Contains(tail.String(), secret) {
		t.Fatalf("secret reached a redacted destination: console=%q tail=%q", console.String(), tail.String())
	}
	if !strings.Contains(console.String(), "safe [REDACTED:exact]") || !strings.Contains(tail.String(), "[REDACTED:exact]") {
		t.Fatalf("redaction was not preserved: console=%q tail=%q", console.String(), tail.String())
	}
}

func TestRunCommandExposesRawEscapeHatch(t *testing.T) {
	flag := runCmd().Flags().Lookup("raw")
	if flag == nil || flag.DefValue != "false" {
		t.Fatalf("raw flag = %#v", flag)
	}
}

func TestStyledConfigRowsAlignAndAttributeSources(t *testing.T) {
	var output bytes.Buffer
	if err := renderCLIConfigRow(&output, true, "workspace", "demo", "stored file"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "workspace") || !strings.Contains(output.String(), "demo") || !strings.Contains(output.String(), "source: stored file") {
		t.Fatalf("styled config row = %q", output.String())
	}
}

func TestBoundTextNormalizesAndElides(t *testing.T) {
	got := boundText("line one\n\tline two "+strings.Repeat("x", 50), 32)
	if strings.ContainsAny(got, "\n\t") || !strings.HasSuffix(got, presentationElisionTag) {
		t.Fatalf("bound text = %q", got)
	}
}

func TestHarnessEventRendererSummarizesOpenCodeEvents(t *testing.T) {
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	events := strings.Join([]string{
		`{"type":"step_start","sessionID":"ses_example","part":{"type":"step-start"}}`,
		`{"type":"text","sessionID":"ses_example","part":{"type":"text","text":"OK"}}`,
		`{"type":"reasoning","part":{"text":"Checking the output"}}`,
		`{"type":"reasoning","part":{}}`,
		`{"type":"reasoning","part":{}}`,
		`{"type":"tool_use","part":{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"running","input":{"command":"echo OK"}}}}`,
		`{"type":"tool_use","part":{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"echo OK"},"output":"must-not-render","metadata":{"secret":"must-not-render"}}}}`,
		`{"type":"step_finish","sessionID":"ses_example","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":7356,"output":15}}}`,
		`{"type":"step_start","sessionID":"ses_example"}`,
		`{"type":"reasoning","part":{}}`,
		`{"type":"tool_use","part":{"tool":"bash","state":{"status":"error","input":{"command":"pwd"},"error":"The user rejected permission…"}}}`,
		`{"type":"tool_use","part":{"tool":"conveyor.get_work_order","state":{"status":"completed","input":{"command":"must-not-render"},"output":"must-not-render"}}}`,
		`{"type":"step_finish","sessionID":"ses_example","part":{"reason":"stop","tokens":{"input":97,"output":6},"cost":0}}`,
		`{"type":"error","sessionID":"ses_example","error":{"name":"UnknownError","data":{"message":"Token refresh failed: 401","extra":"must-not-render"}}}`,
		`{"type":"error","error":{"name":"UnknownError"}}`,
		`{"type":"step_finish","part":{"reason":"stop","tokens":{"input":0,"output":0}}}`,
	}, "\n") + "\n"
	if _, err := renderer.Write([]byte(events[:65])); err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Write([]byte(events[65:])); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"session started ses_example", "agent step started", "OK", "Checking the output", "thinking…",
		"› bash · echo OK · running", "✓ bash · echo OK · completed", "step finished", "agent step started", "thinking…",
		"! bash · pwd · error · The user rejected permission…", "✓ conveyor.get_work_order · completed",
		"✓ agent turn completed · tokens in 97, out 6", "! Token refresh failed: 401", "! UnknownError",
		"✓ agent turn completed · tokens in 0, out 0",
	}, "\n") + "\n"
	if got := output.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestOpenCodeRendererBoundsFieldsAndShowsFirstSession(t *testing.T) {
	long := "first\n" + strings.Repeat("payload ", 500)
	encoded := string(mustJSON(t, long))
	for _, test := range []struct{ name, event, want string }{
		{"text", `{"type":"text","sessionID":` + encoded + `,"part":{"text":` + encoded + `}}`, boundText(long, harnessDetailLimit)},
		{"reasoning", `{"type":"reasoning","part":{"text":` + encoded + `}}`, boundText(long, harnessDetailLimit)},
		{"tool name", `{"type":"tool_use","part":{"tool":` + encoded + `,"state":{"status":"completed"}}}`, "✓ " + boundText(long, harnessCommandLimit) + " · completed"},
		{"command", `{"type":"tool_use","part":{"tool":"bash","state":{"status":"running","input":{"command":` + encoded + `}}}}`, "› " + boundText("bash · "+long, harnessCommandLimit) + " · running"},
		{"status", `{"type":"tool_use","part":{"tool":"tool","state":{"status":` + encoded + `}}}`, "› tool · " + boundText(long, harnessCommandLimit)},
		{"call id", `{"type":"tool_use","part":{"callID":` + encoded + `,"state":{"status":"completed"}}}`, "✓ " + boundText(long, harnessCommandLimit) + " · completed"},
		{"tool error", `{"type":"tool_use","part":{"tool":"bash","state":{"status":"error","error":` + encoded + `}}}`, "! bash · error · " + boundText(long, harnessDetailLimit)},
		{"error message", `{"type":"error","error":{"name":"UnknownError","data":{"message":` + encoded + `}}}`, "! " + boundText(long, harnessDetailLimit)},
		{"error name", `{"type":"error","error":{"name":` + encoded + `}}`, "! " + boundText(long, harnessDetailLimit)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := newHarnessEventRenderer(&output)
			if _, err := renderer.Write([]byte(test.event)); err != nil {
				t.Fatal(err)
			}
			if err := renderer.Flush(); err != nil {
				t.Fatal(err)
			}
			want := test.want + "\n"
			if test.name == "text" {
				want = "session started " + boundText(long, 24) + "\n" + want
			}
			if got := output.String(); got != want {
				t.Fatalf("output=%q want=%q", got, want)
			}
		})
	}
	var output bytes.Buffer
	renderer := newHarnessEventRenderer(&output)
	for _, line := range []string{`{"type":"text","part":{"text":"before session"}}`, `{"type":"text","sessionID":"ses_late","part":{"text":"after session"}}`, `{"type":"step_start","sessionID":"ses_late"}`} {
		if err := renderer.renderLine(line); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(output.String(), "session started") != 1 || !strings.Contains(output.String(), "before session\nsession started ses_late\nafter session") {
		t.Fatalf("session not shown on first carrying event: %q", output.String())
	}
}

func TestOpenCodeFanoutPreservesRawStream(t *testing.T) {
	raw := []byte(`{"type":"text","sessionID":"ses_test","part":{"text":"OK"}}` + "\n" + `{"type":"step_finish","part":{"reason":"stop","tokens":{"input":3,"output":2}}}` + "\n")
	for _, presented := range []bool{false, true} {
		var console, tail, usage bytes.Buffer
		presentation := &runOutputPresentation{output: &console, presentEvents: presented}
		fanout, renderer := harnessStdoutFanout(&console, &tail, &usage, workerservice.DispatchOrder{Dispatch: "run"}, presentation)
		if _, err := fanout.Write(raw); err != nil {
			t.Fatal(err)
		}
		if renderer != nil {
			if err := renderer.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(tail.Bytes(), raw) || !bytes.Equal(usage.Bytes(), raw) {
			t.Fatal("internal stream changed")
		}
		if !presented && !bytes.Equal(console.Bytes(), raw) {
			t.Fatal("raw console changed")
		}
		if presented && !strings.Contains(console.String(), "✓ agent turn completed · tokens in 3, out 2") {
			t.Fatalf("missing presentation: %q", console.String())
		}
	}
}
