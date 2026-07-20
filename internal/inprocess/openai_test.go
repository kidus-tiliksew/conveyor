package inprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOpenAIRunUsesStructuredBinaryInputsAndTranscribesAudio(t *testing.T) {
	t.Parallel()
	const key = "sk-test"
	var responseRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/audio/transcriptions":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			content, err := io.ReadAll(file)
			if err != nil || string(content) != "audio-bytes" || r.FormValue("model") != "gpt-4o-mini-transcribe" {
				t.Fatalf("audio=%q model=%q err=%v", content, r.FormValue("model"), err)
			}
			_, _ = io.WriteString(w, `{"text":"spoken context"}`)
		case "/responses":
			if err := json.NewDecoder(r.Body).Decode(&responseRequest); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, `{"model":"gpt-5.6-terra","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	input := Input{Prompt: "analyze", Attachments: []Attachment{
		{ID: "image-id", Name: "image.png", ContentType: "image/png", Kind: AttachmentImage, Content: append([]byte("\x89PNG\r\n\x1a\n"), []byte("image-bytes")...)},
		{ID: "pdf-id", Name: "file.pdf", ContentType: "application/pdf", Kind: AttachmentDocument, Content: []byte("pdf-bytes")},
		{ID: "audio-id", Name: "clip.mp3", ContentType: "audio/mpeg", Kind: AttachmentAudio, Content: []byte("audio-bytes")},
	}}
	result, err := (&OpenAI{APIKey: key, BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-terra", input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(responseRequest)
	requestText := string(encoded)
	for _, expected := range []string{`"type":"input_image"`, `"type":"input_file"`, "spoken context"} {
		if !strings.Contains(requestText, expected) {
			t.Fatalf("request missing %q: %s", expected, requestText)
		}
	}
	for _, encodedBytes := range []string{base64.StdEncoding.EncodeToString(input.Attachments[0].Content), base64.StdEncoding.EncodeToString([]byte("pdf-bytes"))} {
		if !strings.Contains(requestText, encodedBytes) || strings.Contains(string(result.Transcript), encodedBytes) {
			t.Fatalf("binary request/transcript contract failed for %q: request=%s transcript=%s", encodedBytes, requestText, result.Transcript)
		}
	}
	serverURL, _ := url.Parse(server.URL)
	if result.Diagnostic == nil || result.Diagnostic.Model != "gpt-5.6-terra" || result.Diagnostic.AttachmentCount != 3 || result.Diagnostic.Endpoint != serverURL.Hostname() {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

func TestOpenAIRunDefaultEndpointIsNamedWithoutRequest(t *testing.T) {
	t.Parallel()
	result, err := (&OpenAI{}).Run(context.Background(), "gpt-5.6-terra", Input{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "Responses API (api.openai.com) client validation failed") {
		t.Fatalf("err = %v", err)
	}
	if result.Diagnostic == nil || result.Diagnostic.Endpoint != "api.openai.com" || result.Diagnostic.Provider != "openai_responses" {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

func TestOpenAIRunConditionallySendsReasoningEffort(t *testing.T) {
	for _, test := range []struct {
		name, effort  string
		wantReasoning bool
	}{
		{name: "configured", effort: "low", wantReasoning: true},
		{name: "provider default", effort: "", wantReasoning: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(w, `{"model":"gpt-test","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()

			result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-test", Input{Prompt: "work", Effort: test.effort})
			if err != nil {
				t.Fatal(err)
			}
			reasoning, present := request["reasoning"]
			if present != test.wantReasoning {
				t.Fatalf("reasoning present=%t request=%+v", present, request)
			}
			if test.wantReasoning {
				value, ok := reasoning.(map[string]any)
				if !ok || value["effort"] != test.effort || !strings.Contains(string(result.Transcript), `"reasoning":{"effort":"low"}`) {
					t.Fatalf("reasoning=%+v transcript=%s", reasoning, result.Transcript)
				}
			} else if strings.Contains(string(result.Transcript), `"reasoning"`) {
				t.Fatalf("unset effort emitted reasoning: %s", result.Transcript)
			}
		})
	}
}

func TestResponseEndpointHostExcludesURLMetadata(t *testing.T) {
	t.Parallel()
	const endpoint = "https://user:password@openrouter.ai:8443/api/v1?trace=yes#fragment"
	if got := responseEndpointHost(endpoint); got != "openrouter.ai" {
		t.Fatalf("responseEndpointHost(%q) = %q", endpoint, got)
	}
}

func TestOpenAIRunRejectsUnsupportedOrMalformedImagesBeforeProviderSubmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, model string
		content     []byte
		want        string
		phase       string
	}{
		{name: "unsupported model", model: "text-only-test", content: append([]byte("\x89PNG\r\n\x1a\n"), 'x'), want: "capability validation", phase: "capability_validation"},
		{name: "malformed image", model: "gpt-5.6-terra", content: []byte("not-png"), want: "does not match", phase: "attachment_validation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			defer server.Close()
			serverURL, _ := url.Parse(server.URL)
			hostname := serverURL.Hostname()
			result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), test.model, Input{
				Prompt: "analyze", Attachments: []Attachment{{ID: "image-id", Name: "image.png", ContentType: "image/png", Kind: AttachmentImage, Content: test.content}},
			})
			if err == nil || !strings.Contains(err.Error(), "Responses API ("+hostname+")") || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), server.URL) || calls != 0 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if result.Diagnostic == nil || result.Diagnostic.Phase != test.phase || result.Diagnostic.Provider != "openai_responses" || result.Diagnostic.Endpoint != hostname || len(result.Transcript) == 0 {
				t.Fatalf("result = %+v", result)
			}
			if strings.Contains(string(result.Transcript), server.URL) {
				t.Fatalf("transcript contains full endpoint URL: %s", result.Transcript)
			}
			if strings.Contains(string(result.Transcript), string(test.content)) {
				t.Fatalf("transcript contains binary input: %s", result.Transcript)
			}
		})
	}
}

func TestOpenAIRunRejectsAnimatedGIFBeforeProviderSubmission(t *testing.T) {
	t.Parallel()
	palette := color.Palette{color.Black, color.White}
	frames := []*image.Paletted{
		image.NewPaletted(image.Rect(0, 0, 1, 1), palette),
		image.NewPaletted(image.Rect(0, 0, 1, 1), palette),
	}
	frames[1].Pix[0] = 1
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: frames, Delay: []int{0, 0}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-terra", Input{
		Prompt: "analyze", Attachments: []Attachment{{ID: "animated", Name: "animated.gif", ContentType: "image/gif", Kind: AttachmentImage, Content: encoded.Bytes()}},
	})
	if err == nil || !strings.Contains(err.Error(), "animated GIF") || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
	if result.Diagnostic == nil || result.Diagnostic.Phase != "attachment_validation" || len(result.Transcript) == 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestOpenAIRunMetersUsageAndRedactsTranscript(t *testing.T) {
	t.Parallel()
	const key = "sk-test-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Fatalf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"gpt-newly-configured","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":17,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}},"debug":"%s"}`, key)
	}))
	defer server.Close()

	result, err := (&OpenAI{APIKey: key, BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-newly-configured", Input{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "done" || result.TokensIn != 17 || result.TokensOut != 3 {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(string(result.Transcript), key) || result.Redactions.Total() == 0 {
		t.Fatalf("unredacted transcript: %s", result.Transcript)
	}
	if !strings.Contains(string(result.Transcript), `"text":"work"`) {
		t.Fatalf("request missing from transcript: %s", result.Transcript)
	}
}

func TestOpenAIRunUsesOnlyFinalMessageOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		output   string
		want     string
		preamble string
	}{
		{
			name:     "earlier message is progress narration",
			output:   `[{"type":"message","content":[{"type":"output_text","text":"I will inspect..."}]},{"type":"message","content":[{"type":"output_text","text":"# Title\n\n## Intent..."}]}]`,
			want:     "# Title\n\n## Intent...",
			preamble: "I will inspect...",
		},
		{
			name:   "all text parts in final message are joined",
			output: `[{"type":"message","content":[{"type":"output_text","text":"first "},{"type":"tool_call","text":"ignored"},{"type":"output_text","text":"second"}]}]`,
			want:   "first second",
		},
		{
			name:   "single message remains unchanged",
			output: `[{"type":"message","content":[{"type":"output_text","text":"opening prose without a heading"}]}]`,
			want:   "opening prose without a heading",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"model":"gpt-5.6-terra","output":%s,"usage":{"input_tokens":1,"output_tokens":1}}`, test.output)
			}))
			defer server.Close()

			result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-terra", Input{Prompt: "work"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Output != test.want {
				t.Fatalf("output = %q, want %q", result.Output, test.want)
			}
			if test.preamble != "" && !strings.Contains(string(result.Transcript), test.preamble) {
				t.Fatalf("transcript lost earlier message: %s", result.Transcript)
			}
		})
	}
}

func TestOpenAIRunRetriesTransientUpstreamFailures(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"model":"gpt-5.6-luna","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || result.Output != "done" {
		t.Fatalf("attempts=%d output=%q", attempts, result.Output)
	}
}

func TestOpenAIRunRetriesRateLimits(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"model":"gpt-5.6-terra","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}).Run(context.Background(), "gpt-5.6-terra", Input{Prompt: "work"})
	if err != nil || attempts != 3 || result.Output != "done" {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
}

func TestOpenAIRunRetriesFailuresEmbeddedIn200Envelopes(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			// OpenRouter delivers upstream faults as HTTP 200 with a failed
			// body: reasoning-only output, no message, embedded error object.
			_, _ = io.WriteString(w, `{"status":"failed","error":{"code":"server_error","message":"An error occurred while processing your request."},"output":[{"type":"reasoning"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"x-ai/grok-4.5","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "x-ai/grok-4.5", Input{Prompt: "work"})
	if err != nil || attempts != 3 || result.Output != "done" {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
}

func TestOpenAIRunSurfacesNonRetryableEmbeddedFailureWithoutRetry(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = io.WriteString(w, `{"status":"failed","error":{"code":"invalid_prompt","message":"prompt rejected"},"output":[]}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if !strings.Contains(err.Error(), "provider_code=invalid_prompt") || !strings.Contains(err.Error(), "prompt rejected") || strings.Contains(err.Error(), "no output_text") {
		t.Fatalf("err = %v", err)
	}
	if result.Diagnostic == nil || result.Diagnostic.Phase != "provider_response" || result.Diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

func TestOpenAIRunReportsIncompleteResponsesWithReason(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = io.WriteString(w, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning"}]}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || attempts != 1 || !strings.Contains(err.Error(), "response incomplete (max_output_tokens)") {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if result.Diagnostic == nil || result.Diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

func TestOpenAIRunExhaustsRetriesOnPersistentEmbeddedFailures(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = io.WriteString(w, `{"status":"failed","error":{"code":"server_error","message":"still broken"},"output":[]}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || attempts != responsesMaxAttempts {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if !strings.Contains(err.Error(), "retry_exhausted") || !strings.Contains(err.Error(), "provider_code=server_error") {
		t.Fatalf("err = %v", err)
	}
	if result.Diagnostic == nil || result.Diagnostic.Phase != "retry_exhausted" || !result.Diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestOpenAIRunBoundsTransportRetriesAndPersistsDiagnostic(t *testing.T) {
	t.Parallel()
	attempts := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, fmt.Errorf("connection reset")
	})}
	result, err := (&OpenAI{APIKey: "sk-test", Client: httpClient, RetryDelay: time.Millisecond}).Run(context.Background(), "gpt-5.6-terra", Input{Prompt: "work"})
	if err == nil || attempts != responsesMaxAttempts || result.Diagnostic == nil || result.Diagnostic.Phase != "retry_exhausted" || result.Diagnostic.Attempts != responsesMaxAttempts || result.Diagnostic.Endpoint != "api.openai.com" || !result.Diagnostic.Retryable {
		t.Fatalf("attempts=%d result=%+v err=%v", attempts, result, err)
	}
	if !strings.Contains(err.Error(), "Responses API (api.openai.com)") || !strings.Contains(string(result.Transcript), "connection reset") || !strings.Contains(string(result.Transcript), `"endpoint":"api.openai.com"`) || strings.Contains(string(result.Transcript), "sk-test") {
		t.Fatalf("unsafe or incomplete transcript: %s", result.Transcript)
	}
}

func TestOpenAIRunStopsAfterBoundedRetries(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("x-request-id", fmt.Sprintf("req-safe-%d", attempts))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"server_error","message":"temporary"}}`)
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	hostname := serverURL.Hostname()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	result, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "Responses API ("+hostname+") retry_exhausted") || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("err = %v", err)
	}
	if attempts != responsesMaxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, responsesMaxAttempts)
	}
	wantRequestID := fmt.Sprintf("req-safe-%d", responsesMaxAttempts)
	if result.Diagnostic == nil || result.Diagnostic.Phase != "retry_exhausted" || result.Diagnostic.Endpoint != hostname || result.Diagnostic.Provider != "openai_responses" || result.Diagnostic.Attempts != responsesMaxAttempts || result.Diagnostic.UpstreamRequest != wantRequestID || result.Diagnostic.ProviderCode != "server_error" || !result.Diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
	for _, expected := range []string{wantRequestID, "server_error", "retry_exhausted", `"endpoint":"` + hostname + `"`} {
		if !strings.Contains(string(result.Transcript), expected) {
			t.Fatalf("transcript missing %q: %s", expected, result.Transcript)
		}
	}
	if strings.Contains(string(result.Transcript), server.URL) {
		t.Fatalf("transcript contains full endpoint URL: %s", result.Transcript)
	}
}

func TestOpenAIRunTranscriptionFailureNamesEndpointHost(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	hostname := serverURL.Hostname()
	result, err := (&OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-terra", Input{
		Prompt:      "work",
		Attachments: []Attachment{{ID: "audio-id", Name: "clip.mp3", ContentType: "audio/mpeg", Kind: AttachmentAudio, Content: []byte("audio")}},
	})
	if err == nil || !strings.Contains(err.Error(), "Transcription API ("+hostname+") returned 401 Unauthorized") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("err = %v", err)
	}
	if result.Diagnostic == nil || result.Diagnostic.Endpoint != hostname || result.Diagnostic.Provider != "openai_responses" {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
}

func TestOpenAIRunDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	_, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
