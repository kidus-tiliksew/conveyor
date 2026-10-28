package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// kitRPC uses the same claim-bound native tool contract as other verifiers.
// The launcher credential and claim token stay in the runner process.
type kitRPC struct {
	client                     *client
	order, session, claimToken string
	// timeout is the transport ceiling for one call; zero selects
	// kitDefaultRPCTimeout (component-verification-runner).
	timeout time.Duration
}

// Remote server processing alone can take several seconds, so the transport
// ceiling is sized for a remote round trip. Child teardown keeps its own short
// bound and never shares this one (component-verification-runner).
const (
	kitDefaultRPCTimeout = 30 * time.Second
	kitMinRPCTimeout     = time.Second
	kitMaxRPCTimeout     = 10 * time.Minute
	kitTeardownTimeout   = 5 * time.Second
)

func validateKitRPCTimeout(d time.Duration) error {
	if d < kitMinRPCTimeout || d > kitMaxRPCTimeout {
		return fmt.Errorf("--rpc-timeout must be between %s and %s", kitMinRPCTimeout, kitMaxRPCTimeout)
	}
	return nil
}

func (k kitRPC) call(ctx context.Context, name string, input any, result any) error {
	// The caller bounds a claim-bound call by the lease or execution deadline;
	// the transport ceiling bounds it when the caller's bound is later.
	timeout := k.timeout
	if timeout == 0 {
		timeout = kitDefaultRPCTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := map[string]any{}
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &args); err != nil {
			return err
		}
	}
	args["workspace_id"], args["work_order_id"], args["session_id"] = k.client.workspace, k.order, k.session
	if k.claimToken != "" && name != "get_work_order" && name != "renew_work_order" && name != "report_progress" {
		args["client_token"] = k.claimToken
	}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return err
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = k.client.workerDoContext(ctx, http.MethodPost, "/mcp", payload, &envelope, k.client.token); err != nil {
		return err
	}
	if envelope.Error != nil {
		return k.refused(name, envelope.Error.Message)
	}
	if envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		text := ""
		if envelope.Result.IsError && len(envelope.Result.Content) == 1 {
			text = envelope.Result.Content[0].Text
		}
		return k.refused(name, text)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal([]byte(envelope.Result.Content[0].Text), result)
}

// kitRefusalLimit bounds the server refusal text a runner error carries
// (component-verification-runner).
const kitRefusalLimit = 4096

// refused carries the server's refusal text, such as
// "<code> at <field>: <rule> <remedy>", so a verifier can correct the call.
// The text passes the runner's exact-value redactor before it leaves the
// runner. Without text it keeps the generic sentence. The result is an
// ordinary error, so admission and checkpoint classification are unchanged
// (component-verification-runner; component-verification-service).
func (k kitRPC) refused(name, text string) error {
	clean, _ := redact.New(append(kitParentSecrets(), k.client.token, k.claimToken)).Redact(text)
	clean = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, clean))
	if clean == "" {
		return fmt.Errorf("%s refused or unavailable; inspect the claim and verification context", name)
	}
	if len(clean) > kitRefusalLimit {
		cut := kitRefusalLimit - 3
		for cut > 0 && !utf8.RuneStart(clean[cut]) {
			cut--
		}
		clean = clean[:cut] + "..."
	}
	return fmt.Errorf("%s refused: %s", name, clean)
}
