package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ReviewRoundHeldByRiskOrder reports whether an enforcing task's review round
// of head waits for that head's risk order: one that is queued or claimed,
// including the stale and timed-out forms the order clock recovers. It reads
// no risk order for a task whose frozen mode is not enforce
// (feature-risk-based-review RR-12; req-risk-based-review AC-1.6, AC-5.10;
// component-risk-review invariant 13). The round-building command repeats the
// check in its transaction.
func ReviewRoundHeldByRiskOrder(ctx context.Context, st store.RiskOrderStore, task core.Task, head string) (bool, error) {
	if !store.RiskRoundHoldApplies(task) || strings.TrimSpace(head) == "" {
		return false, nil
	}
	orders, err := st.ListTaskRiskOrders(ctx, task.ID)
	if err != nil {
		return false, fmt.Errorf("read risk orders: %w", err)
	}
	_, held := store.ReviewRoundRiskHold(task, orders, head)
	return held, nil
}

// HeadRiskOrderRequest builds the risk order request of a submitted or
// refreshed head: the head with its observed base commit, its complete
// changed paths, and the rules its path rating matched. It returns nil when
// the task's frozen mode is off, or when the head or its base commit is
// unknown: a head without its base has the path minimum high, so it needs no
// risk order (req-risk-based-review AC-5.4, AC-12.3).
func HeadRiskOrderRequest(ctx context.Context, st store.RiskRatingStore, cfg *config.Config, task core.Task, head, base string, paths []string) (*store.HeadRiskOrderRequest, error) {
	head, base = strings.TrimSpace(head), strings.TrimSpace(base)
	if !task.SetupContract.Risk.Enabled() || head == "" || base == "" {
		return nil, nil
	}
	ratings, err := st.ListTaskRiskRatings(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("read risk ratings: %w", err)
	}
	var rules []riskpolicy.RuleMatch
	key := core.PathRiskKey(head)
	for _, rating := range ratings {
		if rating.Key == key {
			rules = rating.MatchedRules
			break
		}
	}
	queueTimeout := config.DefaultWorkOrderQueueTimeout
	if cfg != nil && cfg.WorkOrderQueueTimeout > 0 {
		queueTimeout = cfg.WorkOrderQueueTimeout
	}
	return &store.HeadRiskOrderRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: base},
		ChangedPaths: slices.Clone(paths), MatchedRules: rules, QueueTimeout: queueTimeout}, nil
}

// headPathRiskHigh reports whether the task's tier is high, so an unread
// changed-path list cannot create a risk order.
func headPathRiskHigh(ctx context.Context, st store.RiskRatingStore, taskID string) (bool, error) {
	ratings, err := st.ListTaskRiskRatings(ctx, taskID)
	if err != nil {
		return false, fmt.Errorf("read risk ratings: %w", err)
	}
	return store.RiskTierReachesHigh(ratings), nil
}

// ensureRefreshHeadRiskOrder creates a refresh head's risk order, and cancels
// open risk orders of earlier heads, before the refresh marks its approval
// stale and dispatches review, so no round of that head opens before its risk
// order exists. The subject is the observed head and base commit, never a
// branch name. A replay returns the head's order and writes nothing
// (feature-risk-based-review RR-12; req-risk-based-review AC-5.8, AC-5.9,
// AC-5.11).
func (d *Dispatcher) ensureRefreshHeadRiskOrder(ctx context.Context, cfg *config.Config, task core.Task, observed github.PullRequest, rated refreshPathRead) error {
	if !task.SetupContract.Risk.Enabled() || observed.HeadSHA == "" || observed.BaseSHA == "" {
		return nil
	}
	paths := rated.paths
	if !rated.read {
		// The head was rated before this refresh, so its changed paths are
		// read again. A head whose tier is high needs no risk order, so a
		// failed read there only cancels earlier heads' orders; any other
		// failed read retries the refresh rather than create an order that
		// covers no path (component-risk-review invariant 7).
		read, err := d.refreshChangedPaths(ctx, cfg, task, observed)
		if err != nil {
			high, highErr := headPathRiskHigh(ctx, d.Store, task.ID)
			if highErr != nil {
				return highErr
			}
			if !high {
				return fmt.Errorf("read refresh head %s changed paths for its risk order: %w", observed.HeadSHA, err)
			}
		}
		paths = read
	}
	request, err := HeadRiskOrderRequest(ctx, d.Store, cfg, task, observed.HeadSHA, observed.BaseSHA, paths)
	if err != nil || request == nil {
		return err
	}
	_, err = taskops.ExecuteWorkOrder(ctx, d.Store, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (store.HeadRiskOrderResult, error) {
		return d.Store.EnsureHeadRiskOrderCommand(ctx, lease, *request)
	})
	return err
}
