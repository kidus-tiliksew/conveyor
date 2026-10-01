package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/spf13/cobra"
)

// verificationCmd hosts operator verification acts. Grant and revoke use the
// invoking user's credential through REST only; no MCP tool, worker route or
// launcher path reaches them (component-runtime VK-RUNTIME-2;
// feature-verification-kit-execution VK-12.1; req-verification-kits REQ-7/AC-7.3).
func verificationCmd() *cobra.Command {
	command := &cobra.Command{Use: "verification", Short: "Operator verification acts"}
	permissions := &cobra.Command{Use: "permissions", Short: "Inspect, grant and revoke claim-bound verification permission grants"}
	permissions.AddCommand(verificationInspectCmd(), verificationGrantCmd(), verificationRevokeCmd())
	command.AddCommand(permissions)
	return command
}

func verificationInspectCmd() *cobra.Command {
	var contextID string
	var asJSON bool
	command := &cobra.Command{Use: "inspect <work-order-id>", Short: "Show the frozen context, subjects, required actions, grants and eligibility", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		view, err := newClient().verificationPermissionView(args[0], contextID)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
		}
		renderVerificationPermissionView(cmd.OutOrStdout(), view)
		return nil
	}}
	command.Flags().StringVar(&contextID, "context-id", "", "retained context to inspect (default: the order's latest context)")
	command.Flags().BoolVar(&asJSON, "json", false, "print the projection as JSON")
	return command
}

func verificationGrantCmd() *cobra.Command {
	var contextID, subjectRef, requestKey string
	var actionArgs []string
	var noActions, yes, asJSON bool
	command := &cobra.Command{Use: "grant <work-order-id>", Short: "Issue one exact operator grant and read back its receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(requestKey) == "" {
			return fmt.Errorf("--request-key is required; reuse it to retry an uncertain response")
		}
		if strings.TrimSpace(subjectRef) == "" {
			return fmt.Errorf("--subject is required (kit:<kit-id>/<exercise-id> or ordinary:<obligation-id>)")
		}
		if noActions == (len(actionArgs) > 0) {
			return fmt.Errorf("supply --action for each granted action, or --no-actions for an explicit empty list")
		}
		actions := []core.VerificationPermission{}
		for _, raw := range actionArgs {
			action, err := parseVerificationAction(raw)
			if err != nil {
				return err
			}
			actions = append(actions, action)
		}
		c := newClient()
		view, err := c.verificationPermissionView(args[0], contextID)
		if err != nil {
			return err
		}
		if view.Context == nil {
			return fmt.Errorf("no prepared verification context: %s", view.Eligibility.Recovery)
		}
		if view.Eligibility.State != "eligible" {
			return &verificationPermissionRefusal{Reason: view.Eligibility.State, Message: view.Eligibility.Reason, Recovery: view.Eligibility.Recovery}
		}
		row, err := selectVerificationSubject(view, subjectRef)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, slot := range row.Actions {
			if slot.Required && !verificationActionsCover(actions, slot.Kind, slot.Binding) {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: no action supplied for required %s binding %s; the runner will report it missing\n", slot.Kind, verificationBindingLabel(slot.Binding))
			}
		}
		// The subject and its digests are copied from the frozen projection; the
		// operator never types a digest.
		request := store.VerificationPermissionRequest{ContextID: view.Context.ID, RequestKey: requestKey, Subject: row.Subject, Actions: actions}
		if err := confirmVerificationRequest(cmd, "Grant request", request, yes); err != nil {
			return err
		}
		payload, _ := json.Marshal(request)
		var receipt store.VerificationReceipt
		if err := c.verificationPermissions(http.MethodPost, args[0], nil, payload, &receipt); err != nil {
			return err
		}
		grant, err := c.verificationPermissionReceipt(args[0], view.Context.ID, receipt.ID)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(out).Encode(grant)
		}
		fmt.Fprintln(out, "Grant receipt")
		renderVerificationGrant(out, grant)
		return nil
	}}
	command.Flags().StringVar(&contextID, "context-id", "", "retained context to grant against (default: the order's latest context)")
	command.Flags().StringVar(&subjectRef, "subject", "", "kit:<kit-id>/<exercise-id> or ordinary:<obligation-id>")
	command.Flags().StringArrayVar(&actionArgs, "action", nil, "kind:binding=target, or operator_interaction:binding (repeatable)")
	command.Flags().BoolVar(&noActions, "no-actions", false, "grant an explicit empty action list")
	command.Flags().StringVar(&requestKey, "request-key", "", "stable request key; reuse it to retry an uncertain response")
	command.Flags().BoolVar(&yes, "yes", false, "issue without the interactive confirmation")
	command.Flags().BoolVar(&asJSON, "json", false, "print the receipt as JSON")
	return command
}

func verificationRevokeCmd() *cobra.Command {
	var contextID, grantID, reason, requestKey string
	var yes, asJSON bool
	command := &cobra.Command{Use: "revoke <work-order-id>", Short: "Revoke one grant and read back its revocation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(grantID) == "" || strings.TrimSpace(reason) == "" || strings.TrimSpace(requestKey) == "" {
			return fmt.Errorf("--grant-id, --reason and --request-key are required")
		}
		c := newClient()
		if contextID == "" {
			view, err := c.verificationPermissionView(args[0], "")
			if err != nil {
				return err
			}
			if view.Context == nil {
				return fmt.Errorf("no prepared verification context: %s", view.Eligibility.Recovery)
			}
			contextID = view.Context.ID
		}
		request := store.VerificationPermissionRequest{ContextID: contextID, RequestKey: requestKey, RevokeGrantID: grantID, Reason: reason}
		if err := confirmVerificationRequest(cmd, "Revocation request", request, yes); err != nil {
			return err
		}
		payload, _ := json.Marshal(request)
		if err := c.verificationPermissions(http.MethodPost, args[0], nil, payload, nil); err != nil {
			return err
		}
		grant, err := c.verificationPermissionReceipt(args[0], contextID, grantID)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(grant)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Revoked grant")
		renderVerificationGrant(cmd.OutOrStdout(), grant)
		return nil
	}}
	command.Flags().StringVar(&contextID, "context-id", "", "context that holds the grant (default: the order's latest context)")
	command.Flags().StringVar(&grantID, "grant-id", "", "grant to revoke")
	command.Flags().StringVar(&reason, "reason", "", "attributed revocation reason")
	command.Flags().StringVar(&requestKey, "request-key", "", "stable request key; reuse it to retry an uncertain response")
	command.Flags().BoolVar(&yes, "yes", false, "revoke without the interactive confirmation")
	command.Flags().BoolVar(&asJSON, "json", false, "print the grant with its revocation as JSON")
	return command
}

// parseVerificationAction reads kind:binding=target. Targets come only from
// the operator; nothing is inferred from local configuration or the manifest.
func parseVerificationAction(raw string) (core.VerificationPermission, error) {
	kind, rest, ok := strings.Cut(raw, ":")
	if !ok || kind == "" || rest == "" {
		return core.VerificationPermission{}, fmt.Errorf("--action %q: want kind:binding=target", raw)
	}
	binding, target, hasTarget := strings.Cut(rest, "=")
	if kind == "operator_interaction" {
		if hasTarget {
			return core.VerificationPermission{}, fmt.Errorf("--action %q: operator_interaction takes no target", raw)
		}
		return core.VerificationPermission{Kind: kind, Binding: binding}, nil
	}
	if !hasTarget || binding == "" || target == "" {
		return core.VerificationPermission{}, fmt.Errorf("--action %q: want kind:binding=target", raw)
	}
	return core.VerificationPermission{Kind: kind, Binding: binding, Target: target}, nil
}

func verificationSubjectRef(s core.VerificationSubject) string {
	if s.Kind == "kit" {
		return "kit:" + s.KitID + "/" + s.ExerciseID
	}
	return "ordinary:" + s.ObligationID
}

func selectVerificationSubject(view store.VerificationPermissionView, ref string) (store.VerificationPermissionSubjectView, error) {
	var found []store.VerificationPermissionSubjectView
	for _, row := range view.Subjects {
		if verificationSubjectRef(row.Subject) == ref {
			found = append(found, row)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return store.VerificationPermissionSubjectView{}, fmt.Errorf("subject %s is not in context %s; run `conveyor verification permissions inspect %s`", ref, view.Context.ID, view.WorkOrderID)
	}
	return store.VerificationPermissionSubjectView{}, fmt.Errorf("subject %s is ambiguous in context %s", ref, view.Context.ID)
}

func verificationActionsCover(actions []core.VerificationPermission, kind, binding string) bool {
	for _, a := range actions {
		if a.Kind == kind && (binding == "" || a.Binding == binding) {
			return true
		}
	}
	return false
}

func verificationBindingLabel(binding string) string {
	if binding == "" {
		return "(any name)"
	}
	return binding
}

// confirmVerificationRequest shows the exact request before the deliberate
// operator act and requires --yes or an interactive "yes". It writes to stderr
// so stdout carries only the receipt.
func confirmVerificationRequest(cmd *cobra.Command, title string, request store.VerificationPermissionRequest, yes bool) error {
	out := cmd.ErrOrStderr()
	encoded, _ := json.MarshalIndent(request, "  ", "  ")
	fmt.Fprintf(out, "%s:\n  %s\n", title, encoded)
	if yes {
		return nil
	}
	fmt.Fprint(out, "Send this request? Type yes to continue: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintln(out)
		return fmt.Errorf("not sent: confirmation declined")
	}
	return nil
}

func renderVerificationPermissionView(w io.Writer, v store.VerificationPermissionView) {
	fmt.Fprintf(w, "Work order   %s (task %s)\n", v.WorkOrderID, v.TaskID)
	fmt.Fprintf(w, "Order state  %s, attempt %s\n", v.OrderState, verificationValue(v.WorkOrderAttemptID))
	fmt.Fprintf(w, "Observed     %s\n", v.ObservedAt.Format(time.RFC3339))
	if v.LeaseExpiresAt != nil {
		fmt.Fprintf(w, "Lease        expires %s (%s after observation; the verifier renews it)\n", v.LeaseExpiresAt.Format(time.RFC3339), v.LeaseExpiresAt.Sub(v.ObservedAt).Round(time.Second))
	}
	if v.ExecutionDeadline != nil {
		fmt.Fprintf(w, "Deadline     %s (%s after observation; fixed)\n", v.ExecutionDeadline.Format(time.RFC3339), v.ExecutionDeadline.Sub(v.ObservedAt).Round(time.Second))
	}
	fmt.Fprintf(w, "Head         %s\n", v.SubmittedHead)
	if v.Context != nil {
		fmt.Fprintf(w, "Context      %s, attempt %s, sealed %t\n", v.Context.ID, v.Context.WorkOrderAttemptID, v.Context.Sealed)
		for _, r := range v.Context.Revisions {
			fmt.Fprintf(w, "Revision     %s %s @ %s\n", r.Repository, r.RemoteIdentity, r.SHA)
		}
	} else {
		fmt.Fprintln(w, "Context      none prepared")
	}
	fmt.Fprintf(w, "Eligibility  %s\n", v.Eligibility.State)
	if v.Eligibility.Reason != "" {
		fmt.Fprintf(w, "             %s\n             Recovery: %s\n", v.Eligibility.Reason, v.Eligibility.Recovery)
	}
	fmt.Fprintf(w, "\nSubjects (%d)\n", len(v.Subjects))
	for _, row := range v.Subjects {
		subject, _ := json.Marshal(row.Subject)
		fmt.Fprintf(w, "  %s\n    subject      %s\n", verificationSubjectRef(row.Subject), subject)
		if row.Description != "" {
			fmt.Fprintf(w, "    description  %s\n", strings.ReplaceAll(row.Description, "\n", "\n                 "))
		}
		fmt.Fprintf(w, "    kind         %s, retry policy %s\n", verificationValue(row.Kind), verificationValue(row.RetryPolicy))
		if len(row.Actions) == 0 {
			fmt.Fprintln(w, "    actions      none declared (grant with --no-actions)")
		}
		for _, a := range row.Actions {
			target := "target unresolved: supply it with --action"
			switch {
			case a.Kind == "operator_interaction":
				target = "no target"
			case a.Path != "":
				target = "kit path " + a.Path + "; target unresolved: supply an absolute root"
			}
			required := "optional"
			if a.Required {
				required = "required"
			}
			fmt.Fprintf(w, "    action       %s binding %s (%s; %s)\n", a.Kind, verificationBindingLabel(a.Binding), required, target)
		}
		for _, p := range row.Prerequisites {
			fmt.Fprintf(w, "    prerequisite %s %s binding %s\n", p.ID, p.Kind, verificationValue(p.EnvironmentBinding))
		}
		for _, in := range row.Inputs {
			fmt.Fprintf(w, "    input        %s (%s, required %t, sensitive %t)\n", in.Name, in.Type, in.Required, in.Sensitive)
		}
		if len(row.GrantIDs) > 0 {
			fmt.Fprintf(w, "    grants       %s\n", strings.Join(row.GrantIDs, ", "))
		}
	}
	fmt.Fprintf(w, "\nGrants (%d)\n", len(v.Grants))
	for _, g := range v.Grants {
		renderVerificationGrant(w, g)
	}
}

func renderVerificationGrant(w io.Writer, g store.VerificationPermissionGrantView) {
	fmt.Fprintf(w, "  %s\n    request key  %s\n    actor        %s at %s\n    subject      %s\n    attempt      %s\n", g.ID, g.RequestKey, g.Actor, g.CreatedAt.Format(time.RFC3339), verificationSubjectRef(g.Subject), g.WorkOrderAttemptID)
	if len(g.Actions) == 0 {
		fmt.Fprintln(w, "    actions      none")
	}
	for _, a := range g.Actions {
		fmt.Fprintf(w, "    action       %s binding %s target %s\n", a.Kind, a.Binding, verificationValue(a.Target))
	}
	for _, r := range g.Revisions {
		fmt.Fprintf(w, "    revision     %s @ %s\n", r.Repository, r.SHA)
	}
	if g.Revocation != nil {
		fmt.Fprintf(w, "    revoked      by %s at %s: %s\n", g.Revocation.Actor, g.Revocation.CreatedAt.Format(time.RFC3339), g.Revocation.Reason)
	}
}

func verificationValue(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
