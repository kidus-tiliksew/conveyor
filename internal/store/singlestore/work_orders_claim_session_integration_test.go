package singlestore

import (
	"context"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// claimSessionDigits numbers 0 through 999 from three cross-joined digit
// lists, because SingleStore has no generate_series.
const claimSessionDigits = `(SELECT 0 AS d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)`

// TestClaimSessionAndActivityReadsAtScaleSingleStoreIntegration runs the claim
// session and whole-workspace activity reads over 200 orders that each pin a
// 64 KiB governance snapshot. One set-based INSERT copies a queued template
// order 200 times and one set-based UPDATE pins both snapshots on every order
// of the workspace. The private pool's read ledger bounds the bytes each read
// receives (component-work-orders, Snapshot-free fold reads;
// component-persistence, Database read accounting).
func TestClaimSessionAndActivityReadsAtScaleSingleStoreIntegration(t *testing.T) {
	st := openOwnedStore(t)
	workspace := "claim-session-scale-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunClaimSessionAndActivityReadsAtScale(t, storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg}, storetest.ClaimSessionScale{
		Seed: func(t *testing.T, ctx context.Context, template core.WorkOrder, count int, governance, served []byte) {
			t.Helper()
			rows, err := st.db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'work_orders' ORDER BY ordinal_position`)
			if err != nil {
				t.Fatal(err)
			}
			var columns, values []string
			for rows.Next() {
				var column string
				if err = rows.Scan(&column); err != nil {
					t.Fatal(err)
				}
				columns = append(columns, "`"+column+"`")
				if column == "id" {
					values = append(values, "CONCAT(src.id, '-copy-', seq.n)")
				} else {
					values = append(values, "src.`"+column+"`")
				}
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			numbers := `(SELECT a.d * 100 + b.d * 10 + c.d AS n FROM ` + claimSessionDigits + ` a CROSS JOIN ` + claimSessionDigits + ` b CROSS JOIN ` + claimSessionDigits + ` c)`
			result, err := st.db.ExecContext(ctx, `INSERT INTO work_orders (`+strings.Join(columns, ", ")+`) SELECT `+strings.Join(values, ", ")+
				` FROM work_orders src CROSS JOIN `+numbers+` seq WHERE src.workspace_id = ? AND src.id = ? AND seq.n BETWEEN 1 AND ?`,
				workspace, template.ID, count)
			if err != nil {
				t.Fatal(err)
			}
			if copied, err := result.RowsAffected(); err != nil || copied != int64(count) {
				t.Fatalf("copied %d orders (%v), want %d", copied, err, count)
			}
			if result, err = st.db.ExecContext(ctx, `UPDATE work_orders SET governance_snapshot = ?, served_requirement_snapshot = ? WHERE workspace_id = ?`, string(governance), string(served), workspace); err != nil {
				t.Fatal(err)
			}
			if pinned, err := result.RowsAffected(); err != nil || pinned < int64(count+1) {
				t.Fatalf("pinned snapshots on %d orders (%v), want at least %d", pinned, err, count+1)
			}
		},
		DrainReadBytes: func() map[string]uint64 {
			bytes := map[string]uint64{}
			for _, total := range st.ReadStats(1 << 16).Top {
				bytes[total.Label] = total.Bytes
			}
			return bytes
		},
	})
}
