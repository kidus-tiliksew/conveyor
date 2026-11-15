package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// submitRiskRatingTool serves the MCP tool submit_risk_rating. It completes
// the risk order that the authenticated claimant's own session holds with a
// tier and a one-line reason; the subject, the paths, the rules, and the model
// come from the stored order and its claim, never from the call
// (req-risk-based-review AC-12.2; component-work-orders invariant 65). A
// missing, mistyped, or invalid tier or reason is refused before any write,
// so the order stays claimed. The answer carries the rating in its served
// shape.
func (s *Server) submitRiskRatingTool(ctx context.Context, args map[string]any, session string) (any, error) {
	workOrderID, _ := args["work_order_id"].(string)
	claim, err := s.authorizeClaimantSession(ctx, workOrderID, session)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, field := range []string{"tier", "reason"} {
		value, supplied := args[field]
		if !supplied {
			return nil, invalidToolArgument(fmt.Errorf("%s is required", field))
		}
		text, ok := value.(string)
		if !ok {
			return nil, invalidToolArgument(fmt.Errorf("%s must be a string", field))
		}
		values[field] = text
	}
	if _, err = riskpolicy.ParseTier(values["tier"]); err != nil {
		return nil, invalidToolArgument(errors.New("tier must be low, medium, or high"))
	}
	if err = core.ValidateRiskReason("reason", values["reason"]); err != nil {
		return nil, invalidToolArgument(err)
	}
	result, err := s.WorkOrders.SubmitRiskRating(ctx, claim, workOrderID, values["tier"], values["reason"])
	if err != nil {
		return nil, err
	}
	// The answer serves the rating with counts in place of its path lists,
	// for the first call and for a replay alike; the stored rating keeps
	// both lists (component-work-orders invariant 65; component-risk-review,
	// Public surfaces).
	if rating, ok := result["rating"].(core.RiskRating); ok {
		result["rating"] = core.ServeRiskRating(rating)
	}
	return result, nil
}
