package dispatch

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTriageBudgetRetainsIntentAndProvenanceWhileOmittingLargeAdjacentLog(t *testing.T) {
	input := inprocess.Input{Prompt: "exact task intent\nREQ-1 confirmed governing instruction\nsource event 17; sibling path\n", Attachments: []inprocess.Attachment{
		{ID: "log", Name: "ci.log", Kind: inprocess.AttachmentDocument, ContentType: "text/plain", Content: bytes.Repeat([]byte("x"), 4864716)},
		{ID: "local", Name: "intent.md", Kind: inprocess.AttachmentDocument, Content: []byte("local instructions")},
		{ID: "small", Name: "outcome.md", Kind: inprocess.AttachmentDocument, Content: []byte("relevant outcome")},
	}}
	input.Prompt += "Context artifact supplied as document input: ci.log (text/plain, 4864716 bytes, id log)\n"
	artifacts := []core.Artifact{{ID: "log", TaskID: "sibling"}, {ID: "local", TaskID: "task"}, {ID: "small", TaskID: "sibling"}}
	got, err := boundTriageInput(input, artifacts, "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 2 || got.Attachments[0].ID != "local" || got.Attachments[1].ID != "small" {
		t.Fatalf("attachments=%v", got.Attachments)
	}
	for _, want := range []string{"exact task intent", "REQ-1 confirmed governing instruction", "source event 17; sibling path", "body omitted by triage byte budget: ci.log", "id log"} {
		if !strings.Contains(got.Prompt, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if measureTriageInput(got).TextBytes > maxTriageInitialBytes {
		t.Fatal("oversized input")
	}
	again, err := boundTriageInput(input, artifacts, "task")
	if err != nil || again.Prompt != got.Prompt {
		t.Fatal("selection not deterministic")
	}
}

func TestTriageBudgetRefusesMandatoryInputWithoutTruncation(t *testing.T) {
	for _, attachment := range []bool{false, true} {
		input := inprocess.Input{Prompt: "mandatory task"}
		if attachment {
			input.Attachments = []inprocess.Attachment{{ID: "local", Content: bytes.Repeat([]byte("x"), maxTriageInitialBytes)}}
		} else {
			input.Prompt = strings.Repeat("authority", maxTriageInitialBytes)
		}
		_, err := boundTriageInput(input, []core.Artifact{{ID: "local", TaskID: "task"}}, "task")
		if err == nil || !strings.Contains(err.Error(), "were not truncated") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestTriageOversizedCorpusBodyIsExplicitlyUnread(t *testing.T) {
	content := strings.Repeat("authority must not be partially cited", maxTriageToolResultBytes)
	for _, got := range []any{boundedTriageToolOutput(map[string]any{"content": content}), boundedTriagePromptJSON([]byte(content))} {
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), "body not supplied or read") || strings.Contains(string(raw), "preview") || len(raw) > 1024 {
			t.Fatalf("invalid refusal %s", raw)
		}
	}
}

func TestTriageContinuationBudgetReturnsVerdictWithoutSendingOversizedHistory(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	first := nativeCallResult("list", "list_requirements", "{}", "")
	item, _ := json.Marshal(map[string]any{"type": "reasoning", "content": strings.Repeat("r", maxTriageInputBytes)})
	first.ResponseItems = append(first.ResponseItems, item)
	agent := &sequenceAgent{results: []inprocess.Result{first}}
	result, err := New(store.NewMemory(), nil, agent).runTriageLoop(ctx, "model", inprocess.Input{Prompt: "intent"})
	if err != nil || len(agent.inputs) != 1 || !strings.Contains(result.Output, `"route":"proceed"`) || !strings.Contains(result.Output, "input byte allowance was exhausted") || !strings.Contains(string(result.Transcript), "provider_call_skipped") {
		t.Fatalf("calls=%d err=%v output=%s", len(agent.inputs), err, result.Output)
	}
}

func TestTriageCorpusReadsRespectCumulativeAllowance(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	req, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-large", Title: "Large confirmed body"}, core.RequirementVersion{Content: "# Large confirmed body\n\n" + strings.Repeat("governing text ", 3200), Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep authority whole."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	first := inprocess.Result{}
	for _, id := range []string{"one", "two", "three", "four"} {
		call := nativeCallResult(id, "read_requirement", `{"requirement_id":"req-large"}`, "")
		first.FunctionCalls = append(first.FunctionCalls, call.FunctionCalls...)
		first.ResponseItems = append(first.ResponseItems, call.ResponseItems...)
	}
	agent := &sequenceAgent{results: []inprocess.Result{first, nativeMessageResult("final")}}
	_, err = New(st, nil, agent).runTriageLoop(ctx, "model", inprocess.Input{Prompt: strings.Repeat("p", maxTriageInitialBytes-4096)})
	if err != nil || len(agent.inputs) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(agent.inputs))
	}
	next := agent.inputs[1]
	if measureTriageInput(next).TextBytes > maxTriageInputBytes {
		t.Fatal("oversized continuation sent")
	}
	outputs := next.Continuation.FunctionCallOutputs
	if len(outputs) != 4 || !strings.Contains(outputs[0].Output, "governing text") || !strings.Contains(outputs[3].Output, "body not supplied or read") || strings.Contains(outputs[3].Output, "governing text") {
		t.Fatalf("tool results did not retain whole bodies and explicit refusals")
	}
}

func TestTriageAttachmentAccountingTable(t *testing.T) {
	for _, tc := range []struct {
		name, mime string
		kind       inprocess.AttachmentKind
		text       bool
	}{
		{"shot.png", "image/png", inprocess.AttachmentImage, false},
		{"voice.mp3", "audio/mpeg", inprocess.AttachmentAudio, false},
		{"file.pdf", "application/pdf", inprocess.AttachmentDocument, false},
		{"file.txt", "text/plain; charset=utf-8", inprocess.AttachmentDocument, true},
		{"file.md", "application/octet-stream", inprocess.AttachmentDocument, true},
		{"file.json", "application/json", inprocess.AttachmentDocument, true},
		{"file.xml", "application/xml", inprocess.AttachmentDocument, true},
		{"file.html", "", inprocess.AttachmentDocument, true},
		{"file.csv", "", inprocess.AttachmentDocument, true},
		{"file.tsv", "", inprocess.AttachmentDocument, true},
		{"file.rtf", "application/rtf", inprocess.AttachmentDocument, true},
		{"file.doc", "", inprocess.AttachmentDocument, false},
		{"file.docx", "", inprocess.AttachmentDocument, false},
		{"file.odt", "", inprocess.AttachmentDocument, false},
		{"file.ppt", "", inprocess.AttachmentDocument, false},
		{"file.pptx", "", inprocess.AttachmentDocument, false},
		{"file.xls", "", inprocess.AttachmentDocument, false},
		{"file.xlsx", "", inprocess.AttachmentDocument, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := inprocess.Input{Prompt: "intent", Tools: []inprocess.FunctionTool{{Name: "read"}}, Continuation: &inprocess.Continuation{ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}, FunctionCallOutputs: []inprocess.FunctionCallOutput{{CallID: "call", Output: "body"}}}}
			baseline := measureTriageInput(input)
			attachment := inprocess.Attachment{ID: "artifact", Name: tc.name, ContentType: tc.mime, Kind: tc.kind, Content: bytes.Repeat([]byte("x"), 461<<10)}
			input.Attachments = []inprocess.Attachment{attachment}
			got := measureTriageInput(input)
			wantText := baseline.TextBytes + len(attachment.ID) + len(tc.name) + len(tc.mime) + len(tc.kind)
			if tc.text {
				wantText += len(attachment.Content)
			}
			if got.TextBytes != wantText || got.RawBytes != len(attachment.Content) {
				t.Fatalf("budget=%+v want text=%d raw=%d", got, wantText, len(attachment.Content))
			}
		})
	}
}

func TestTriageBudgetMetadataCannotHideBehindBinaryKind(t *testing.T) {
	input := inprocess.Input{Attachments: []inprocess.Attachment{{ID: "local", Name: strings.Repeat("m", maxTriageInitialBytes), Kind: inprocess.AttachmentImage, Content: []byte("image")}}}
	if _, err := boundTriageInput(input, []core.Artifact{{ID: "local", TaskID: "task"}}, "task"); err == nil || !strings.Contains(err.Error(), "text/history") {
		t.Fatalf("err=%v", err)
	}
}

func TestTriageBudgetSelectsAdjacentImagesByRawBudget(t *testing.T) {
	image := bytes.Repeat([]byte("i"), 20<<20)
	input := inprocess.Input{Prompt: "mandatory intent", Attachments: []inprocess.Attachment{
		{ID: "first", Name: "first.png", Kind: inprocess.AttachmentImage, Content: image},
		{ID: "local", Name: "local.png", Kind: inprocess.AttachmentImage, Content: image},
		{ID: "omitted", Name: "omitted.png", Kind: inprocess.AttachmentImage, Content: image},
		{ID: "last", Name: "last.png", Kind: inprocess.AttachmentImage, Content: image[:461<<10]},
	}}
	artifacts := []core.Artifact{{ID: "local", TaskID: "task"}}
	for attempt := 0; attempt < 2; attempt++ {
		got, err := boundTriageInput(input, artifacts, "task")
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, a := range got.Attachments {
			ids = append(ids, a.ID)
		}
		if strings.Join(ids, ",") != "local,first,last" {
			t.Fatalf("ids=%v", ids)
		}
		for _, want := range []string{"omitted.png", "id omitted", "raw attachment combined", "62914560 bytes", "52428800-byte limit"} {
			if !strings.Contains(got.Prompt, want) {
				t.Fatalf("missing %q in omission", want)
			}
		}
	}
}

func TestTriageTranscriptBudgetSuppressesResponses(t *testing.T) {
	for _, tc := range []struct {
		name            string
		transcriptSizes []int
		wantResponses   int
		wantError       bool
	}{
		{"small transcript continuation", []int{40, 40}, 2, false},
		{"initial overage", []int{maxTriageInitialBytes}, 0, true},
		{"continuation overage", []int{40, maxTriageInputBytes}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses, transcriptions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/audio/transcriptions" {
					size := tc.transcriptSizes[transcriptions]
					transcriptions++
					_ = json.NewEncoder(w).Encode(map[string]string{"text": strings.Repeat("s", size)})
					return
				}
				responses++
				if responses == 1 {
					_, _ = io.WriteString(w, `{"output":[{"type":"function_call","id":"fc1","call_id":"list","name":"list_requirements","arguments":"{}"}]}`)
					return
				}
				_, _ = io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"final"}]}]}`)
			}))
			defer server.Close()
			client := &inprocess.OpenAI{APIKey: "test", BaseURL: server.URL, Client: server.Client()}
			input := inprocess.Input{Prompt: "intent", Attachments: []inprocess.Attachment{{ID: "voice", Name: "voice.mp3", Kind: inprocess.AttachmentAudio, ContentType: "audio/mpeg", Content: bytes.Repeat([]byte("a"), 461<<10)}}}
			result, err := New(store.NewMemory(), nil, client).runTriageLoop(store.WithWorkspace(t.Context(), "demo"), "model", input)
			if (err != nil) != tc.wantError || responses != tc.wantResponses || transcriptions != len(tc.transcriptSizes) {
				t.Fatalf("err=%v Responses=%d transcriptions=%d", err, responses, transcriptions)
			}
			if tc.wantError {
				if !strings.Contains(err.Error(), "text/history") || !strings.Contains(err.Error(), "voice.mp3") {
					t.Fatalf("err=%v", err)
				}
			} else if tc.wantResponses == 1 {
				if !strings.Contains(result.Output, `"route":"proceed"`) || !strings.Contains(result.Output, "text/history") || !strings.Contains(string(result.Transcript), "provider_call_skipped") {
					t.Fatalf("missing fallback diagnostic: %s", result.Output)
				}
			}
			if strings.Contains(string(result.Transcript), strings.Repeat("s", 100)) {
				t.Fatal("transcript content leaked into audit")
			}
		})
	}
}

func TestTriageOmissionMetadataEvictsOptionalBodyBeforeMandatoryContext(t *testing.T) {
	input := inprocess.Input{Prompt: "mandatory intent", Attachments: []inprocess.Attachment{
		{ID: "first", Name: "first.txt", Kind: inprocess.AttachmentDocument, Content: bytes.Repeat([]byte("a"), 1000)},
		{ID: "second", Name: "second.txt", Kind: inprocess.AttachmentDocument, Content: bytes.Repeat([]byte("b"), 2000)},
	}}
	// Leave room for the first body, but not both it and the second omission.
	baseline, err := boundTriageInput(inprocess.Input{Prompt: input.Prompt}, nil, "task")
	if err != nil {
		t.Fatal(err)
	}
	input.Prompt += strings.Repeat("p", maxTriageInitialBytes-measureTriageInput(baseline).TextBytes-1100)
	got, err := boundTriageInput(input, nil, "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 0 || !strings.Contains(got.Prompt, input.Prompt) || !strings.Contains(got.Prompt, "first.txt") || !strings.Contains(got.Prompt, "second.txt") || measureTriageInput(got).TextBytes > maxTriageInitialBytes {
		t.Fatal("omission did not preserve mandatory context within budget")
	}
}
