package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// kitRPC uses the same claim-bound native tool contract as other verifiers.
// The launcher credential and claim token stay in the runner process.
type kitRPC struct {
	client                     *client
	order, session, claimToken string
}

func (k kitRPC) call(ctx context.Context, name string, input any, result any) error {
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
	if envelope.Error != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		return fmt.Errorf("%s refused or unavailable; inspect the claim and verification context", name)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal([]byte(envelope.Result.Content[0].Text), result)
}
