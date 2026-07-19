package inprocess

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
			_, _ = io.WriteString(w, `{"model":"gpt-5.6-luna","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	input := Input{Prompt: "analyze", Attachments: []Attachment{
		{ID: "image-id", Name: "image.png", ContentType: "image/png", Kind: AttachmentImage, Content: []byte("image-bytes")},
		{ID: "pdf-id", Name: "file.pdf", ContentType: "application/pdf", Kind: AttachmentDocument, Content: []byte("pdf-bytes")},
		{ID: "audio-id", Name: "clip.mp3", ContentType: "audio/mpeg", Kind: AttachmentAudio, Content: []byte("audio-bytes")},
	}}
	result, err := (&OpenAI{APIKey: key, BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-luna", input)
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
	for _, encodedBytes := range []string{base64.StdEncoding.EncodeToString([]byte("image-bytes")), base64.StdEncoding.EncodeToString([]byte("pdf-bytes"))} {
		if !strings.Contains(requestText, encodedBytes) || strings.Contains(string(result.Transcript), encodedBytes) {
			t.Fatalf("binary request/transcript contract failed for %q: request=%s transcript=%s", encodedBytes, requestText, result.Transcript)
		}
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

func TestOpenAIRunStopsAfterBoundedRetries(t *testing.T) {
	t.Parallel()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: "sk-test", BaseURL: server.URL, Client: server.Client(), RetryDelay: time.Millisecond}
	_, err := client.Run(context.Background(), "gpt-5.6-luna", Input{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	if attempts != responsesMaxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, responsesMaxAttempts)
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
