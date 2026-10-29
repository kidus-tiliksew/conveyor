package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// conveyor order recover|redispatch are the CLI equivalents of the recovery
// choices the next-action projection offers (req-operator-next-action AC-5.5).
// conveyor order retire retires one queued or stale order through the
// existing preempt route (component-work-orders "Recovery, redispatch, and
// preemption"; feature-operator-next-action NA-8). The server enforces
// recover_work; the CLI grants nothing and passes refusals through unchanged.

func orderCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "order", Short: "Recover, redispatch, or retire work orders"}
	cmd.AddCommand(orderRecoverCmd(), orderRedispatchCmd(), orderRetireCmd())
	return cmd
}

func orderRecoverCmd() *cobra.Command {
	var direction, requestID string
	var verification orderRecoverVerification
	asJSON := false
	cmd := &cobra.Command{
		Use:   "recover <order-id>",
		Short: "Recover a work order, optionally with operator direction or a verification disposition",
		Long: "Posts {request_id, direction} to POST /v1/work-orders/{id}/recover. The request ID is generated when omitted and is also sent as X-Request-Id and X-Idempotency-Key.\n\n" +
			"--disposition adds the verification object of a verify order's recovery: --context-id, --run-id, --input-digest, and --reason are required with it, and each --operation-id names one unresolved operation (none sends []). " +
			"Copy the binding from the order's next action (details.disposition). The server issues the one-use authorization recovery:<request_id>, which the next verifier passes as conveyor kit verify --replay-authorization.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			set := flags.Changed("disposition") || flags.Changed("context-id") || flags.Changed("run-id") || flags.Changed("operation-id") || flags.Changed("input-digest") || flags.Changed("reason")
			var disposition *orderRecoverVerification
			if set {
				disposition = &verification
			}
			return runOrderRecover(newClient(), cmd.OutOrStdout(), args[0], direction, requestID, disposition, asJSON)
		},
	}
	cmd.Flags().StringVar(&direction, "direction", "", "operator direction for the recovered order")
	cmd.Flags().StringVar(&requestID, "request-id", "", "idempotency key (generated when omitted)")
	cmd.Flags().StringVar(&verification.Disposition, "disposition", "", "verification disposition: applied, not_applied, or unknown")
	cmd.Flags().StringVar(&verification.ContextID, "context-id", "", "verification context of the disposed run (required with --disposition)")
	cmd.Flags().StringVar(&verification.RunID, "run-id", "", "verification run the disposition binds (required with --disposition)")
	cmd.Flags().StringArrayVar(&verification.OperationIDs, "operation-id", nil, "unresolved operation the disposition covers (repeatable; none sends [])")
	cmd.Flags().StringVar(&verification.InputDigest, "input-digest", "", "safe-input digest of the run (required with --disposition)")
	cmd.Flags().StringVar(&verification.Reason, "reason", "", "operator reason for the disposition (required with --disposition)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the server body verbatim")
	return cmd
}

// orderRecoverVerification is the verification object of a verify order's
// recovery request (component-verification-service, Operator checkpoint and
// recovery). The server checks every field; the CLI refuses an incomplete
// object before sending and encodes no operations as [], never null.
type orderRecoverVerification struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
	Disposition  string   `json:"disposition"`
	Reason       string   `json:"reason"`
}

func (v *orderRecoverVerification) validate() error {
	switch strings.TrimSpace(v.Disposition) {
	case "":
		return fmt.Errorf("--context-id, --run-id, --operation-id, --input-digest, and --reason require --disposition")
	case "applied", "not_applied", "unknown":
	default:
		return fmt.Errorf("--disposition must be applied, not_applied, or unknown, not %q", v.Disposition)
	}
	var missing []string
	for _, field := range []struct{ flag, value string }{{"--context-id", v.ContextID}, {"--run-id", v.RunID}, {"--input-digest", v.InputDigest}, {"--reason", v.Reason}} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.flag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("--disposition requires %s", strings.Join(missing, ", "))
	}
	if v.OperationIDs == nil {
		v.OperationIDs = []string{}
	}
	return nil
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

func orderRetireCmd() *cobra.Command {
	var reason, requestID string
	asJSON := false
	cmd := &cobra.Command{
		Use:   "retire <order-id> --reason <text>",
		Short: "Retire a queued or stale work order",
		Long:  "Reads the order from GET /v1/work-orders and, when it is queued or stale, posts {reason, request_id} to POST /v1/work-orders/{id}/preempt, which retires it to cancelled. A claimed order is refused before anything is sent; stop its attempt with operator preemption instead. The request ID is generated when omitted and is also sent as X-Request-Id and X-Idempotency-Key.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOrderRetire(newClient(), cmd.OutOrStdout(), args[0], reason, requestID, asJSON)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the order is retired (required)")
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

// runOrderRecover posts the recovery. A non-nil verification adds the
// disposition object; on success the human output names the one-use
// authorization the server issued and how a verifier passes it.
func runOrderRecover(c *client, w io.Writer, orderID, direction, requestID string, verification *orderRecoverVerification, asJSON bool) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	if verification != nil {
		if err := verification.validate(); err != nil {
			return err
		}
	}
	requestID, err := orderRequestID(requestID)
	if err != nil {
		return err
	}
	payload := struct {
		Verification *orderRecoverVerification `json:"verification,omitempty"`
		RequestID    string                    `json:"request_id"`
		Direction    string                    `json:"direction,omitempty"`
	}{verification, requestID, direction}
	response, err := orderPost(c, "/v1/work-orders/"+url.PathEscape(orderID)+"/recover", requestID, payload)
	if err != nil {
		return err
	}
	human := fmt.Sprintf("recovered work order %s (request %s)", orderID, requestID)
	if verification != nil {
		authorization := "recovery:" + requestID
		human += fmt.Sprintf("\nverification authorization %s (%s, run %s)\nnext verifier: conveyor kit verify --replay-authorization %s ...", authorization, verification.Disposition, verification.RunID, authorization)
	}
	return writeOrderResult(w, response, asJSON, human)
}

// resolvedWorkOrder is the part of a work order the order commands read.
type resolvedWorkOrder struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	State  string `json:"state"`
}

// resolveWorkOrder reads one order through GET /v1/work-orders/{id}, which
// serves an order in any state; the list holds live orders only. A 404 falls
// back to one list search, so a server that predates the exact read still
// resolves a live order (component-work-orders, Listing).
func resolveWorkOrder(c *client, orderID string) (resolvedWorkOrder, bool, error) {
	var order resolvedWorkOrder
	err := c.doHeaders(http.MethodGet, "/v1/work-orders/"+url.PathEscape(orderID), nil, &order, nil)
	if err == nil {
		return order, order.ID == orderID, nil
	}
	var status *apiStatusError
	if !errors.As(err, &status) || status.Status != http.StatusNotFound {
		return resolvedWorkOrder{}, false, err
	}
	var orders []resolvedWorkOrder
	if err := c.doHeaders(http.MethodGet, "/v1/work-orders", nil, &orders, nil); err != nil {
		return resolvedWorkOrder{}, false, err
	}
	for _, candidate := range orders {
		if candidate.ID == orderID {
			return candidate, true, nil
		}
	}
	return resolvedWorkOrder{}, false, nil
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
	order, found, err := resolveWorkOrder(c, orderID)
	if err != nil {
		return err
	}
	taskID := order.TaskID
	if !found || taskID == "" {
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

func runOrderRetire(c *client, w io.Writer, orderID, reason, requestID string, asJSON bool) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	order, found, err := resolveWorkOrder(c, orderID)
	if err != nil {
		return err
	}
	state := order.State
	switch {
	case !found:
		return fmt.Errorf("work order %s not found", orderID)
	case state == "claimed":
		return fmt.Errorf("work order %s is claimed; retire refuses a live claim, stop its attempt with operator preemption instead", orderID)
	case state != "queued" && state != "stale":
		return fmt.Errorf("work order %s is %s; only queued or stale orders can be retired", orderID, state)
	}
	requestID, err = orderRequestID(requestID)
	if err != nil {
		return err
	}
	payload := struct {
		Reason    string `json:"reason"`
		RequestID string `json:"request_id"`
	}{reason, requestID}
	response, err := orderPost(c, "/v1/work-orders/"+url.PathEscape(orderID)+"/preempt", requestID, payload)
	if err != nil {
		return err
	}
	var result struct {
		WorkOrder struct {
			State string `json:"state"`
		} `json:"work_order"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return fmt.Errorf("decode preempt result: %w", err)
	}
	returned := result.WorkOrder.State
	if returned != "cancelled" {
		// The order changed between the read and the preempt; report what the
		// server did rather than calling it retired.
		if writeErr := writeOrderResult(w, response, asJSON, fmt.Sprintf("work order %s returned state %s (request %s)", orderID, returned, requestID)); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("work order %s was not retired: the server returned state %q", orderID, returned)
	}
	return writeOrderResult(w, response, asJSON, fmt.Sprintf("retired work order %s: state %s (request %s)", orderID, returned, requestID))
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
