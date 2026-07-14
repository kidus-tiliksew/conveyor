// Package inprocess executes the small always-on pipeline stages directly in
// conveyord. Operator-owned coding agents remain outside this package.
package inprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	CostUSD    float64
	Transcript []byte
	Redactions core.RedactionStats
}

type Agent interface {
	Run(context.Context, string, string) (Result, error)
}

type OpenAI struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func (client *OpenAI) Run(ctx context.Context, model, prompt string) (Result, error) {
	if strings.TrimSpace(client.APIKey) == "" {
		return Result{}, fmt.Errorf("CONVEYOR_API_KEY is required for in-process stages")
	}
	endpoint := strings.TrimRight(client.BaseURL, "/")
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": prompt, "store": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/responses", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+client.APIKey)
	req.Header.Set("Content-Type", "application/json")
	httpClient := client.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Hour}
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return Result{}, err
	}
	redactor := redact.New([]string{client.APIKey})
	var responseValue any
	if json.Unmarshal(raw, &responseValue) != nil {
		responseValue = string(raw)
	}
	var requestValue any
	_ = json.Unmarshal(body, &requestValue)
	envelope, _ := json.Marshal(map[string]any{"request": requestValue, "response": responseValue})
	transcript, stats, redactErr := redactor.RedactJSON(envelope)
	if redactErr != nil {
		return Result{}, redactErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{Transcript: transcript, Redactions: stats}, fmt.Errorf("OpenAI Responses API returned %s", response.Status)
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
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			InputTokenDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
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
	cost, err := estimateOpenAICost(decoded.Model, decoded.Usage.InputTokens, decoded.Usage.InputTokenDetails.CachedTokens, decoded.Usage.OutputTokens)
	if err != nil {
		return Result{Output: output.String(), Model: decoded.Model, TokensIn: decoded.Usage.InputTokens, TokensOut: decoded.Usage.OutputTokens, Transcript: transcript, Redactions: stats}, err
	}
	return Result{Output: output.String(), Model: decoded.Model, TokensIn: decoded.Usage.InputTokens, TokensOut: decoded.Usage.OutputTokens, CostUSD: cost, Transcript: transcript, Redactions: stats}, nil
}

type tokenPrice struct{ input, cached, output float64 }

// Prices are standard-processing USD per million text tokens published on
// July 14, 2026. Keeping the catalog explicit makes an unknown model fail
// closed instead of silently disabling the USD breaker.
func estimateOpenAICost(model string, input, cached, output int64) (float64, error) {
	prices := []struct {
		prefix string
		price  tokenPrice
	}{
		{"gpt-5.6-luna", tokenPrice{input: 1, cached: .1, output: 6}},
		{"gpt-5.4-pro", tokenPrice{input: 30, cached: 30, output: 180}},
		{"gpt-5.4-mini", tokenPrice{input: .75, cached: .075, output: 4.5}},
		{"gpt-5.4-nano", tokenPrice{input: .2, cached: .02, output: 1.25}},
		{"gpt-5.4", tokenPrice{input: 2.5, cached: .25, output: 15}},
	}
	for _, item := range prices {
		if model == item.prefix || strings.HasPrefix(model, item.prefix+"-") {
			if cached < 0 || cached > input {
				return 0, fmt.Errorf("invalid cached token count %d for %d input tokens", cached, input)
			}
			inputMultiplier, outputMultiplier := 1.0, 1.0
			if (item.prefix == "gpt-5.4" || item.prefix == "gpt-5.4-pro") && input > 272_000 {
				inputMultiplier, outputMultiplier = 2, 1.5
			}
			uncached := input - cached
			return (float64(uncached)*item.price.input*inputMultiplier + float64(cached)*item.price.cached*inputMultiplier + float64(output)*item.price.output*outputMultiplier) / 1_000_000, nil
		}
	}
	return 0, fmt.Errorf("no in-process token price registered for model %q", model)
}
