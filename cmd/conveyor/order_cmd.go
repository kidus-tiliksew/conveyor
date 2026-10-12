package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// conveyor order recover|redispatch are the CLI equivalents of the recovery
// choices the next-action projection offers (req-operator-next-action AC-5.5).
// The server enforces recover_work; the CLI grants nothing and passes refusals
// through unchanged.

func orderCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "order", Short: "Recover or redispatch work orders"}
	cmd.AddCommand(orderRecoverCmd(), orderRedispatchCmd())
	return cmd
}

func orderRecoverCmd() *cobra.Command {
	var direction, requestID string
	asJSON := false
	cmd := &cobra.Command{
		Use:   "recover <order-id>",
		Short: "Recover a work order, optionally with operator direction",
		Long:  "Posts {request_id, direction} to POST /v1/work-orders/{id}/recover. The request ID is generated when omitted and is also sent as X-Request-Id and X-Idempotency-Key.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOrderRecover(newClient(), cmd.OutOrStdout(), args[0], direction, requestID, asJSON)
		},
	}
	cmd.Flags().StringVar(&direction, "direction", "", "operator direction for the recovered order")
	cmd.Flags().StringVar(&requestID, "request-id", "", "idempotency key (generated when omitted)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the server body verbatim")
	return cmd
}

func orderRedispatchCmd() *cobra.Command {
	var requestID string
	asJSON := false
	cmd := &cobra.Command{
		Use:   "redispatch <order-id>",
		Short: "Put a work order's task back in the queue",
		Long:  "Resolves the order's task from GET /v1/work-orders, then posts to POST /v1/tasks/{task_id}/redispatch. The request ID is generated when omitted and is sent as body request_id, X-Request-Id, and X-Idempotency-Key.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOrderRedispatch(newClient(), cmd.OutOrStdout(), args[0], requestID, asJSON)
		},
	}
	cmd.Flags().StringVar(&requestID, "request-id", "", "idempotency key (generated when omitted)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the server body verbatim")
	return cmd
}

func orderRequestID(given string) (string, error) {
	if given = strings.TrimSpace(given); given != "" {
		return given, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func orderPost(c *client, path, requestID string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var response json.RawMessage
	headers := map[string]string{"X-Request-Id": requestID, "X-Idempotency-Key": requestID}
	if err := c.doHeaders(http.MethodPost, path, body, &response, headers); err != nil {
		return nil, err
	}
	return response, nil
}

func runOrderRecover(c *client, w io.Writer, orderID, direction, requestID string, asJSON bool) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	requestID, err := orderRequestID(requestID)
	if err != nil {
		return err
	}
	payload := struct {
		RequestID string `json:"request_id"`
		Direction string `json:"direction,omitempty"`
	}{requestID, direction}
	response, err := orderPost(c, "/v1/work-orders/"+url.PathEscape(orderID)+"/recover", requestID, payload)
	if err != nil {
		return err
	}
	return writeOrderResult(w, response, asJSON, fmt.Sprintf("recovered work order %s (request %s)", orderID, requestID))
}

func runOrderRedispatch(c *client, w io.Writer, orderID, requestID string, asJSON bool) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	requestID, err := orderRequestID(requestID)
	if err != nil {
		return err
	}
	var orders []struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	if err := c.doHeaders(http.MethodGet, "/v1/work-orders", nil, &orders, nil); err != nil {
		return err
	}
	taskID := ""
	for _, order := range orders {
		if order.ID == orderID {
			taskID = order.TaskID
			break
		}
	}
	if taskID == "" {
		return fmt.Errorf("work order %s not found", orderID)
	}
	payload := struct {
		RequestID string `json:"request_id"`
	}{requestID}
	response, err := orderPost(c, "/v1/tasks/"+url.PathEscape(taskID)+"/redispatch", requestID, payload)
	if err != nil {
		return err
	}
	return writeOrderResult(w, response, asJSON, fmt.Sprintf("redispatched task %s for work order %s (request %s)", taskID, orderID, requestID))
}

func writeOrderResult(w io.Writer, response json.RawMessage, asJSON bool, human string) error {
	if asJSON {
		if len(response) == 0 {
			response = json.RawMessage("null")
		}
		_, err := fmt.Fprintf(w, "%s\n", response)
		return err
	}
	_, err := fmt.Fprintln(w, human)
	return err
}
