// Package inprocess executes the small always-on pipeline stages directly in
// conveyord. Operator-owned coding agents remain outside this package.
package inprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/gif"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

type Result struct {
	Output     string
	Model      string
	TokensIn   int64
	TokensOut  int64
	Transcript []byte
	Redactions core.RedactionStats
	Diagnostic *Diagnostic
}

type Diagnostic struct {
	Phase           string   `json:"phase"`
	Provider        string   `json:"provider"`
	Endpoint        string   `json:"endpoint"`
	Model           string   `json:"model"`
	AttachmentCount int      `json:"attachment_count"`
	AttachmentTypes []string `json:"attachment_types,omitempty"`
	Attempts        int      `json:"attempts,omitempty"`
	HTTPStatus      int      `json:"http_status,omitempty"`
	ProviderCode    string   `json:"provider_code,omitempty"`
	UpstreamRequest string   `json:"upstream_request_id,omitempty"`
	Retryable       bool     `json:"retryable"`
}

type AttachmentKind string

const (
	AttachmentImage    AttachmentKind = "image"
	AttachmentDocument AttachmentKind = "document"
	AttachmentAudio    AttachmentKind = "audio"
)

type Attachment struct {
	ID          string
	Name        string
	ContentType string
	Kind        AttachmentKind
	Content     []byte
}

type Input struct {
	Prompt      string
	Effort      string
	Attachments []Attachment
}

type Agent interface {
	Run(context.Context, string, Input) (Result, error)
}

type OpenAI struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
	// RetryDelay overrides the transient-failure backoff base; zero uses the
	// default. Tests set it to keep retries fast.
	RetryDelay time.Duration
}

const (
	responsesMaxAttempts = 6
	responsesRetryDelay  = 2 * time.Second
)

func (client *OpenAI) Run(ctx context.Context, model string, input Input) (Result, error) {
	endpoint := strings.TrimRight(client.BaseURL, "/")
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	endpointHost := responseEndpointHost(endpoint)
	diagnostic := requestDiagnostic(model, endpointHost, input.Attachments)
	if strings.TrimSpace(client.APIKey) == "" {
		diagnostic.Phase = "client_validation"
		return Result{Diagnostic: &diagnostic}, fmt.Errorf("Responses API (%s) client validation failed for model %q: CONVEYOR_API_KEY is required for in-process stages", endpointHost, model)
	}
	if phase, err := validateImageInputs(model, endpointHost, input.Attachments); err != nil {
		diagnostic.Phase = phase
		requestValue := map[string]any{"model": model, "attachment_summary": diagnostic, "store": false}
		transcript, stats, redactErr := client.auditEnvelope(requestValue, map[string]any{"diagnostic": diagnostic})
		if redactErr != nil {
			return Result{Diagnostic: &diagnostic}, redactErr
		}
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, err
	}
	content := []map[string]any{{"type": "input_text", "text": input.Prompt}}
	auditContent := []map[string]any{{"type": "input_text", "text": input.Prompt}}
	for _, attachment := range input.Attachments {
		dataURL := "data:" + attachment.ContentType + ";base64," + base64.StdEncoding.EncodeToString(attachment.Content)
		metadata := fmt.Sprintf("[binary omitted: artifact %s, %s, %d bytes]", attachment.ID, attachment.ContentType, len(attachment.Content))
		switch attachment.Kind {
		case AttachmentImage:
			content = append(content, map[string]any{"type": "input_image", "image_url": dataURL, "detail": "auto"})
			auditContent = append(auditContent, map[string]any{"type": "input_image", "image_url": metadata})
		case AttachmentDocument:
			content = append(content, map[string]any{"type": "input_file", "filename": attachment.Name, "file_data": dataURL})
			auditContent = append(auditContent, map[string]any{"type": "input_file", "filename": attachment.Name, "file_data": metadata})
		case AttachmentAudio:
			transcript, err := client.transcribe(ctx, endpoint, attachment)
			if err != nil {
				diagnostic.Phase = "attachment_preparation"
				failure := fmt.Errorf("transcribe artifact %s (%s): %w", attachment.ID, attachment.Name, err)
				audit, stats, redactErr := client.auditEnvelope(map[string]any{"model": model, "input": auditContent, "store": false}, map[string]any{"diagnostic": diagnostic, "error": failure.Error()})
				if redactErr != nil {
					return Result{Diagnostic: &diagnostic}, redactErr
				}
				return Result{Transcript: audit, Redactions: stats, Diagnostic: &diagnostic}, failure
			}
			text := fmt.Sprintf("# Audio attachment: %s (artifact %s)\n\n%s", attachment.Name, attachment.ID, transcript)
			content = append(content, map[string]any{"type": "input_text", "text": text})
			auditContent = append(auditContent, map[string]any{"type": "input_text", "text": fmt.Sprintf("# Audio attachment: %s (artifact %s)\n\n[transcript supplied to model; %d characters]", attachment.Name, attachment.ID, len(transcript))})
		default:
			diagnostic.Phase = "attachment_preparation"
			failure := fmt.Errorf("unsupported attachment kind %q for artifact %s", attachment.Kind, attachment.ID)
			audit, stats, redactErr := client.auditEnvelope(map[string]any{"model": model, "input": auditContent, "store": false}, map[string]any{"diagnostic": diagnostic, "error": failure.Error()})
			if redactErr != nil {
				return Result{Diagnostic: &diagnostic}, redactErr
			}
			return Result{Transcript: audit, Redactions: stats, Diagnostic: &diagnostic}, failure
		}
	}
	requestInput := []map[string]any{{"role": "user", "content": content}}
	auditInput := []map[string]any{{"role": "user", "content": auditContent}}
	requestValue := responsesRequestEnvelope(model, requestInput, input.Effort)
	auditRequestValue := responsesRequestEnvelope(model, auditInput, input.Effort)
	body, _ := json.Marshal(requestValue)
	httpClient := client.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Hour}
	}
	attempt := func() ([]byte, int, string, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, 0, "", "", err
		}
		req.Header.Set("Authorization", "Bearer "+client.APIKey)
		req.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(req)
		if err != nil {
			return nil, 0, "", "", err
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
		if err != nil {
			return nil, 0, "", response.Header.Get("x-request-id"), err
		}
		requestID := response.Header.Get("x-request-id")
		if requestID == "" {
			requestID = response.Header.Get("request-id")
		}
		return raw, response.StatusCode, response.Status, requestID, nil
	}
	// A transient upstream failure (network error, 429, 5xx) would otherwise
	// halt the pipeline at the human gate with zero tokens generated, so it
	// retries a bounded number of times before the job fails. The schedule is
	// exponential with jitter (base 2s: ~2s, 4s, 8s, 16s, 32s) so the window
	// spans over a minute and rides out upstream 500 bursts; stage routes
	// carry far longer timeouts and bound total time through ctx. Providers
	// also deliver failures inside HTTP 200 envelopes (body status "failed"
	// with an error object), so retry classification must read the body, not
	// just the status code.
	var raw []byte
	var statusCode int
	var status string
	var requestID string
	var err error
	attempts := 0
	for try := 1; ; try++ {
		attempts = try
		raw, statusCode, status, requestID, err = attempt()
		transient := err != nil || statusCode == http.StatusTooManyRequests || statusCode >= 500
		if !transient && statusCode >= 200 && statusCode < 300 {
			if failure, failed := embeddedResponseFailure(raw); failed && failure.retryable() {
				transient = true
			}
		}
		if !transient || try >= responsesMaxAttempts {
			break
		}
		base := client.RetryDelay
		if base <= 0 {
			base = responsesRetryDelay
		}
		delay := base << (try - 1)
		delay += rand.N(delay/2 + 1)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	diagnostic.Attempts = attempts
	diagnostic.HTTPStatus = statusCode
	diagnostic.UpstreamRequest = requestID
	diagnostic.ProviderCode = providerErrorCode(raw)
	if err != nil {
		diagnostic.Phase = "retry_exhausted"
		diagnostic.Retryable = true
		transcript, stats, redactErr := client.auditEnvelope(auditRequestValue, map[string]any{"diagnostic": diagnostic, "transport_error": err.Error()})
		if redactErr != nil {
			return Result{Diagnostic: &diagnostic}, redactErr
		}
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, fmt.Errorf("Responses API (%s) request for model %q exhausted %d attempts after a transport failure: %w", endpointHost, model, attempts, err)
	}
	var responseValue any
	if json.Unmarshal(raw, &responseValue) != nil {
		responseValue = string(raw)
	}
	var embedded *responseFailure
	if statusCode < 200 || statusCode >= 300 {
		diagnostic.Retryable = statusCode == http.StatusTooManyRequests || statusCode >= 500
		if diagnostic.Retryable && attempts >= responsesMaxAttempts {
			diagnostic.Phase = "retry_exhausted"
		} else {
			diagnostic.Phase = "provider_response"
		}
	} else if failure, failed := embeddedResponseFailure(raw); failed {
		embedded = &failure
		diagnostic.Retryable = failure.retryable()
		if diagnostic.Retryable && attempts >= responsesMaxAttempts {
			diagnostic.Phase = "retry_exhausted"
		} else {
			diagnostic.Phase = "provider_response"
		}
	}
	transcript, stats, redactErr := client.auditEnvelope(auditRequestValue, map[string]any{"response": responseValue, "diagnostic": diagnostic})
	if redactErr != nil {
		return Result{Diagnostic: &diagnostic}, redactErr
	}
	if statusCode < 200 || statusCode >= 300 {
		details := ""
		if diagnostic.UpstreamRequest != "" {
			details += " request_id=" + diagnostic.UpstreamRequest
		}
		if diagnostic.ProviderCode != "" {
			details += " provider_code=" + diagnostic.ProviderCode
		}
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, fmt.Errorf("Responses API (%s) %s for model %q after %d attempt(s): %s%s", endpointHost, diagnostic.Phase, model, attempts, status, details)
	}
	if embedded != nil {
		details := ""
		if diagnostic.UpstreamRequest != "" {
			details += " request_id=" + diagnostic.UpstreamRequest
		}
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, fmt.Errorf("Responses API (%s) %s for model %q after %d attempt(s): %s%s", endpointHost, diagnostic.Phase, model, attempts, embedded.describe(), details)
	}
	var decoded struct {
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		diagnostic.Phase = "response_validation"
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, fmt.Errorf("decode Responses API result for model %q: %w", model, err)
	}
	lastMessage := -1
	for index := len(decoded.Output) - 1; index >= 0; index-- {
		if decoded.Output[index].Type == "message" {
			lastMessage = index
			break
		}
	}
	var output strings.Builder
	if lastMessage >= 0 {
		for _, part := range decoded.Output[lastMessage].Content {
			if part.Type == "output_text" {
				output.WriteString(part.Text)
			}
		}
	}
	if output.Len() == 0 {
		diagnostic.Phase = "response_validation"
		return Result{Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, fmt.Errorf("Responses API returned no output_text for model %q", model)
	}
	diagnostic.Phase = "completed"
	return Result{Output: output.String(), Model: decoded.Model, TokensIn: decoded.Usage.InputTokens, TokensOut: decoded.Usage.OutputTokens, Transcript: transcript, Redactions: stats, Diagnostic: &diagnostic}, nil
}

func responsesRequestEnvelope(model string, input any, effort string) map[string]any {
	request := map[string]any{"model": model, "input": input, "store": false}
	if effort != "" {
		request["reasoning"] = map[string]any{"effort": effort}
	}
	return request
}

func requestDiagnostic(model, endpoint string, attachments []Attachment) Diagnostic {
	types := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		types = append(types, string(attachment.Kind)+":"+strings.ToLower(strings.TrimSpace(attachment.ContentType)))
	}
	sort.Strings(types)
	return Diagnostic{Phase: "request_preparation", Provider: "openai_responses", Endpoint: endpoint, Model: model, AttachmentCount: len(attachments), AttachmentTypes: types}
}

func responseEndpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func validateImageInputs(model, endpoint string, attachments []Attachment) (string, error) {
	for _, attachment := range attachments {
		if attachment.Kind != AttachmentImage {
			continue
		}
		if err := validateImageContent(attachment.ContentType, attachment.Content); err != nil {
			return "attachment_validation", fmt.Errorf("Responses API (%s) image preparation failed for model %q: artifact %s (%s): %w", endpoint, model, attachment.ID, attachment.ContentType, err)
		}
	}
	return "", nil
}

func validateImageContent(contentType string, content []byte) error {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	validSignature := false
	switch contentType {
	case "image/png":
		validSignature = len(content) >= 8 && bytes.Equal(content[:8], []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		validSignature = len(content) >= 3 && content[0] == 0xff && content[1] == 0xd8 && content[2] == 0xff
	case "image/gif":
		validSignature = len(content) >= 6 && (string(content[:6]) == "GIF87a" || string(content[:6]) == "GIF89a")
		if validSignature {
			decoded, err := gif.DecodeAll(bytes.NewReader(content))
			if err != nil {
				return fmt.Errorf("invalid GIF image: %w", err)
			}
			if len(decoded.Image) != 1 {
				return fmt.Errorf("animated GIF images are not supported by the OpenAI Responses image-input contract")
			}
		}
	case "image/webp":
		validSignature = len(content) >= 12 && string(content[:4]) == "RIFF" && string(content[8:12]) == "WEBP"
	default:
		return fmt.Errorf("unsupported image media type")
	}
	if !validSignature {
		return fmt.Errorf("content does not match its declared image media type")
	}
	return nil
}

// responseFailure is a failure the provider delivered inside a 2xx transport
// envelope: the Responses API (and OpenRouter passing through upstream faults)
// returns HTTP 200 with body status "failed" plus an error object, or status
// "incomplete" when generation stopped early. Without body inspection these
// surface as misleading "no output_text" extraction errors and are never
// retried.
type responseFailure struct {
	status  string
	code    string
	message string
	reason  string
}

// retryable reports whether the same request may succeed on resubmission.
// Incomplete responses are deterministic (the same request hits the same
// output cap); a failed status without a contradicting code is treated as a
// server-side fault, matching the provider's own "you can retry" guidance.
func (f responseFailure) retryable() bool {
	if f.status == "incomplete" {
		return false
	}
	if f.code == "" {
		return true
	}
	return strings.Contains(f.code, "server_error") || strings.Contains(f.code, "rate_limit")
}

func (f responseFailure) describe() string {
	switch {
	case f.status == "incomplete" && f.reason != "":
		return fmt.Sprintf("response incomplete (%s)", f.reason)
	case f.status == "incomplete":
		return "response incomplete"
	}
	description := "response status \"failed\""
	if f.code != "" {
		description += " provider_code=" + f.code
	}
	if f.message != "" {
		description += ": " + f.message
	}
	return description
}

func embeddedResponseFailure(raw []byte) (responseFailure, bool) {
	var envelope struct {
		Status string `json:"status"`
		Error  *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return responseFailure{}, false
	}
	failed := envelope.Error != nil || envelope.Status == "failed" || envelope.Status == "incomplete"
	if !failed {
		return responseFailure{}, false
	}
	failure := responseFailure{status: envelope.Status}
	if envelope.Error != nil {
		if envelope.Error.Code != nil {
			failure.code = fmt.Sprint(envelope.Error.Code)
		}
		failure.message = envelope.Error.Message
	}
	if envelope.IncompleteDetails != nil {
		failure.reason = envelope.IncompleteDetails.Reason
	}
	return failure, true
}

func providerErrorCode(raw []byte) string {
	var envelope struct {
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Error.Code == nil {
		return ""
	}
	return fmt.Sprint(envelope.Error.Code)
}

func (client *OpenAI) auditEnvelope(request, response any) ([]byte, core.RedactionStats, error) {
	envelope, _ := json.Marshal(map[string]any{"request": request, "result": response})
	transcript, stats, err := redact.New([]string{client.APIKey}).RedactJSON(envelope)
	if err != nil {
		return nil, core.RedactionStats{}, err
	}
	return transcript, stats, nil
}

func (client *OpenAI) transcribe(ctx context.Context, endpoint string, attachment Attachment) (string, error) {
	endpointHost := responseEndpointHost(endpoint)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, attachment.Name))
	header.Set("Content-Type", attachment.ContentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err = part.Write(attachment.Content); err != nil {
		return "", err
	}
	if err = writer.WriteField("model", "gpt-4o-mini-transcribe"); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+client.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	httpClient := client.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Hour}
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Transcription API (%s) returned %s", endpointHost, response.Status)
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("decode transcription result: %w", err)
	}
	if strings.TrimSpace(decoded.Text) == "" {
		return "", fmt.Errorf("transcription returned no text")
	}
	return decoded.Text, nil
}
