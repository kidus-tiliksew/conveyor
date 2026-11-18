package postgres

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func init() {
	readEgressCases.Register("risk-ratings-and-orders-batch", buildRiskRatingsAndOrdersBatch)
	readEgressCases.Register("work-order-clock-lifecycle", buildWorkOrderClockLifecycle)
}

func seedStoredStaleForEgress(t testing.TB, ctx context.Context, st *Store, orderID string) {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	tag, err := st.pool.Exec(ctx, `UPDATE work_orders SET state='stale' WHERE workspace_id=$1 AND id=$2`, ws, orderID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("seed stored stale rows=%d err=%v", tag.RowsAffected(), err)
	}
}

func foldRiskBatch(ctx context.Context, st *Store, ids []string, perTask bool) (storetest.ReadEgressOutcome, error) {
	ws, _ := store.WorkspaceFromContext(ctx)
	ratings := map[string][]core.RiskRating{}
	orders := map[string][]core.RiskOrder{}
	if perTask {
		for _, id := range ids {
			listed, err := st.ListTaskRiskRatings(ctx, id)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			ratings[id] = listed
			listedOrders, err := st.ListTaskRiskOrders(ctx, id)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			orders[id] = listedOrders
		}
	} else {
		var err error
		ratings, err = st.ListRiskRatingsForTasks(ctx, ids)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		orders, err = st.ListRiskOrdersForTasks(ctx, ws, ids)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
	}
	folded := storetest.ReduceReadEgressRiskBatch(ids, ratings, orders)
	digest, err := storetest.ReadEgressDigest(folded)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(folded)}, err
}

func buildRiskRatingsAndOrdersBatch(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	selected, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_RISK_TASKS", 8)
	if err != nil {
		t.Fatal(err)
	}
	history, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_RISK_HISTORY_RATINGS", 80)
	if err != nil {
		t.Fatal(err)
	}
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_RISK_NOISE_TASKS", 40)
	if err != nil {
		t.Fatal(err)
	}
	reasonBytes, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_RISK_REASON_BYTES", 512)
	if err != nil {
		t.Fatal(err)
	}
	ids, fixture := storetest.SeedReadEgressRiskBatch(t, ctx, st, selected, history, noise, reasonBytes)
	return storetest.ReadEgressCase{Name: "risk-ratings-and-orders-batch", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return foldRiskBatch(ctx, st, ids, true)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return foldRiskBatch(ctx, st, ids, false)
		}}
}

func foldClockOrders(ctx context.Context, st *Store, ids []string, snapshot bool) (storetest.ReadEgressOutcome, error) {
	var orders []core.WorkOrder
	if snapshot {
		for _, id := range ids {
			listed, err := st.ListTaskWorkOrdersSnapshot(ctx, id)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			orders = append(orders, listed...)
		}
	} else {
		for _, id := range ids {
			listed, err := st.ListTaskWorkOrders(ctx, id)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			orders = append(orders, listed...)
		}
	}
	folded := storetest.ReduceReadEgressClockOrders(orders)
	digest, err := storetest.ReadEgressDigest(folded)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(folded)}, err
}

func buildWorkOrderClockLifecycle(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_CLOCK_NOISE_ORDERS", 8)
	if err != nil {
		t.Fatal(err)
	}
	pinBytes, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_CLOCK_PIN_BYTES", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	ids, fixture := storetest.SeedReadEgressClockLifecycle(t, ctx, st, noise, pinBytes, func(t testing.TB, ctx context.Context, orderID string) {
		seedStoredStaleForEgress(t, ctx, st, orderID)
	})
	return storetest.ReadEgressCase{Name: "work-order-clock-lifecycle", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return foldClockOrders(ctx, st, ids, true)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return foldClockOrders(ctx, st, ids, false)
		}}
}
