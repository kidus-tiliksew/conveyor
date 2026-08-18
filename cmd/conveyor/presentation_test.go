package main

import (
	"bytes"
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
