package inprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	sanitiseAPIKey     = "fixture-provider-api-key-value"
	sanitiseStoredKey  = "fixture-stored-app-key-value"
	sanitiseCredential = "tenant-path-credential-fixture"
	sanitiseQuery      = "query-credential-fixture"
	sanitisePassword   = "userinfo-password-fixture"
	sanitiseToken      = "cv_pat_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6kM9nP2r"
	sanitiseBaseURL    = "https://fixture-user:" + sanitisePassword + "@provider.example/" + sanitiseCredential + "/v1?api_key=" + sanitiseQuery
)

var sanitiseValues = []string{sanitiseAPIKey, sanitiseStoredKey, sanitiseCredential, sanitiseQuery, sanitisePassword, sanitiseToken}

type sanitiseRoundTripper func(*http.Request) (*http.Response, error)

func (f sanitiseRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ctxSecretSource fails the way a store read does once its context has ended.
type ctxSecretSource struct{ values []string }

func (s ctxSecretSource) ListGitHubAppKeysForRedaction(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.values, nil
}

func sanitiseClient(transport sanitiseRoundTripper) *OpenAI {
	return &OpenAI{APIKey: sanitiseAPIKey, BaseURL: sanitiseBaseURL, Client: &http.Client{Transport: transport}, RedactionSecrets: documentSecretSource{sanitiseStoredKey}, RetryDelay: time.Millisecond}
}

func requireNoSanitiseValues(t *testing.T, name, text string) {
	t.Helper()
	for _, value := range sanitiseValues {
		if strings.Contains(text, value) {
			t.Fatalf("%s holds a fixture secret of %d bytes", name, len(value))
		}
	}
}

func TestRunTransportFailureNamesOnlyTheHostAndKeepsItsCause(t *testing.T) {
	t.Parallel()
	calls := 0
	client := sanitiseClient(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("proxy said %s: %w", sanitiseToken, context.Canceled)
	})
	result, err := client.Run(context.Background(), "model-a", Input{Prompt: "work"})
	if err == nil || calls != responsesMaxAttempts {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	requireNoSanitiseValues(t, "error", err.Error())
	requireNoSanitiseValues(t, "transcript", string(result.Transcript))
	if !strings.Contains(err.Error(), "provider.example") || strings.Contains(err.Error(), "/v1") || !strings.Contains(err.Error(), "[REDACTED:pattern]") {
		t.Fatalf("error text does not name only the host: %d bytes", len(err.Error()))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("errors.Is(context.Canceled) lost through the sanitised error")
	}
	if result.Diagnostic == nil || result.Diagnostic.Endpoint != "provider.example" || result.Diagnostic.Phase != "retry_exhausted" {
		t.Fatalf("diagnostic=%+v", result.Diagnostic)
	}
	if result.Redactions.Pattern < 1 {
		t.Fatalf("redactions=%+v", result.Redactions)
	}
}

func TestRunTranscriptionTransportFailureNamesOnlyTheHost(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := sanitiseClient(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	result, err := client.Run(ctx, "model-a", Input{Prompt: "work", Attachments: []Attachment{{ID: "audio-id", Name: "clip.mp3", ContentType: "audio/mpeg", Kind: AttachmentAudio, Content: []byte("audio")}}})
	if err == nil {
		t.Fatal("expected a transcription failure")
	}
	requireNoSanitiseValues(t, "error", err.Error())
	requireNoSanitiseValues(t, "transcript", string(result.Transcript))
	if !strings.Contains(err.Error(), "transcribe artifact audio-id") || !strings.Contains(err.Error(), "provider.example") || len(result.Transcript) == 0 {
		t.Fatalf("the failure lost its detail: %d-byte error, %d-byte transcript", len(err.Error()), len(result.Transcript))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("errors.Is(context.Canceled) lost through the sanitised transcription error")
	}
}

// TestRunInvalidEndpointKeepsTheURLOutOfTheError covers the request-building
// failure, which prints the whole URL before any call.
func TestRunInvalidEndpointKeepsTheURLOutOfTheError(t *testing.T) {
	t.Parallel()
	client := sanitiseClient(func(*http.Request) (*http.Response, error) {
		t.Error("no request can be built")
		return nil, errors.New("unexpected")
	})
	client.BaseURL = "https://provider.example/" + sanitiseCredential + "\x7f/v1"
	result, err := client.Run(context.Background(), "model-a", Input{Prompt: "work"})
	if err == nil {
		t.Fatal("expected a request-building failure")
	}
	requireNoSanitiseValues(t, "error", err.Error())
	requireNoSanitiseValues(t, "transcript", string(result.Transcript))
}

func TestRunRedactsEveryProviderFieldOnSuccess(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", sanitiseToken)
		_, _ = io.WriteString(w, `{"model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: sanitiseAPIKey, BaseURL: server.URL, Client: server.Client()}
	result, err := client.Run(context.Background(), "model-a", Input{Prompt: "work"})
	if err != nil || result.Output != "done" {
		t.Fatalf("output=%q err=%v", result.Output, err)
	}
	if result.Diagnostic == nil || result.Diagnostic.UpstreamRequest != "[REDACTED:pattern]" {
		t.Fatalf("diagnostic=%+v", result.Diagnostic)
	}
}

// TestRunWithholdsDetailWhenSecretsCannotBeRead proves the failure path never
// returns unredacted provider text, and that classification still works.
func TestRunWithholdsDetailWhenSecretsCannotBeRead(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", sanitiseToken)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, sanitiseToken, sanitiseAPIKey)
	}))
	defer server.Close()
	client := &OpenAI{APIKey: sanitiseAPIKey, BaseURL: server.URL, Client: server.Client(), RedactionSecrets: failingDocumentSecretSource{}}
	result, err := client.Run(context.Background(), "model-a", Input{Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "detail withheld because redaction is unavailable") {
		t.Fatalf("err=%v", err)
	}
	requireNoSanitiseValues(t, "error", err.Error())
	if errors.Unwrap(err) == nil {
		t.Fatal("the withheld error lost its cause")
	}
	if result.Diagnostic == nil || result.Diagnostic.ProviderCode != "" || result.Diagnostic.UpstreamRequest != "" {
		t.Fatalf("diagnostic keeps provider text: %+v", result.Diagnostic)
	}
	if len(result.Transcript) != 0 {
		t.Fatal("no transcript can be stored without redaction")
	}
}

// TestRunRedactsAfterTheStageContextEnds proves a cancelled stage still gets a
// secret read, so its error keeps detail and its stored key is removed.
func TestRunRedactsAfterTheStageContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	client := &OpenAI{APIKey: sanitiseAPIKey, RedactionSecrets: ctxSecretSource{values: []string{sanitiseStoredKey}}}
	cancel()
	failure := fmt.Errorf("stage stopped holding %s: %w", sanitiseStoredKey, context.Canceled)
	_, err := client.sanitiseResult(ctx, "model-a", Result{}, failure)
	if err == nil || strings.Contains(err.Error(), "withheld") || !strings.Contains(err.Error(), "[REDACTED:exact]") {
		t.Fatalf("err=%v", err)
	}
	requireNoSanitiseValues(t, "error", err.Error())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("errors.Is(context.Canceled) lost")
	}
}

// TestRunKeepsAnUnchangedErrorIdentical leaves an error with nothing to remove
// exactly as it was.
func TestRunKeepsAnUnchangedErrorIdentical(t *testing.T) {
	t.Parallel()
	failure := errors.New("plain failure")
	_, err := (&OpenAI{APIKey: sanitiseAPIKey}).sanitiseResult(context.Background(), "model-a", Result{}, failure)
	if err != failure {
		t.Fatalf("err=%v", err)
	}
}

// TestRunAudioPathKeepsSecretsOutOfTheAudit covers the transcript prompt site:
// the transcript reaches the provider inside its untrusted block and the audit
// records only its size, while the attachment name, which the prompt heading
// quotes, is still redacted when quoting escapes a known value
// (req-security-boundaries REQ-3; component-triage).
func TestRunAudioPathKeepsSecretsOutOfTheAudit(t *testing.T) {
	t.Parallel()
	const escapedStored = `fixture"stored\key-value`
	const spoken = "spoken " + sanitiseToken + " and " + sanitiseAPIKey
	attachment := Attachment{ID: "audio-id", Name: "clip " + sanitiseToken + " " + escapedStored + ".mp3", ContentType: "audio/mpeg", Kind: AttachmentAudio, Content: []byte("audio")}
	// "stored" and "key-value" find the known value however many layers of Go
	// and JSON quoting surround its escapes.
	forms := []string{sanitiseToken, sanitiseAPIKey, escapedStored, `fixture\"stored`, "stored", "key-value", "spoken "}
	requireClean := func(t *testing.T, name, text string) {
		t.Helper()
		for _, form := range forms {
			if strings.Contains(text, form) {
				t.Fatalf("%s holds a fixture value of %d bytes", name, len(form))
			}
		}
	}
	for _, tc := range []struct {
		name     string
		status   int
		response string
	}{
		{name: "success", status: http.StatusOK, response: `{"model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`},
		{name: "provider failure echoing the name", status: http.StatusBadRequest, response: fmt.Sprintf(`{"error":{"code":"bad_input","message":%s}}`, strconv.Quote("rejected "+sanitiseToken+" in "+escapedStored))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sent atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/audio/transcriptions" {
					_, _ = fmt.Fprintf(w, `{"text":%s}`, strconv.Quote(spoken))
					return
				}
				body, _ := io.ReadAll(r.Body)
				sent.Store(string(body))
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			client := &OpenAI{APIKey: sanitiseAPIKey, BaseURL: server.URL, Client: server.Client(), RedactionSecrets: documentSecretSource{escapedStored}}
			result, err := client.Run(context.Background(), "model-a", Input{Prompt: "work", Attachments: []Attachment{attachment}})
			if request, _ := sent.Load().(string); !strings.Contains(request, sanitiseToken) || !strings.Contains(request, "spoken ") {
				t.Fatal("the provider did not receive the transcript")
			}
			requireClean(t, "transcript", string(result.Transcript))
			if !strings.Contains(string(result.Transcript), fmt.Sprintf("[transcript supplied to model; %d characters]", len(spoken))) {
				t.Fatalf("the audit lost the transcript's size: %d bytes", len(result.Transcript))
			}
			if result.Redactions.Pattern < 1 || result.Redactions.Encoded < 1 {
				t.Fatalf("removals not counted: %+v", result.Redactions)
			}
			if tc.status == http.StatusOK {
				if err != nil {
					t.Fatalf("run failed: %d-byte error", len(err.Error()))
				}
				return
			}
			if err == nil {
				t.Fatal("expected a provider failure")
			}
			requireClean(t, "error", err.Error())
		})
	}
}
