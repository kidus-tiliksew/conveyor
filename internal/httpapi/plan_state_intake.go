package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	// invalidPlanIntake answers each 400 of component-plan-state invariant 2.
	invalidPlanIntake = "invalid_plan_intake"
	// planApprovalForbidden answers plan_approved true from a credential whose
	// owner lacks operate_gates (component-plan-state invariant 2;
	// component-http-api invariant 12).
	planApprovalForbidden = "plan_approval_forbidden"
)

// intakePlanFieldNames are the intake plan fields of component-plan-state's
// public surfaces, in the order a refusal names them.
var intakePlanFieldNames = []string{"plan_state", "plan", "plan_base_commit", "plan_approved"}

// intakePlanInput carries the plan fields a request supplied. A field a JSON
// body supplied with the wrong type, null included, is kept as a refusal so
// the route answers it before the idempotency lookup.
type intakePlanInput struct {
	PlanState      *string
	Plan           *string
	PlanBaseCommit *string
	PlanApproved   *bool
	typeErr        string
}

// supplied reports whether the request carried any intake plan field.
func (p intakePlanInput) supplied() bool {
	return p.PlanState != nil || p.Plan != nil || p.PlanBaseCommit != nil || p.PlanApproved != nil || p.typeErr != ""
}

// decodeIntakePlanFields reads the plan fields from a task request object.
// Field names match case-insensitively, as encoding/json matches the other
// intake fields.
func decodeIntakePlanFields(fields map[string]json.RawMessage) intakePlanInput {
	var input intakePlanInput
	for name, raw := range fields {
		field := strings.ToLower(name)
		switch field {
		case "plan_state", "plan", "plan_base_commit":
			var value *string
			if err := json.Unmarshal(raw, &value); err != nil || value == nil {
				input.recordTypeErr(field + " must be a string")
				continue
			}
			switch field {
			case "plan_state":
				input.PlanState = value
			case "plan":
				input.Plan = value
			default:
				input.PlanBaseCommit = value
			}
		case "plan_approved":
			var value *bool
			if err := json.Unmarshal(raw, &value); err != nil || value == nil {
				input.recordTypeErr("plan_approved must be a boolean")
				continue
			}
			input.PlanApproved = value
		}
	}
	return input
}

func (p *intakePlanInput) recordTypeErr(message string) {
	if p.typeErr == "" {
		p.typeErr = message
	}
}

func planIntakeError(format string, args ...any) *taskCreateError {
	return &taskCreateError{Status: http.StatusBadRequest, Code: invalidPlanIntake, Message: fmt.Sprintf(format, args...)}
}

// validateIntakePlan applies component-plan-state invariant 2's refusals, each
// answering 400 with nothing recorded, and returns the plan-state request
// that ResolveIntakePlanState resolves together with any intake plan
// (req-task-plan-state AC-2.2, AC-2.5, AC-2.6; DEC-122(6)).
func validateIntakePlan(req createTaskReq) (store.PlanStateRequest, *core.IntakePlan, *taskCreateError) {
	input := req.plan
	request := store.PlanStateRequest{SpecApproval: req.SpecApproval, Level: req.Level}
	if input.typeErr != "" {
		return request, nil, planIntakeError("%s", input.typeErr)
	}
	if input.PlanState != nil {
		state := core.PlanState(strings.TrimSpace(*input.PlanState))
		if state != core.PlanNeeded && state != core.PlanNotRequired {
			return request, nil, planIntakeError("plan_state must be %s or %s at intake; file a plan to reach %s", core.PlanNeeded, core.PlanNotRequired, core.PlanAwaitingApproval)
		}
		request.PlanState = &state
	}
	if input.Plan == nil {
		if input.PlanBaseCommit != nil {
			return request, nil, planIntakeError("plan_base_commit requires plan")
		}
		if input.PlanApproved != nil {
			return request, nil, planIntakeError("plan_approved requires plan")
		}
		if request.PlanState != nil && req.SpecApproval != nil && store.PlanStateFromSpecApproval(*req.SpecApproval) != *request.PlanState {
			return request, nil, planIntakeError("spec_approval %t disagrees with plan_state %s; send only plan_state", *req.SpecApproval, *request.PlanState)
		}
		return request, nil, nil
	}
	if request.PlanState != nil {
		return request, nil, planIntakeError("plan_state must be omitted with plan; a filed plan sets the plan state")
	}
	if req.SpecApproval != nil && !*req.SpecApproval {
		return request, nil, planIntakeError("spec_approval false disagrees with plan; a filed plan needs approval")
	}
	if _, err := pipeline.ParsePlan(*input.Plan, nil); err != nil {
		return request, nil, planIntakeError("invalid plan: %v", err)
	}
	plan := &core.IntakePlan{Content: strings.TrimSpace(*input.Plan)}
	if input.PlanBaseCommit != nil {
		if !core.ValidPlanBaseCommit(*input.PlanBaseCommit) {
			return request, nil, planIntakeError("plan_base_commit must be a full commit ID of 40 lowercase hex characters")
		}
		plan.BaseCommit = *input.PlanBaseCommit
	}
	if input.PlanApproved != nil {
		plan.Approved = *input.PlanApproved
	}
	request.HasPlan, request.PlanApproved = true, plan.Approved
	return request, plan, nil
}

// authorizeIntakePlanApproval judges plan_approved true after admission, from
// the same live binding the route admitted, against operate_gates. A caller
// lacking it gets 403 with nothing recorded; the route has already admitted
// the caller to the workspace, so the answer reveals no existence
// (component-http-api invariant 12; component-plan-state invariant 2;
// req-task-plan-state AC-2.3).
func (s *Server) authorizeIntakePlanApproval(ctx context.Context) error {
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser {
		return &taskCreateError{Status: http.StatusForbidden, Code: planApprovalForbidden, Message: "plan_approved true requires a person's credential holding operate_gates"}
	}
	allowed := true
	var err error
	if workspaceID, scoped := store.WorkspaceFromContext(ctx); scoped && s.Workspaces != nil {
		if s.Memberships == nil {
			allowed = false
		} else {
			allowed, err = s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspaceID, core.CapabilityOperateGates)
		}
	} else if s.Credentials != nil && s.Memberships != nil {
		allowed, err = s.Memberships.AuthorizeDeployment(ctx, credential.OwnerUserID, core.CapabilityOperateGates)
	} else {
		// Memory-only deployments admit the configured operator token, as
		// requireMutationCapability does.
		allowed = credential.Scope == core.CredentialScopeOperator
	}
	if err != nil {
		log.Printf("httpapi: authorize intake plan approval: request_id=%q error=%q", middleware.GetReqID(ctx), redactedErrorLogText(err))
		return err
	}
	if !allowed {
		return &taskCreateError{Status: http.StatusForbidden, Code: planApprovalForbidden, Message: "plan_approved true requires the operate_gates capability"}
	}
	return nil
}

// sameIntakePlan compares a keyed retry's plan fields with the values the
// task was created with: the plan state task.created recorded and plan
// version 1 when intake filed one (component-task-lifecycle invariant 17).
func sameIntakePlan(req createTaskReq, intake taskIntakeRecord) bool {
	resolved, plan, refusal := validateIntakePlan(req)
	if refusal != nil {
		return false
	}
	if plan == nil {
		if intake.PlanVersion != nil {
			return false
		}
		if resolved.PlanState == nil && resolved.SpecApproval == nil && resolved.Level == "" {
			return true
		}
		return intake.PlanState == store.ResolveIntakePlanState(resolved, "")
	}
	if intake.PlanVersion == nil || intake.PlanVersion.Origin != core.PlanOriginIntake {
		return false
	}
	baseCommit := ""
	if intake.PlanVersion.BaseCommit != nil {
		baseCommit = *intake.PlanVersion.BaseCommit
	}
	return intake.PlanVersion.Content == plan.Content && baseCommit == plan.BaseCommit &&
		intake.PlanState == store.ResolveIntakePlanState(resolved, "")
}
