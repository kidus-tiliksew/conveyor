// Package inprocess executes the small always-on pipeline stages directly in
// conveyord. Operator-owned coding agents remain outside this package.
package inprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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
	responsesMaxAttempts = 3
	responsesRetryDelay  = 2 * time.Second
)

func (client *OpenAI) Run(ctx context.Context, model string, input Input) (Result, error) {
	if strings.TrimSpace(client.APIKey) == "" {
		return Result{}, fmt.Errorf("CONVEYOR_API_KEY is required for in-process stages")
	}
	endpoint := strings.TrimRight(client.BaseURL, "/")
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
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
				return Result{}, fmt.Errorf("transcribe artifact %s (%s): %w", attachment.ID, attachment.Name, err)
			}
			text := fmt.Sprintf("# Audio attachment: %s (artifact %s)\n\n%s", attachment.Name, attachment.ID, transcript)
			content = append(content, map[string]any{"type": "input_text", "text": text})
			auditContent = append(auditContent, map[string]any{"type": "input_text", "text": fmt.Sprintf("# Audio attachment: %s (artifact %s)\n\n[transcript supplied to model; %d characters]", attachment.Name, attachment.ID, len(transcript))})
		default:
			return Result{}, fmt.Errorf("unsupported attachment kind %q for artifact %s", attachment.Kind, attachment.ID)
		}
	}
	requestInput := []map[string]any{{"role": "user", "content": content}}
	auditInput := []map[string]any{{"role": "user", "content": auditContent}}
	body, _ := json.Marshal(map[string]any{"model": model, "input": requestInput, "store": false})
	httpClient := client.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Hour}
	}
	attempt := func() ([]byte, int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, 0, "", err
		}
		req.Header.Set("Authorization", "Bearer "+client.APIKey)
		req.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(req)
		if err != nil {
			return nil, 0, "", err
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
		if err != nil {
			return nil, 0, "", err
		}
		return raw, response.StatusCode, response.Status, nil
	}
	// A transient upstream failure (network error, 429, 5xx) would otherwise
	// halt the pipeline at the human gate with zero tokens generated, so it
	// retries a bounded number of times before the job fails.
	var raw []byte
	var statusCode int
	var status string
	var err error
	for try := 1; ; try++ {
		raw, statusCode, status, err = attempt()
		transient := err != nil || statusCode == http.StatusTooManyRequests || statusCode >= 500
		if !transient || try >= responsesMaxAttempts {
			break
		}
		delay := client.RetryDelay
		if delay <= 0 {
			delay = responsesRetryDelay
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(try) * delay):
		}
	}
	if err != nil {
		return Result{}, err
	}
	redactor := redact.New([]string{client.APIKey})
	var responseValue any
	if json.Unmarshal(raw, &responseValue) != nil {
		responseValue = string(raw)
	}
	requestValue := map[string]any{"model": model, "input": auditInput, "store": false}
	envelope, _ := json.Marshal(map[string]any{"request": requestValue, "response": responseValue})
	transcript, stats, redactErr := redactor.RedactJSON(envelope)
	if redactErr != nil {
		return Result{}, redactErr
	}
	if statusCode < 200 || statusCode >= 300 {
		return Result{Transcript: transcript, Redactions: stats}, fmt.Errorf("OpenAI Responses API returned %s", status)
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
		return Result{Transcript: transcript, Redactions: stats}, fmt.Errorf("decode Responses API result: %w", err)
	}
	var output strings.Builder
	for _, item := range decoded.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				output.WriteString(part.Text)
			}
		}
	}
	if output.Len() == 0 {
		return Result{Transcript: transcript, Redactions: stats}, fmt.Errorf("Responses API returned no output_text")
	}
	return Result{Output: output.String(), Model: decoded.Model, TokensIn: decoded.Usage.InputTokens, TokensOut: decoded.Usage.OutputTokens, Transcript: transcript, Redactions: stats}, nil
}

func (client *OpenAI) transcribe(ctx context.Context, endpoint string, attachment Attachment) (string, error) {
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
		return "", fmt.Errorf("OpenAI transcription API returned %s", response.Status)
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
