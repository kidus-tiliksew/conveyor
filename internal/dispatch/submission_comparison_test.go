package dispatch

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestValidateDoneCriteriaCoverageRequiresDisjointReasonedAssessment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		hasPlan bool
		value   *core.DoneCriteriaAssessment
		want    string
	}{
		{name: "plan missing assessment", hasPlan: true, want: "assessment is required"},
		{name: "applicability mismatch", hasPlan: true, value: &core.DoneCriteriaAssessment{Summary: "checked"}, want: "does not match"},
		{name: "summary required", hasPlan: true, value: &core.DoneCriteriaAssessment{Applicable: true}, want: "summary is required"},
		{name: "disjoint findings", hasPlan: true, value: &core.DoneCriteriaAssessment{Applicable: true, Summary: "checked", Satisfied: []string{"tests pass"}, Unverified: []string{"tests pass"}}, want: "finding lists are disjoint"},
		{name: "fallback lists empty", value: &core.DoneCriteriaAssessment{Summary: "task body", Unsatisfied: []string{"missing"}}, want: "must be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pipeline.Review{DoneCriteriaCoverage: tt.value}
			err := validateDoneCriteriaCoverage(&result, tt.hasPlan)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}
	result := pipeline.Review{DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: true, Summary: "all criteria assessed", Satisfied: []string{"tests pass"}, Unverified: []string{"manual evidence"}}}
	if err := validateDoneCriteriaCoverage(&result, true); err != nil {
		t.Fatal(err)
	}
}

func TestApproveDoneCriteriaConsistency(t *testing.T) {
	for _, category := range []string{"unverified", "unsatisfied", "conflicts", "all"} {
		for _, verdict := range []string{"approve", "changes_requested"} {
			t.Run(category+"/"+verdict, func(t *testing.T) {
				assessment := &core.DoneCriteriaAssessment{Applicable: true, Summary: "mandatory aggregate remains unresolved despite unrelated timing failure", Satisfied: []string{"focused tests pass"}}
				switch category {
				case "unverified":
					assessment.Unverified = []string{storetest.PR907MandatoryValidation}
				case "unsatisfied":
					assessment.Unsatisfied = []string{storetest.PR907MandatoryValidation}
				case "conflicts":
					assessment.Conflicts = []string{storetest.PR907MandatoryValidation}
				case "all":
					assessment.Unverified, assessment.Unsatisfied, assessment.Conflicts = []string{storetest.PR907MandatoryValidation}, []string{"failed"}, []string{"conflict"}
				}
				before := core.JSONPayload(assessment)
				result := pipeline.Review{Verdict: verdict, DoneCriteriaCoverage: assessment}
				err := validateDoneCriteriaCoverage(&result, true)
				if verdict == "approve" {
					want := category
					if category == "all" {
						want = "unsatisfied, unverified, conflicts"
					}
					if err == nil || !strings.Contains(err.Error(), "blocks approve: unresolved criteria in "+want) || !strings.Contains(err.Error(), "submit changes_requested") {
						t.Fatalf("error=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(core.JSONPayload(result.DoneCriteriaCoverage)) || result.Verdict != verdict {
					t.Fatal("validation changed unfavorable findings or verdict")
				}
			})
		}
	}
	result := pipeline.Review{Verdict: "approve", DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: true, Summary: "all validation passed", Satisfied: []string{storetest.PR907MandatoryValidation}}}
	if err := validateDoneCriteriaCoverage(&result, true); err != nil {
		t.Fatal(err)
	}
}
