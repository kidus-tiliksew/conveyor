package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecisionSummaryBoundsAndNormalizes(t *testing.T) {
	t.Parallel()
	for name, statement := range map[string]string{
		"ascii":      strings.Repeat("a", 1000),
		"spaced":     strings.Repeat("word ", 400),
		"multiline":  strings.Repeat("line one\n\n\tline two ", 100),
		"two bytes":  strings.Repeat("é", 500),
		"three":      strings.Repeat("決", 500),
		"four bytes": strings.Repeat("😀", 500),
		"mixed":      "a" + strings.Repeat("決😀", 300),
		"invalid":    strings.Repeat("a\xff", 400),
	} {
		summary := DecisionSummary(statement)
		if !utf8.ValidString(summary) || utf8.RuneCountInString(summary) > DecisionSummaryMaxRunes || len(summary) > DecisionSummaryMaxBytes {
			t.Fatalf("%s: %d runes, %d bytes, valid=%t", name, utf8.RuneCountInString(summary), len(summary), utf8.ValidString(summary))
		}
		if !strings.HasSuffix(summary, "…") {
			t.Fatalf("%s: %q has no visible ellipsis", name, summary)
		}
		if strings.ContainsAny(summary, "\n\t") {
			t.Fatalf("%s: %q is not one line", name, summary)
		}
		if summary != DecisionSummary(statement) {
			t.Fatalf("%s: summary is not deterministic", name)
		}
	}
	if got := DecisionSummary("  Keep\n\n this   whole.\t"); got != "Keep this whole." {
		t.Fatalf("short statement=%q", got)
	}
	exact := strings.Repeat("x", DecisionSummaryMaxBytes)
	if got := DecisionSummary(exact); got != exact {
		t.Fatalf("a statement exactly at the bound was cut: %q", got)
	}
	if got := DecisionSummary(exact + "x"); len(got) != DecisionSummaryMaxBytes || !strings.HasSuffix(got, "…") {
		t.Fatalf("one byte over the bound: %d bytes %q", len(got), got)
	}
}

func deliveryFixtureSnapshot() GovernanceSnapshot {
	design := func(id, path string, version int) GovernanceDesignContext {
		return GovernanceDesignContext{ID: id, Title: "T " + id, Category: "c", Version: version, Content: "BODY " + id, Governs: []GovernedScope{{Repository: "app", Paths: []string{path}}}}
	}
	return GovernanceSnapshot{
		Designs: []GovernanceDesignContext{design("a-repo", "other/**", 4), design("b-diff", "internal/**", 5), design("c-attached", "internal/**", 6), design("d-attached", "other/**", 7), design("e-submitted", "other/**", 8)},
		Decisions: []Decision{
			{ID: "DEC-2", Status: DecisionSuperseded, SupersededBy: "DEC-1", Statement: "RETIRED", Context: "CTX"},
			{ID: "DEC-1", Status: DecisionConfirmed, Statement: "Current\nrule.", Context: "CTX", AlternativesRejected: "ALT"},
		},
		PendingDesignProposals: []PendingSystemDesignProposal{{DocumentID: "z", Version: 2, ProposalEventID: 2}, {DocumentID: "a", Version: 1, ProposalEventID: 1}},
		ResolutionNotes:        []string{"note"},
	}
}

func TestProjectGovernanceReferencesOrdersAndCarriesNoBody(t *testing.T) {
	t.Parallel()
	snapshot := deliveryFixtureSnapshot()
	before := jsonForCompare(t, snapshot)
	references := ProjectGovernanceReferences(snapshot, "app", GovernanceDesignProvenance{Attached: []string{"d-attached", "c-attached", "unpinned-attachment"}, DiffScoped: []string{"e-submitted"}}, []string{"internal/x.go"})
	if after := jsonForCompare(t, snapshot); after != before {
		t.Fatalf("projection changed the snapshot:\n%s\n%s", before, after)
	}
	got := []string{}
	for _, design := range references.Designs {
		got = append(got, design.ID+":"+string(design.Group))
	}
	// Attachment wins an overlap: c-attached's scope also covers the diff.
	want := []string{"d-attached:attached", "c-attached:attached", "b-diff:diff_scoped", "e-submitted:diff_scoped", "a-repo:repository"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want %v", got, want)
	}
	if references.Designs[0].Version != 7 {
		t.Fatalf("version=%d, want the snapshot's", references.Designs[0].Version)
	}
	encoded := jsonForCompare(t, references)
	for _, forbidden := range []string{"BODY", "CTX", "ALT", "RETIRED", "other/**"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("references carry %q: %s", forbidden, encoded)
		}
	}
	if references.Decisions[0].ID != "DEC-1" || references.Decisions[0].Summary != "Current rule." || references.Decisions[1].Summary != "" || references.Decisions[1].SupersededBy != "DEC-1" {
		t.Fatalf("decisions=%+v", references.Decisions)
	}
	if references.PendingDesignProposals[0].DocumentID != "a" || snapshot.PendingDesignProposals[0].DocumentID != "z" {
		t.Fatalf("pending order=%+v source=%+v", references.PendingDesignProposals, snapshot.PendingDesignProposals)
	}
	// No diff and no provenance: the diff group is absent and repository order is by ID.
	plain := ProjectGovernanceReferences(snapshot, "app", GovernanceDesignProvenance{}, nil)
	for index, design := range plain.Designs {
		if design.Group != GovernanceDesignRepository {
			t.Fatalf("no-diff design %d group=%s", index, design.Group)
		}
		if index > 0 && plain.Designs[index-1].ID > design.ID {
			t.Fatalf("repository group is not sorted: %+v", plain.Designs)
		}
	}
}

func TestGovernanceEventReferencesHoldIdentityOnly(t *testing.T) {
	t.Parallel()
	encoded := jsonForCompare(t, GovernanceEventReferences(deliveryFixtureSnapshot()))
	for _, forbidden := range []string{"BODY", "CTX", "ALT", "RETIRED", "Current", "summary"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("event references carry %q: %s", forbidden, encoded)
		}
	}
	for _, required := range []string{`"id":"a-repo"`, `"version":4`, `"id":"DEC-1","status":"confirmed"`, `"superseded_by":"DEC-1"`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("event references lack %s: %s", required, encoded)
		}
	}
}

func TestOutboundProjectionsLeaveTheSourceIntact(t *testing.T) {
	t.Parallel()
	snapshot := deliveryFixtureSnapshot()
	order := WorkOrder{ID: "o", GovernanceSnapshot: &snapshot, GovernanceSnapshotDigest: "digest", ServedRequirementSnapshot: []ServedRequirementContext{{ID: "r"}}}
	outbound := OutboundWorkOrder(order)
	if outbound.GovernanceSnapshot != nil || outbound.ServedRequirementSnapshot != nil || outbound.GovernanceSnapshotDigest != "digest" {
		t.Fatalf("outbound=%+v", outbound)
	}
	if order.GovernanceSnapshot == nil || order.ServedRequirementSnapshot == nil {
		t.Fatal("projection cleared the source order")
	}
	freshness := ContextFreshness{PriorAuthorityReferences: []string{"system_design:a:v1", "system_design:b:v1"}, Snapshot: ContextSnapshot{AuthorityReferences: []string{"system_design:a:v2", "system_design:b:v1"}, AuthorityDigest: "digest"}}
	projected := OutboundContextFreshness(freshness)
	if projected.Snapshot.AuthorityReferences != nil || projected.Snapshot.AuthorityDigest != "digest" || !reflect.DeepEqual(projected.PriorAuthorityReferences, []string{"system_design:a:v1"}) {
		t.Fatalf("projected=%+v", projected)
	}
	if len(freshness.Snapshot.AuthorityReferences) != 2 || len(freshness.PriorAuthorityReferences) != 2 {
		t.Fatalf("projection changed the source: %+v", freshness)
	}
}

func TestDiffChangedPathsReadsHeadersOnly(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1,2 +1,2 @@\n--- a/not/a/header.go\n+++ b/not/a/header.go\n" +
		"diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\n" +
		"diff --git a/added.go b/added.go\nnew file mode 100644\n--- /dev/null\n+++ b/added.go\n" +
		"diff --git a/mode only.sh b/mode only.sh\nold mode 100644\nnew mode 100755\n" +
		"diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n--- \"a/caf\\303\\251.go\"\n+++ \"b/caf\\303\\251.go\"\n" +
		"diff --git \"a/tab\\there.go\" \"b/tab\\there.go\"\nold mode 100644\nnew mode 100755\n" +
		"diff --git \"a/from\\\"q.go\" \"b/to\\\\q.go\"\nrename from \"from\\\"q.go\"\nrename to \"to\\\\q.go\"\n"
	got := DiffChangedPaths(diff)
	want := []string{"added.go", "café.go", "from\"q.go", "internal/a.go", "mode only.sh", "new.go", "old.go", "tab\there.go", "to\\q.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v want %v", got, want)
	}
	if len(DiffChangedPaths("")) != 0 {
		t.Fatal("empty diff has paths")
	}
}

func TestAppendJSONObjectField(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{`{}`: `{"k":1}`, `{"a":2}`: `{"a":2,"k":1}`, "{\"a\":2}\n": `{"a":2,"k":1}`, `[]`: `[]`} {
		if got := string(AppendJSONObjectField([]byte(input), `"k":1`)); got != want {
			t.Fatalf("append to %q=%q want %q", input, got, want)
		}
	}
}

func jsonForCompare(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
