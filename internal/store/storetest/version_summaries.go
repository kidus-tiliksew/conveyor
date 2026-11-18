package storetest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// versionSummaryHistory is how many large historical versions each primary
// fixture seeds. Their content is baseline-equivalent to the full read only
// after the summary read drops it.
const versionSummaryHistory = 100

// runDocumentVersionSummaries proves the current and pending version summary
// reads on every backend: each equals the same backend's per-document version
// read for its selection, deduplicates its input, skips missing and
// other-workspace IDs, returns no unrequested document, and keeps historical
// requirement content empty while preserving its metadata and statements
// (component-persistence; DEC-38).
func runDocumentVersionSummaries(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	other := ws + "-version-summaries"
	otherCtx := store.WithActor(store.WithWorkspace(context.Background(), other), store.SystemActor())
	if _, err := st.CreateWorkspace(otherCtx, other, "Version summaries", &config.Config{Workspace: other, Repos: requirementConformanceRepos}); err != nil {
		t.Fatal(err)
	}

	statements := func(name string) []core.RequirementStatement {
		return []core.RequirementStatement{{ID: "REQ-1", Statement: "The summary read stays bounded for " + name + "."}}
	}
	requirementVersion := func(id string, version int) core.RequirementVersion {
		return core.RequirementVersion{
			RequirementID:   id,
			Content:         fmt.Sprintf("# %s\n\n%s-v%d %s", id, id, version, strings.Repeat("c", 32768)),
			Statements:      statements(id),
			Origin:          core.RequirementOriginChat,
			OriginSessionID: fmt.Sprintf("session-%s-%d", id, version),
		}
	}
	designVersion := func(id string, version int) core.SystemDesignVersion {
		return core.SystemDesignVersion{
			DocumentID: id,
			Content:    designContent(fmt.Sprintf("%s-v%d %s", id, version, strings.Repeat("d", 32768))),
			Origin:     core.SystemDesignOriginOperator,
		}
	}

	// seedRequirement leaves one historical chat-origin version per proposal,
	// its confirmed current version, a pending proposal, and a dismissed
	// proposal. Confirmation retires each earlier unconfirmed proposal.
	seedRequirement := func(ctx context.Context, id string, history int) {
		t.Helper()
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, requirementVersion(id, 1))
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version)
		requireOK(t, err)
		for n := 2; n <= history; n++ {
			proposal, proposeErr := st.ProposeRequirementVersion(ctx, requirementVersion(id, n))
			requireOK(t, proposeErr)
			_, _, confirmErr := st.ConfirmRequirementVersion(ctx, id, proposal.Version)
			requireOK(t, confirmErr)
		}
		_, err = st.ProposeRequirementVersion(ctx, requirementVersion(id, history+1))
		requireOK(t, err)
		dismissed, err := st.ProposeRequirementVersion(ctx, requirementVersion(id, history+2))
		requireOK(t, err)
		_, _, err = st.DismissRequirementVersion(ctx, id, dismissed.Version)
		requireOK(t, err)
	}

	// seedDesign leaves one historical version per proposal, its confirmed
	// current version, a pending proposal, and a dismissed proposal.
	seedDesign := func(ctx context.Context, id string, history int) {
		t.Helper()
		_, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Component design"}, designVersion(id, 1))
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, id, version.Version)
		requireOK(t, err)
		for n := 2; n <= history; n++ {
			proposal, proposeErr := st.ProposeSystemDesignVersion(ctx, designVersion(id, n))
			requireOK(t, proposeErr)
			_, _, confirmErr := st.ConfirmSystemDesignVersion(ctx, id, proposal.Version)
			requireOK(t, confirmErr)
		}
		_, err = st.ProposeSystemDesignVersion(ctx, designVersion(id, history+1))
		requireOK(t, err)
		dismissed, err := st.ProposeSystemDesignVersion(ctx, designVersion(id, history+2))
		requireOK(t, err)
		_, _, err = st.DismissSystemDesignVersion(ctx, id, dismissed.Version)
		requireOK(t, err)
	}

	requirementID := "req-versions-" + core.NewTaskID()
	otherRequirementID := "req-versions-foreign-" + core.NewTaskID()
	designID := "design-versions-" + core.NewTaskID()
	otherDesignID := "design-versions-foreign-" + core.NewTaskID()
	seedRequirement(ctx, requirementID, versionSummaryHistory)
	seedRequirement(otherCtx, otherRequirementID, 1)
	seedDesign(ctx, designID, versionSummaryHistory)
	seedDesign(otherCtx, otherDesignID, 1)

	missingRequirement := "missing-" + core.NewTaskID()
	missingDesign := "missing-" + core.NewTaskID()

	// listedRequirements and listedDesigns pair each ID with the current
	// version the workspace's row read names, as the list routes do. An ID
	// the row read does not list pairs with version 0.
	listedRequirements := func(t *testing.T, ids ...string) []store.DocumentVersionSelection {
		t.Helper()
		rows, err := st.ListRequirements(ctx, true)
		requireOK(t, err)
		current := map[string]int{}
		for _, row := range rows {
			current[row.ID] = row.CurrentVersion
		}
		out := make([]store.DocumentVersionSelection, 0, len(ids))
		for _, id := range ids {
			out = append(out, store.DocumentVersionSelection{ID: id, CurrentVersion: current[id]})
		}
		return out
	}
	listedDesigns := func(t *testing.T, ids ...string) []store.DocumentVersionSelection {
		t.Helper()
		rows, err := st.ListSystemDesigns(ctx, true)
		requireOK(t, err)
		current := map[string]int{}
		for _, row := range rows {
			current[row.ID] = row.CurrentVersion
		}
		out := make([]store.DocumentVersionSelection, 0, len(ids))
		for _, id := range ids {
			out = append(out, store.DocumentVersionSelection{ID: id, CurrentVersion: current[id]})
		}
		return out
	}

	t.Run("ReadsAreBounded", func(t *testing.T) {
		if x.DrainReadBytes == nil || x.DrainReadQueries == nil || x.DrainReadRows == nil {
			return
		}
		requirementSelection := listedRequirements(t, requirementID)
		designSelection := listedDesigns(t, designID)
		x.DrainReadBytes()
		x.DrainReadQueries()
		x.DrainReadRows()
		_, err := st.ListRequirementVersionSummaries(ctx, requirementSelection)
		requireOK(t, err)
		fullRequirements, err := st.ListRequirementVersions(ctx, requirementID)
		requireOK(t, err)
		requirementQueries, requirementRows, requirementBytes := x.DrainReadQueries(), x.DrainReadRows(), x.DrainReadBytes()
		if len(fullRequirements) != versionSummaryHistory+2 {
			t.Fatalf("full requirement read returned %d versions, want %d", len(fullRequirements), versionSummaryHistory+2)
		}
		if requirementQueries["ListRequirementVersionSummaries"] != 1 {
			t.Fatalf("summary statements = %d, want 1", requirementQueries["ListRequirementVersionSummaries"])
		}
		if requirementRows["ListRequirementVersionSummaries"] != uint64(versionSummaryHistory+2) {
			t.Fatalf("summary rows = %d, want %d", requirementRows["ListRequirementVersionSummaries"], versionSummaryHistory+2)
		}
		if requirementBytes["ListRequirementVersionSummaries"]*8 >= requirementBytes["ListRequirementVersions"] {
			t.Fatalf("summary bytes = %d against full-read bytes = %d, want the historical content excluded", requirementBytes["ListRequirementVersionSummaries"], requirementBytes["ListRequirementVersions"])
		}

		x.DrainReadBytes()
		x.DrainReadQueries()
		x.DrainReadRows()
		_, err = st.ListSystemDesignVersionSummaries(ctx, designSelection)
		requireOK(t, err)
		fullDesigns, err := st.ListSystemDesignVersions(ctx, designID)
		requireOK(t, err)
		designQueries, designRows, designBytes := x.DrainReadQueries(), x.DrainReadRows(), x.DrainReadBytes()
		if len(fullDesigns) != versionSummaryHistory+2 {
			t.Fatalf("full design read returned %d versions, want %d", len(fullDesigns), versionSummaryHistory+2)
		}
		if designQueries["ListSystemDesignVersionSummaries"] != 1 {
			t.Fatalf("design summary statements = %d, want 1", designQueries["ListSystemDesignVersionSummaries"])
		}
		if designRows["ListSystemDesignVersionSummaries"] != 2 {
			t.Fatalf("design summary rows = %d, want the current and pending rows", designRows["ListSystemDesignVersionSummaries"])
		}
		if designBytes["ListSystemDesignVersionSummaries"] >= 256<<10 {
			t.Fatalf("design summary bytes = %d, want only the current and pending content", designBytes["ListSystemDesignVersionSummaries"])
		}
	})

	t.Run("RequirementsEqualFullVersionReads", func(t *testing.T) {
		requested := listedRequirements(t, requirementID, missingRequirement, otherRequirementID, requirementID)
		got, err := st.ListRequirementVersionSummaries(ctx, requested)
		requireOK(t, err)
		if _, ok := got[otherRequirementID]; ok {
			t.Fatalf("another workspace's requirement returned: %+v", got[otherRequirementID])
		}
		if _, ok := got[missingRequirement]; ok {
			t.Fatalf("a missing requirement returned: %+v", got[missingRequirement])
		}
		if len(got) != 1 {
			t.Fatalf("summaries returned %d requirements, want 1", len(got))
		}
		requirement, err := st.GetRequirement(ctx, requirementID)
		requireOK(t, err)
		full, err := st.ListRequirementVersions(ctx, requirementID)
		requireOK(t, err)
		want := make([]core.RequirementVersion, 0, len(full))
		withContent := 0
		for _, version := range full {
			historical := version.Version != requirement.CurrentVersion && (version.Confirmed || version.Retired)
			if historical {
				version.Content = ""
			} else {
				withContent++
			}
			want = append(want, version)
		}
		if !reflect.DeepEqual(got[requirementID], want) {
			t.Fatalf("summaries = %+v\nwant %+v", got[requirementID], want)
		}
		if len(want) != versionSummaryHistory+2 {
			t.Fatalf("summaries returned %d versions, want %d", len(want), versionSummaryHistory+2)
		}
		if withContent != 2 {
			t.Fatalf("summaries returned %d content rows, want only the current and pending rows", withContent)
		}
		historical := want[0]
		if historical.Content != "" || historical.OriginSessionID != "session-"+requirementID+"-1" || len(historical.Statements) == 0 {
			t.Fatalf("historical metadata was dropped: %+v", historical)
		}
		for _, version := range want[1:] {
			if version.Version == requirement.CurrentVersion || (!version.Confirmed && !version.Retired) {
				if version.Content == "" {
					t.Fatalf("current or pending content was dropped: %+v", version)
				}
			} else if version.Content != "" {
				t.Fatalf("historical content was transferred: %+v", version)
			}
		}
	})

	t.Run("SystemDesignsEqualCurrentAndPendingReads", func(t *testing.T) {
		requested := listedDesigns(t, designID, missingDesign, otherDesignID, designID)
		got, err := st.ListSystemDesignVersionSummaries(ctx, requested)
		requireOK(t, err)
		if _, ok := got[otherDesignID]; ok {
			t.Fatalf("another workspace's design returned: %+v", got[otherDesignID])
		}
		if _, ok := got[missingDesign]; ok {
			t.Fatalf("a missing design returned: %+v", got[missingDesign])
		}
		if len(got) != 1 {
			t.Fatalf("summaries returned %d designs, want 1", len(got))
		}
		document, err := st.GetSystemDesign(ctx, designID)
		requireOK(t, err)
		full, err := st.ListSystemDesignVersions(ctx, designID)
		requireOK(t, err)
		want := make([]core.SystemDesignVersion, 0, 2)
		for _, version := range full {
			if version.Version != document.CurrentVersion && (version.Confirmed || version.Dismissed) {
				continue
			}
			if version.Content == "" {
				t.Fatalf("current or pending content is empty in the full read: %+v", version)
			}
			want = append(want, version)
		}
		if !reflect.DeepEqual(got[designID], want) {
			t.Fatalf("summaries = %+v\nwant %+v", got[designID], want)
		}
		if len(want) != 2 {
			t.Fatalf("summaries returned %d versions, want only the current and pending rows", len(want))
		}
		if len(full) != versionSummaryHistory+2 {
			t.Fatalf("full read returned %d versions, want %d", len(full), versionSummaryHistory+2)
		}
	})

	t.Run("EmptySelectionsReturnEmptyMaps", func(t *testing.T) {
		for _, ids := range [][]store.DocumentVersionSelection{nil, {}} {
			requirements, err := st.ListRequirementVersionSummaries(ctx, ids)
			if err != nil || requirements == nil || len(requirements) != 0 {
				t.Fatalf("requirement summaries for %v = %+v, %v; want an empty map", ids, requirements, err)
			}
			designs, err := st.ListSystemDesignVersionSummaries(ctx, ids)
			if err != nil || designs == nil || len(designs) != 0 {
				t.Fatalf("design summaries for %v = %+v, %v; want an empty map", ids, designs, err)
			}
		}
	})

	t.Run("DoneContextReturnsError", func(t *testing.T) {
		done, cancel := context.WithCancel(ctx)
		cancel()
		if requirements, err := st.ListRequirementVersionSummaries(done, []store.DocumentVersionSelection{{ID: requirementID, CurrentVersion: 1}}); err == nil || requirements != nil {
			t.Fatalf("requirement summaries with a done context = %+v, %v; want no result and an error", requirements, err)
		}
		if designs, err := st.ListSystemDesignVersionSummaries(done, []store.DocumentVersionSelection{{ID: designID, CurrentVersion: 1}}); err == nil || designs != nil {
			t.Fatalf("design summaries with a done context = %+v, %v; want no result and an error", designs, err)
		}
	})

	// A confirmation that lands between the list's row read and its summary
	// read must not move the listed current version: the summary keeps the
	// content of the version the row named (component-document-corpus).
	t.Run("ConfirmationBetweenRowAndSummaryReadsKeepsListedVersion", func(t *testing.T) {
		raceRequirement := "req-versions-race-" + core.NewTaskID()
		raceDesign := "design-versions-race-" + core.NewTaskID()
		_, first, err := st.CreateRequirement(ctx, core.Requirement{ID: raceRequirement, Title: raceRequirement}, requirementVersion(raceRequirement, 1))
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, raceRequirement, first.Version)
		requireOK(t, err)
		nextRequirement, err := st.ProposeRequirementVersion(ctx, requirementVersion(raceRequirement, 2))
		requireOK(t, err)
		_, firstDesign, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: raceDesign, Title: raceDesign, Category: "Component design"}, designVersion(raceDesign, 1))
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, raceDesign, firstDesign.Version)
		requireOK(t, err)
		nextDesign, err := st.ProposeSystemDesignVersion(ctx, designVersion(raceDesign, 2))
		requireOK(t, err)
		listedRequirement, err := st.GetRequirementVersion(ctx, raceRequirement, first.Version)
		requireOK(t, err)
		listedDesign, err := st.GetSystemDesignVersion(ctx, raceDesign, firstDesign.Version)
		requireOK(t, err)

		requirementSelection := listedRequirements(t, raceRequirement)
		designSelection := listedDesigns(t, raceDesign)
		if requirementSelection[0].CurrentVersion != first.Version || designSelection[0].CurrentVersion != firstDesign.Version {
			t.Fatalf("row reads listed %+v and %+v, want the first confirmed versions", requirementSelection, designSelection)
		}
		// The hook: the next versions are confirmed after the row read and
		// before the summary read.
		_, _, err = st.ConfirmRequirementVersion(ctx, raceRequirement, nextRequirement.Version)
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, raceDesign, nextDesign.Version)
		requireOK(t, err)

		requirements, err := st.ListRequirementVersionSummaries(ctx, requirementSelection)
		requireOK(t, err)
		var gotRequirement *core.RequirementVersion
		for i := range requirements[raceRequirement] {
			if requirements[raceRequirement][i].Version == first.Version {
				gotRequirement = &requirements[raceRequirement][i]
			}
		}
		if gotRequirement == nil || !gotRequirement.Confirmed || gotRequirement.Content == "" || gotRequirement.Content != listedRequirement.Content {
			t.Fatalf("listed requirement version after a later confirmation = %+v, want v%d with its content", gotRequirement, first.Version)
		}
		if len(requirements[raceRequirement]) != 2 {
			t.Fatalf("requirement summaries returned %d versions, want 2", len(requirements[raceRequirement]))
		}

		designs, err := st.ListSystemDesignVersionSummaries(ctx, designSelection)
		requireOK(t, err)
		var gotDesign *core.SystemDesignVersion
		for i := range designs[raceDesign] {
			if designs[raceDesign][i].Version == firstDesign.Version {
				gotDesign = &designs[raceDesign][i]
			}
		}
		if gotDesign == nil || !gotDesign.Confirmed || gotDesign.Content == "" || gotDesign.Content != listedDesign.Content {
			t.Fatalf("listed design version after a later confirmation = %+v, want v%d with its content", gotDesign, firstDesign.Version)
		}
		if len(designs[raceDesign]) != 1 {
			t.Fatalf("design summaries returned %d versions, want only the listed current version", len(designs[raceDesign]))
		}

		// The next list read picks up the version confirmed between the two
		// reads, with its content.
		requirementSelection = listedRequirements(t, raceRequirement)
		designSelection = listedDesigns(t, raceDesign)
		if requirementSelection[0].CurrentVersion != nextRequirement.Version || designSelection[0].CurrentVersion != nextDesign.Version {
			t.Fatalf("next row reads listed %+v and %+v, want the later confirmed versions", requirementSelection, designSelection)
		}
		requirements, err = st.ListRequirementVersionSummaries(ctx, requirementSelection)
		requireOK(t, err)
		for _, version := range requirements[raceRequirement] {
			if version.Version == first.Version && version.Content != "" {
				t.Fatalf("historical requirement v%d kept content on the next read", version.Version)
			}
			if version.Version == nextRequirement.Version && (!version.Confirmed || version.Content != nextRequirement.Content) {
				t.Fatalf("next read requirement v%d = %+v, want the confirmed version with its content", version.Version, version)
			}
		}
		designs, err = st.ListSystemDesignVersionSummaries(ctx, designSelection)
		requireOK(t, err)
		if len(designs[raceDesign]) != 1 || designs[raceDesign][0].Version != nextDesign.Version || designs[raceDesign][0].Content != nextDesign.Content {
			t.Fatalf("next read design summaries = %+v, want only v%d with its content", designs[raceDesign], nextDesign.Version)
		}
	})
}
