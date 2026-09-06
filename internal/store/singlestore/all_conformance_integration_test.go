package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
	if raw == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	cfg, err := connectionConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("SingleStore integration database must end in _test")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	t.Cleanup(func() { admin.Close() })
	name := fmt.Sprintf("conveyor_%d_test", time.Now().UnixNano())
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Errorf("drop isolated test database: %v", err)
		}
	})
	cfg.DBName = name
	st, err := Open(t.Context(), cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}
func TestSingleStoreConformanceIntegration(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{ProductionCapable: false,
		Skip: []string{
			"ProjectionReads",
			"CommandRefusals",
			"ApprovalRefresh",
			"PlanningReads",
			"Blueprint",
			"Requirements",
			"VersionDismissal",
			"Lineage",
			"CheckpointDecisionRequests",
			"CheckpointRenewal",
			"ForgeAuthorIdentity",
			"PlanRevision",
			"ReferenceDocuments",
			"SystemDesignProposals",
			"PlanningBundles",
			"SystemDesignDrift",
			"TaskContextRepin",
			"DependencyAddition",
			"TaskOperationsPagination",
			"TaskLifecycle",
			"TaskEventAtomicity",
			"Workers",
			"WorkOrders",
			"WorkOrderClocks",
			"ReviewRounds",
			"ReviewAcceptance",
			"Decisions",
			"ArchiveRestore",
			"Monitor",
			"TaskFilter",
			"TaskAssigneeMembership",
			"Identity",
			"Membership",
			"InvitationSessions",
			"Tokens",
		},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			st := integrationStore(t)
			ws := "conformance-" + core.NewTaskID()
			ctx := store.WithWorkspace(t.Context(), ws)
			cfg := &config.Config{Workspace: ws, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: ws, Config: cfg}
		}})
}
