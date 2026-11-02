package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestClaimSessionAndActivityReadsAtScalePostgresIntegration runs the claim
// session and whole-workspace activity reads over 200 orders that each pin a
// 64 KiB governance snapshot. One set-based INSERT copies a queued template
// order 200 times and one set-based UPDATE pins both snapshots on every order
// of the workspace. The store's read ledger bounds the bytes each read
// receives (component-work-orders, Snapshot-free fold reads;
// component-persistence, Database read accounting).
func TestClaimSessionAndActivityReadsAtScalePostgresIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	root := store.WithActor(t.Context(), store.SystemActor())
	st, err := Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "claim-session-scale-" + core.NewTaskID()
	ctx := store.WithWorkspace(root, workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	storetest.RunClaimSessionAndActivityReadsAtScale(t, storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg}, storetest.ClaimSessionScale{
		Seed: func(t *testing.T, ctx context.Context, template core.WorkOrder, count int, governance, served []byte) {
			t.Helper()
			rows, err := st.Pool().Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'work_orders' AND is_generated = 'NEVER' ORDER BY ordinal_position`)
			if err != nil {
				t.Fatal(err)
			}
			var columns, values []string
			for rows.Next() {
				var column string
				if err = rows.Scan(&column); err != nil {
					t.Fatal(err)
				}
				columns = append(columns, column)
				if column == "id" {
					values = append(values, "template.id || '-copy-' || copy.n")
				} else {
					values = append(values, "template."+column)
				}
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			tag, err := st.Pool().Exec(ctx, `INSERT INTO work_orders (`+strings.Join(columns, ", ")+`) SELECT `+strings.Join(values, ", ")+
				` FROM work_orders template CROSS JOIN generate_series(1, $3::int) AS copy(n) WHERE template.workspace_id = $1 AND template.id = $2`,
				workspace, template.ID, count)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != int64(count) {
				t.Fatalf("copied %d orders, want %d", tag.RowsAffected(), count)
			}
			if tag, err = st.Pool().Exec(ctx, `UPDATE work_orders SET governance_snapshot = $2::jsonb, served_requirement_snapshot = $3::jsonb WHERE workspace_id = $1`, workspace, string(governance), string(served)); err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() < int64(count+1) {
				t.Fatalf("pinned snapshots on %d orders, want at least %d", tag.RowsAffected(), count+1)
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
