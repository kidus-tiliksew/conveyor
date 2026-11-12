package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	failureAPIKey       = "fixture-provider-api-key-value"
	failurePatToken     = "cv_pat_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6kM9nP2r"
	failureSessionToken = "cv_session_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6kM9nP2r"
	failureGitHubToken  = "github_pat_11AAAAAAA0fixture_Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6k"
	failureProviderKey  = "sk-fixture0provider0key0value"
	failurePathValue    = "tenant-path-credential-fixture"
	failureQueryValue   = "query-credential-fixture"
	failurePassword     = "userinfo-password-fixture"
)

type failureRoundTripper func(*http.Request) (*http.Response, error)

func (f failureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// storedFailure runs the triage stage against client and returns what the
// store then holds: the job.failed payload and the audit transcript's bytes.
func storedFailure(t *testing.T, id string, client *inprocess.OpenAI) (map[string]any, string, string, core.Transcript) {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.SystemActor())
	st := store.NewMemory()
	task := core.Task{ID: id, Workspace: "demo", Repo: "api", Title: "Small fix", Body: "Fix the thing.", Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "model-a", Timeout: time.Minute}}}
	dispatcher := New(st, cfg, client)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	var payload map[string]any
	for _, event := range events {
		if event.Kind != "job.failed" {
			continue
		}
		raw = string(event.Payload)
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if payload == nil {
		t.Fatalf("no job.failed event among %d events", len(events))
	}
	job, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("job ok=%t err=%v", ok, err)
	}
	transcript, err := st.GetTranscript(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, content, err := st.GetArtifact(ctx, strings.TrimPrefix(transcript.URI, "artifact://"))
	if err != nil {
		t.Fatal(err)
	}
	return payload, raw, string(content), transcript
}

func requireNoFailureSecret(t *testing.T, name, text string) {
	t.Helper()
	for label, value := range map[string]string{
		"api key": failureAPIKey, "personal token": failurePatToken, "session token": failureSessionToken,
		"GitHub token": failureGitHubToken, "provider key": failureProviderKey, "path credential": failurePathValue,
		"query credential": failureQueryValue, "userinfo password": failurePassword,
	} {
		if strings.Contains(text, value) {
			t.Fatalf("%s stores the %s", name, label)
		}
	}
}

func failureDiagnostic(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	diagnostic, ok := payload["diagnostic"].(map[string]any)
	if !ok {
		t.Fatalf("job.failed has no diagnostic: keys %d", len(payload))
	}
	return diagnostic
}

// TestStoredProviderFailureHoldsNoSecret inspects what job.failed, its
// diagnostic, and the audit transcript store for each provider failure shape
// (req-security-boundaries REQ-2, REQ-3, AC-3.1; component-triage).
func TestStoredProviderFailureHoldsNoSecret(t *testing.T) {
	t.Parallel()
	t.Run("200 embedded failure", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-request-id", failureSessionToken)
			_, _ = fmt.Fprintf(w, `{"status":"failed","error":{"code":%q,"message":%q}}`, failureGitHubToken, "upstream echoed "+failurePatToken+" and "+failureAPIKey+" and "+failureProviderKey)
		}))
		defer server.Close()
		payload, raw, transcript, record := storedFailure(t, "failure-embedded", &inprocess.OpenAI{APIKey: failureAPIKey, BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond})
		requireNoFailureSecret(t, "job.failed", raw)
		requireNoFailureSecret(t, "transcript", transcript)
		text, _ := payload["error"].(string)
		if !strings.Contains(text, "response status") || !strings.Contains(text, "[REDACTED:pattern]") || !strings.Contains(text, "[REDACTED:exact]") {
			t.Fatalf("error lost its shape: %d bytes", len(text))
		}
		diagnostic := failureDiagnostic(t, payload)
		if diagnostic["upstream_request_id"] != "[REDACTED:pattern]" || diagnostic["provider_code"] != "[REDACTED:pattern]" {
			t.Fatalf("diagnostic=%v", diagnostic)
		}
		if record.RedactionStats.Pattern < 3 || record.RedactionStats.Exact < 1 {
			t.Fatalf("removals not counted: %+v", record.RedactionStats)
		}
	})
	t.Run("400 x-request-id", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-request-id", failureSessionToken)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, failureGitHubToken, "bad key "+failurePatToken)
		}))
		defer server.Close()
		payload, raw, transcript, _ := storedFailure(t, "failure-400", &inprocess.OpenAI{APIKey: failureAPIKey, BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond})
		requireNoFailureSecret(t, "job.failed", raw)
		requireNoFailureSecret(t, "transcript", transcript)
		text, _ := payload["error"].(string)
		for _, want := range []string{"400 Bad Request", "request_id=[REDACTED:pattern]", "provider_code=[REDACTED:pattern]", "provider_message=bad key [REDACTED:pattern]"} {
			if !strings.Contains(text, want) {
				t.Fatalf("error lacks %q", want)
			}
		}
		diagnostic := failureDiagnostic(t, payload)
		if diagnostic["upstream_request_id"] != "[REDACTED:pattern]" || diagnostic["provider_code"] != "[REDACTED:pattern]" {
			t.Fatalf("diagnostic=%v", diagnostic)
		}
	})
	t.Run("transport error with a credential in the base URL", func(t *testing.T) {
		t.Parallel()
		client := &inprocess.OpenAI{
			APIKey:     failureAPIKey,
			BaseURL:    "https://fixture-user:" + failurePassword + "@provider.example/" + failurePathValue + "/v1?api_key=" + failureQueryValue,
			RetryDelay: time.Millisecond,
			Client: &http.Client{Transport: failureRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			})},
		}
		payload, raw, transcript, _ := storedFailure(t, "failure-transport", client)
		requireNoFailureSecret(t, "job.failed", raw)
		requireNoFailureSecret(t, "transcript", transcript)
		text, _ := payload["error"].(string)
		if !strings.Contains(text, "provider.example") || !strings.Contains(text, "connection refused") || strings.Contains(text, "/v1") {
			t.Fatalf("error does not name only the host: %d bytes", len(text))
		}
		if diagnostic := failureDiagnostic(t, payload); diagnostic["endpoint"] != "provider.example" {
			t.Fatalf("diagnostic=%v", diagnostic)
		}
	})
}

// TestSanitisedProviderErrorKeepsClassification proves errors.As still finds a
// triage budget overage and errors.Is a cancellation when the sanitiser has
// changed the error's text, which is the only time it wraps.
func TestSanitisedProviderErrorKeepsClassification(t *testing.T) {
	t.Parallel()
	t.Run("errors.As triageBudgetError", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"text":"transcribed audio"}`)
		}))
		defer server.Close()
		overage := &triageBudgetError{Budget: "text/history", Usage: 2, Limit: 1, AttachmentID: "audio-id", AttachmentName: "clip.mp3"}
		client := &inprocess.OpenAI{APIKey: failureAPIKey, BaseURL: server.URL, Client: server.Client()}
		_, err := client.Run(context.Background(), "model-a", inprocess.Input{
			Prompt:      "work",
			Attachments: []inprocess.Attachment{{ID: "audio-id", Name: "clip.mp3", ContentType: "audio/mpeg", Kind: inprocess.AttachmentAudio, Content: []byte("audio")}},
			CheckPreparedText: func(inprocess.Attachment, int) error {
				return fmt.Errorf("prepared text held %s: %w", failurePatToken, overage)
			},
		})
		if err == nil || strings.Contains(err.Error(), failurePatToken) || !strings.Contains(err.Error(), "[REDACTED:pattern]") {
			t.Fatalf("the sanitiser did not rewrite the error: present=%t", err != nil)
		}
		var found *triageBudgetError
		if !errors.As(err, &found) || found != overage {
			t.Fatal("errors.As(*triageBudgetError) lost through the sanitised error")
		}
	})
	t.Run("errors.Is context.Canceled", func(t *testing.T) {
		t.Parallel()
		client := &inprocess.OpenAI{
			APIKey:     failureAPIKey,
			BaseURL:    "https://provider.example/" + failurePathValue + "/v1",
			RetryDelay: time.Millisecond,
			Client: &http.Client{Transport: failureRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("stopped holding %s: %w", failurePatToken, context.Canceled)
			})},
		}
		_, err := client.Run(context.Background(), "model-a", inprocess.Input{Prompt: "work"})
		if err == nil || strings.Contains(err.Error(), failurePatToken) || strings.Contains(err.Error(), failurePathValue) {
			t.Fatalf("the sanitiser did not rewrite the error: present=%t", err != nil)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("errors.Is(context.Canceled) lost through the sanitised error")
		}
	})
}

// TestInProcessStagePromptKeepsTaskBodyExact proves the task body reaches an
// in-process stage as submitted when it holds a token
// (req-intake-and-triage AC-1.2; req-security-boundaries REQ-3).
func TestInProcessStagePromptKeepsTaskBodyExact(t *testing.T) {
	t.Parallel()
	ctx := store.WithActor(context.Background(), store.SystemActor())
	st := store.NewMemory()
	body := "Rotate this leaked value: " + failurePatToken + "\n\n- keep **markdown**  spacing\n"
	task := core.Task{ID: "exact-stage-body", Workspace: "demo", Repo: "api", Title: "Rotate", Body: body, Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "model-a", Timeout: time.Minute}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agent.input.Prompt, body) {
		t.Fatal("the in-process stage prompt does not hold the body as submitted")
	}
}

// TestProviderMessageRemovalRecordedOnTranscript proves the stage error's copy
// of the provider message reaches the transcript record's counts. The two
// responses hold the same secret: only the first holds it in error.message, the
// text the stage error copies, so its record counts one removal more and the
// count is what differs (req-security-boundaries AC-3.1; component-triage).
func TestProviderMessageRemovalRecordedOnTranscript(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, id, field string) (string, core.RedactionStats) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{%q:%q}}`, field, "bad key "+failurePatToken)
		}))
		defer server.Close()
		_, raw, transcript, record := storedFailure(t, id, &inprocess.OpenAI{APIKey: failureAPIKey, BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond})
		requireNoFailureSecret(t, "job.failed", raw)
		requireNoFailureSecret(t, "transcript", transcript)
		return raw, record.RedactionStats
	}
	messageEvent, withMessage := run(t, "failure-message-count", "message")
	if !strings.Contains(messageEvent, "provider_message=bad key [REDACTED:pattern]") {
		t.Fatal("the stage error did not copy the redacted provider message")
	}
	controlEvent, control := run(t, "failure-message-control", "detail")
	if strings.Contains(controlEvent, "provider_message=") {
		t.Fatal("control: the stage error copied a provider message")
	}
	if want := (core.RedactionStats{Pattern: control.Pattern + 1, Exact: control.Exact, Encoded: control.Encoded, Entropy: control.Entropy}); withMessage != want {
		t.Fatalf("transcript counts = %+v, want %+v", withMessage, want)
	}
}
