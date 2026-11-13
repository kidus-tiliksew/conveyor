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
	var disposition orderRecoverDisposition
	asJSON := false
	cmd := &cobra.Command{
		Use:   "recover <order-id>",
		Short: "Recover a work order, optionally with operator direction or verification dispositions",
		Long: "Posts {request_id, direction} to POST /v1/work-orders/{id}/recover. The request ID is generated when omitted and is also sent as X-Request-Id and X-Idempotency-Key.\n\n" +
			"--disposition records one answer for each attempt in the disposition set of a verify order. The command reads the set from the next action of the order's task (details.dispositions) and sends one entry for each attempt, with that disposition and the --reason text. " +
			"Each --run <run-id>=<disposition> gives one attempt a different answer. The command sends nothing when the next action states no set, or when --run names an attempt that is not in the set. " +
			"The server records the authorization recovery:<request_id> on each attempt in the set.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			var verification *orderRecoverDisposition
			if flags.Changed("disposition") || flags.Changed("run") || flags.Changed("reason") {
				verification = &disposition
			}
			return runOrderRecover(newClient(), cmd.OutOrStdout(), args[0], direction, requestID, verification, asJSON)
		},
	}
	cmd.Flags().StringVar(&direction, "direction", "", "operator direction for the recovered order")
	cmd.Flags().StringVar(&requestID, "request-id", "", "idempotency key (generated when omitted)")
	cmd.Flags().StringVar(&disposition.Default, "disposition", "", "answer for each attempt in the disposition set: applied, not_applied, or unknown")
	cmd.Flags().StringArrayVar(&disposition.Runs, "run", nil, "a different answer for one attempt, as <run-id>=<disposition> (repeatable)")
	cmd.Flags().StringVar(&disposition.Reason, "reason", "", "operator reason for the dispositions (required with --disposition)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the server body verbatim")
	return cmd
}

// orderRecoverDisposition is the operator's input for the dispositions of a
// verify order's recovery: one answer for every attempt in the order's
// disposition set, answers for single attempts, and one reason. The command
// reads the set from the next action, so the operator names no context,
// digest, or operation (component-verification-service invariant 45;
// component-next-action invariant 15; DEC-133(4)).
type orderRecoverDisposition struct {
	Default string
	Runs    []string
	Reason  string
}

func validOrderDisposition(value string) bool {
	switch value {
	case "applied", "not_applied", "unknown":
		return true
	}
	return false
}

// overrides checks the flags and returns the --run answers by run ID.
func (d *orderRecoverDisposition) overrides() (map[string]string, error) {
	switch strings.TrimSpace(d.Default) {
	case "":
		return nil, fmt.Errorf("--run and --reason require --disposition")
	default:
		if !validOrderDisposition(d.Default) {
			return nil, fmt.Errorf("--disposition must be applied, not_applied, or unknown, not %q", d.Default)
		}
	}
	if strings.TrimSpace(d.Reason) == "" {
		return nil, fmt.Errorf("--disposition requires --reason")
	}
	runs := make(map[string]string, len(d.Runs))
	for _, value := range d.Runs {
		run, answer, ok := strings.Cut(value, "=")
		run, answer = strings.TrimSpace(run), strings.TrimSpace(answer)
		if !ok || run == "" || answer == "" {
			return nil, fmt.Errorf("--run %q must have the form <run-id>=<disposition>", value)
		}
		if !validOrderDisposition(answer) {
			return nil, fmt.Errorf("--run %s: the disposition must be applied, not_applied, or unknown, not %q", run, answer)
		}
		if _, dup := runs[run]; dup {
			return nil, fmt.Errorf("--run names %s more than once", run)
		}
		runs[run] = answer
	}
	return runs, nil
}

// orderDispositionEntry is one entry of details.dispositions in the next
// action (component-next-action invariant 15).
type orderDispositionEntry struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
	Subject      string   `json:"subject"`
	State        string   `json:"state"`
}

// orderRecoverEntry is one entry of the verification object the recover
// route accepts. operation_ids is [] when the attempt has none, never null.
type orderRecoverEntry struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
	Disposition  string   `json:"disposition"`
}

type orderRecoverVerification struct {
	Reason       string              `json:"reason"`
	Dispositions []orderRecoverEntry `json:"dispositions"`
}

// readOrderDispositionSet reads the disposition set of orderID from the next
// action of its task. It refuses when the next action does not ask for
// dispositions on this order, or states no set, and then quotes the server's
// refusal of the record_dispositions choice when there is one.
func readOrderDispositionSet(c *client, orderID string) ([]orderDispositionEntry, error) {
	order, found, err := resolveWorkOrder(c, orderID)
	if err != nil {
		return nil, err
	}
	if !found || order.TaskID == "" {
		return nil, fmt.Errorf("work order %s not found", orderID)
	}
	var action struct {
		Kind         string `json:"kind"`
		CurrentOrder *struct {
			ID string `json:"id"`
		} `json:"current_order"`
		Details struct {
			Dispositions []orderDispositionEntry `json:"dispositions"`
			Refusals     []struct {
				Choice string `json:"choice"`
				Text   string `json:"text"`
			} `json:"refusals"`
		} `json:"details"`
	}
	if err := c.doHeaders(http.MethodGet, "/v1/tasks/"+url.PathEscape(order.TaskID)+"/next-action", nil, &action, nil); err != nil {
		return nil, err
	}
	refusal := ""
	for _, r := range action.Details.Refusals {
		if r.Choice == "record_dispositions" && strings.TrimSpace(r.Text) != "" {
			refusal = ": " + r.Text
		}
	}
	if action.Kind != "verification_disposition" || action.CurrentOrder == nil || action.CurrentOrder.ID != orderID {
		return nil, fmt.Errorf("the next action of task %s does not ask for dispositions on work order %s; nothing was sent", order.TaskID, orderID)
	}
	if len(action.Details.Dispositions) == 0 {
		return nil, fmt.Errorf("the next action of task %s states no disposition set for work order %s%s; nothing was sent", order.TaskID, orderID, refusal)
	}
	return action.Details.Dispositions, nil
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
		Short: "Retire a queued or stale work order, except a review seat",
		Long:  "Reads the order from GET /v1/work-orders and, when it is queued or stale and not a review seat, posts {reason, request_id} to POST /v1/work-orders/{id}/preempt, which retires it to cancelled. A claimed order is refused before anything is sent; stop its attempt with operator preemption instead. The request ID is generated when omitted and is also sent as X-Request-Id and X-Idempotency-Key.",
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

// runOrderRecover posts the recovery. A non-nil disposition reads the
// disposition set from the next action and adds one entry for each attempt;
// on success the human output prints each run with its disposition and the
// authorization the server recorded.
func runOrderRecover(c *client, w io.Writer, orderID, direction, requestID string, disposition *orderRecoverDisposition, asJSON bool) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	var verification *orderRecoverVerification
	var set []orderDispositionEntry
	if disposition != nil {
		runs, err := disposition.overrides()
		if err != nil {
			return err
		}
		if set, err = readOrderDispositionSet(c, orderID); err != nil {
			return err
		}
		verification = &orderRecoverVerification{Reason: disposition.Reason, Dispositions: make([]orderRecoverEntry, 0, len(set))}
		members := make(map[string]bool, len(set))
		for _, entry := range set {
			members[entry.RunID] = true
			answer := disposition.Default
			if override, ok := runs[entry.RunID]; ok {
				answer = override
			}
			operations := entry.OperationIDs
			if operations == nil {
				operations = []string{}
			}
			verification.Dispositions = append(verification.Dispositions, orderRecoverEntry{ContextID: entry.ContextID, RunID: entry.RunID, OperationIDs: operations, InputDigest: entry.InputDigest, Disposition: answer})
		}
		for _, value := range disposition.Runs {
			run, _, _ := strings.Cut(value, "=")
			if run = strings.TrimSpace(run); !members[run] {
				return fmt.Errorf("--run %s: the run is not in the disposition set of work order %s; nothing was sent", run, orderID)
			}
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
		human += fmt.Sprintf("\nverification authorization %s", authorization)
		for i, entry := range verification.Dispositions {
			human += fmt.Sprintf("\n  run %s: %s (%s)", entry.RunID, entry.Disposition, set[i].Subject)
		}
		human += fmt.Sprintf("\nnext verifier: conveyor kit verify --replay-authorization %s ...", authorization)
	}
	return writeOrderResult(w, response, asJSON, human)
}

// resolvedWorkOrder is the part of a work order the order commands read.
type resolvedWorkOrder struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	State  string `json:"state"`
	Stage  string `json:"stage"`
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
	case order.Stage == "review":
		// A retired seat would stay in its round's required count, so the
		// round could never settle (req-review-gates-evidence AC-2.6).
		return fmt.Errorf("work order %s is a review seat; retire refuses to remove a seat from its round", orderID)
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
