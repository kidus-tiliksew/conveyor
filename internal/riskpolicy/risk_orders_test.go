package riskpolicy

import (
	"fmt"
	"testing"
)

// TestNeedsRiskOrder covers every row of component-risk-review invariant 12:
// the mode, the tier, the subject, and the rated paths of the latest
// completed head risk order (req-risk-based-review AC-4.7, AC-5.8, AC-5.9,
// AC-12.3).
func TestNeedsRiskOrder(t *testing.T) {
	paths := func(values ...string) *[]string { return &values }
	empty := paths()
	type row struct {
		name   string
		in     RiskOrderNeedInput
		expect bool
	}
	rows := []row{
		{"off plan", RiskOrderNeedInput{Mode: ModeOff, SubjectKind: RiskSubjectPlan}, false},
		{"off head", RiskOrderNeedInput{Mode: ModeOff, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go"}}, false},
		{"absent mode head", RiskOrderNeedInput{SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go"}}, false},
		{"unknown mode plan", RiskOrderNeedInput{Mode: "strict", SubjectKind: RiskSubjectPlan}, false},
		{"unknown subject", RiskOrderNeedInput{Mode: ModeRecord, SubjectKind: "task"}, false},
		{"no subject", RiskOrderNeedInput{Mode: ModeEnforce}, false},
		{"record high plan", RiskOrderNeedInput{Mode: ModeRecord, Tier: TierHigh, SubjectKind: RiskSubjectPlan}, false},
		{"enforce high plan", RiskOrderNeedInput{Mode: ModeEnforce, Tier: TierHigh, SubjectKind: RiskSubjectPlan}, false},
		{"record high head without prior", RiskOrderNeedInput{Mode: ModeRecord, Tier: TierHigh, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go"}}, false},
		{"enforce high head with new path", RiskOrderNeedInput{Mode: ModeEnforce, Tier: TierHigh, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"b.go"}, LatestCompletedHeadPaths: paths("a.go")}, false},
		{"enforce head prior empty no paths", RiskOrderNeedInput{Mode: ModeEnforce, SubjectKind: RiskSubjectHead, LatestCompletedHeadPaths: empty}, false},
	}
	for _, mode := range []Mode{ModeRecord, ModeEnforce} {
		for _, tier := range []Tier{"", TierLow, TierMedium} {
			label := fmt.Sprintf("%s %s", mode, tier)
			if tier == "" {
				label = fmt.Sprintf("%s unrated", mode)
			}
			rows = append(rows,
				row{label + " plan", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectPlan}, true},
				row{label + " plan ignores head paths", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectPlan, ChangedPaths: []string{"a.go"}, LatestCompletedHeadPaths: paths("a.go")}, true},
				row{label + " head without prior rating", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go"}}, true},
				row{label + " head without prior rating or paths", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead}, true},
				row{label + " head equal paths", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go", "b.go"}, LatestCompletedHeadPaths: paths("a.go", "b.go")}, false},
				row{label + " head subset", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"b.go"}, LatestCompletedHeadPaths: paths("a.go", "b.go")}, false},
				row{label + " head order changed", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"b.go", "a.go"}, LatestCompletedHeadPaths: paths("a.go", "b.go")}, false},
				row{label + " head duplicates", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go", "a.go", "b.go"}, LatestCompletedHeadPaths: paths("b.go", "a.go", "b.go")}, false},
				row{label + " head new path", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go", "c.go"}, LatestCompletedHeadPaths: paths("a.go", "b.go")}, true},
				row{label + " head disjoint", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"c.go"}, LatestCompletedHeadPaths: paths("a.go")}, true},
				row{label + " head known empty prior with path", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, ChangedPaths: []string{"a.go"}, LatestCompletedHeadPaths: empty}, true},
				row{label + " head known empty prior without path", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, LatestCompletedHeadPaths: empty}, false},
				row{label + " head no changed path", RiskOrderNeedInput{Mode: mode, Tier: tier, SubjectKind: RiskSubjectHead, LatestCompletedHeadPaths: paths("a.go")}, false},
			)
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsRiskOrder(tc.in); got != tc.expect {
				t.Fatalf("NeedsRiskOrder(%+v)=%t, want %t", tc.in, got, tc.expect)
			}
			// The same inputs always give the same answer.
			if again := NeedsRiskOrder(tc.in); again != tc.expect {
				t.Fatalf("second NeedsRiskOrder(%+v)=%t, want %t", tc.in, again, tc.expect)
			}
		})
	}
}

// TestNeedsRiskOrderAbsentDiffersFromEmpty proves that a missing completed
// head rating and a completed rating of a head that changed no path are
// different inputs.
func TestNeedsRiskOrderAbsentDiffersFromEmpty(t *testing.T) {
	empty := []string{}
	changed := []string{"a.go"}
	absent := NeedsRiskOrder(RiskOrderNeedInput{Mode: ModeRecord, SubjectKind: RiskSubjectHead})
	known := NeedsRiskOrder(RiskOrderNeedInput{Mode: ModeRecord, SubjectKind: RiskSubjectHead, LatestCompletedHeadPaths: &empty})
	if !absent || known {
		t.Fatalf("absent=%t known-empty=%t, want true and false", absent, known)
	}
	if !NeedsRiskOrder(RiskOrderNeedInput{Mode: ModeRecord, SubjectKind: RiskSubjectHead, ChangedPaths: changed, LatestCompletedHeadPaths: &empty}) {
		t.Fatal("a changed path outside a known-empty rated set needs a risk order")
	}
}

func TestRiskSubjectKindValid(t *testing.T) {
	for kind, want := range map[RiskSubjectKind]bool{RiskSubjectPlan: true, RiskSubjectHead: true, "": false, "task": false} {
		if kind.Valid() != want {
			t.Fatalf("%q.Valid()=%t, want %t", kind, kind.Valid(), want)
		}
	}
}
