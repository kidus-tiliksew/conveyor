package postgres

import (
	"net/url"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestPhase62RequirementConformanceIntegration runs the requirement and
// planning-session conformance suite against Postgres (spec §4.2 item 1 AC-1,
// §9 AC-2). Its in-memory twin is TestMemoryRequirementConformance: the two
// together are the guarantee that requirement versioning, confirmation, and
// planning transcripts behave identically whichever store a deployment uses.
func TestPhase62RequirementConformanceIntegration(t *testing.T) {
	databaseURL, err := url.Parse(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	query := databaseURL.Query()
	query.Set("pool_max_conns", "1")
	databaseURL.RawQuery = query.Encode()
	st, err := Open(t.Context(), databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	storetest.RunRequirementConformance(t, func(t *testing.T, repos []config.Repo) storetest.RequirementFixture {
		t.Helper()
		// One fresh workspace per subtest so document slugs, session ids, and
		// listing order are never contaminated by an earlier case.
		workspace := "phase62-" + core.NewTaskID()
		ctx := store.WithWorkspace(t.Context(), workspace)
		if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: repos}); err != nil {
			t.Fatal(err)
		}
		return storetest.RequirementFixture{Store: st, Context: ctx, Workspace: workspace}
	})
}
