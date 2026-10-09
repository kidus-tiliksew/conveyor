package dispatch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

// This fixture exercises dispatch assembly and the actual provider serializer,
// rather than accepting an invalid image through a counter-only test.
func triageScreenshot(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 393))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(i), byte(i>>8), byte(i>>16), 255
	}
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 450<<10 || buf.Len() > 480<<10 {
		t.Fatalf("fixture size=%d", buf.Len())
	}
	return buf.Bytes()
}

func triageBudgetDispatcher(t *testing.T, agent inprocess.Agent) (*Dispatcher, store.Store, context.Context, core.Task) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := newHistoricalArtifactStore()
	task := core.Task{ID: "budget-task", Workspace: "demo", Repo: "api", Title: "Preserve screenshot", Body: "Exact task intent.", PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute}}}}
	d := New(st, cfg, agent)
	var err error
	d.Pack, err = pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	return d, st, ctx, task
}

func TestTriageScreenshotReachesProviderAndSurvivesContinuation(t *testing.T) {
	screenshot := triageScreenshot(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var user struct {
			Content []struct {
				Type string `json:"type"`
				URL  string `json:"image_url"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(request.Input[0], &user); err != nil {
			t.Error(err)
			return
		}
		found := false
		for _, content := range user.Content {
			if content.Type == "input_image" {
				got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content.URL, "data:image/png;base64,"))
				if err != nil || !bytes.Equal(got, screenshot) {
					t.Error("screenshot changed during serialization")
				}
				found = true
			}
		}
		if !found || !strings.Contains(user.Content[0].Text, "Exact task intent.") {
			t.Error("mandatory context missing")
		}
		if calls == 1 {
			_, _ = io.WriteString(w, `{"output":[{"type":"function_call","id":"fc1","call_id":"list","name":"list_requirements","arguments":"{}"}]}`)
			return
		}
		if len(request.Input) < 3 {
			t.Error("continuation missing tool exchange")
		}
		output := "```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Ready.\"}\n```"
		_ = json.NewEncoder(w).Encode(map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": output}}}}})
	}))
	defer server.Close()
	d, st, ctx, task := triageBudgetDispatcher(t, &inprocess.OpenAI{APIKey: "test", BaseURL: server.URL, Client: server.Client()})
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "screenshot.png", ContentType: "image/png", TaskID: task.ID}, screenshot); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Responses calls=%d", calls)
	}
}

func TestTriageMandatoryBudgetOveragesSuppressAgent(t *testing.T) {
	for _, tc := range []struct {
		name, mime, budget string
		sizes              []int
		limit              int
	}{
		{"text.txt", "text/plain", "text/history", []int{maxTriageInitialBytes + 1}, maxTriageInitialBytes},
		{"image.png", "image/png", "raw image", []int{maxModelImageBytes + 1}, maxModelImageBytes},
		{"file.pdf", "application/pdf", "raw attachment per-file", []int{maxModelAttachmentBytes + 1}, maxModelAttachmentBytes},
		{"combined.pdf", "application/pdf", "raw attachment combined", []int{20 << 20, 20 << 20, 11 << 20}, maxModelFileBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &capturingInputAgent{}
			d, st, ctx, task := triageBudgetDispatcher(t, agent)
			for i, size := range tc.sizes {
				content := bytes.Repeat([]byte{byte('a' + i)}, size)
				if tc.mime == "image/png" {
					copy(content, testimage.PNG(fmt.Sprint(i)))
				}
				if _, err := st.CreateArtifact(ctx, core.Artifact{Name: tc.name, ContentType: tc.mime, TaskID: task.ID}, content); err != nil {
					t.Fatal(err)
				}
			}
			err := d.DispatchNow(ctx, task.ID)
			if err == nil || agent.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, agent.calls)
			}
			for _, want := range []string{tc.budget, tc.name, fmt.Sprintf("%d-byte limit", tc.limit), "reduce"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error missing %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), strings.Repeat("a", 100)) {
				t.Fatal("content leaked")
			}
		})
	}
}

func TestTriageAdjacentImageSelectionUsesRawBudgetDuringAssembly(t *testing.T) {
	agent := &capturingInputAgent{}
	d, st, ctx, task := triageBudgetDispatcher(t, agent)
	sibling := core.Task{ID: "budget-sibling", Workspace: "demo", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, sibling); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: task.ID, DependsOnTaskID: sibling.ID, Reason: "context", RequestID: "budget-dependency"}); err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{20 << 20, 20 << 20, 20 << 20, 461 << 10} {
		owner := sibling.ID
		if i == 0 {
			owner = task.ID
		}
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("image-%d.png", i), ContentType: "image/png", TaskID: owner}, paddedArtifactPNG(i, size)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := d.buildStageInput(ctx, d.Cfg, core.StageTriage, task)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.buildStageInput(ctx, d.Cfg, core.StageTriage, task)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Attachments, second.Attachments) || first.Prompt != second.Prompt {
		t.Fatal("nondeterministic selection")
	}
	if len(first.Attachments) != 3 || first.Attachments[0].Name != "image-0.png" || !strings.Contains(first.Prompt, "raw attachment combined") {
		t.Fatalf("attachments=%d prompt missing omission=%t", len(first.Attachments), !strings.Contains(first.Prompt, "raw attachment combined"))
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || len(agent.input.Attachments) != 3 {
		t.Fatalf("calls=%d attachments=%d", agent.calls, len(agent.input.Attachments))
	}
}

func TestTriagePromptOverageSuppressesAgent(t *testing.T) {
	agent := &capturingInputAgent{}
	d, st, ctx, task := triageBudgetDispatcher(t, agent)
	task.ID = "large-intent"
	task.Body = strings.Repeat("private task intent", maxTriageInitialBytes/10)
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	err := d.DispatchNow(ctx, task.ID)
	if err == nil || agent.calls != 0 || !strings.Contains(err.Error(), "text/history") || !strings.Contains(err.Error(), "524288-byte limit") || strings.Contains(err.Error(), "private task intent") {
		t.Fatalf("budget rejection missing; calls=%d err=%v", agent.calls, err)
	}
}
