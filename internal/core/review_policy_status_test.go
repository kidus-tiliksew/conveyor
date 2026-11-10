package core

import "testing"

// The policy status row lifecycle (component-git-delivery invariant 37).
func TestReviewPolicyStatusTransitions(t *testing.T) {
	allowed := map[[2]ReviewPolicyStatusState]bool{}
	for from, targets := range reviewPolicyStatusTransitions {
		for _, to := range targets {
			allowed[[2]ReviewPolicyStatusState{from, to}] = true
		}
	}
	states := []ReviewPolicyStatusState{ReviewPolicyStatusQueued, ReviewPolicyStatusRetrying, ReviewPolicyStatusPublished, ReviewPolicyStatusFailed}
	for _, from := range states {
		for _, to := range states {
			err := TransitionReviewPolicyStatus(from, to)
			if allowed[[2]ReviewPolicyStatusState{from, to}] != (err == nil) {
				t.Errorf("%s -> %s err=%v", from, to, err)
			}
		}
	}
	// A failed row is reopened or retried, never marked published without
	// an attempt.
	if TransitionReviewPolicyStatus(ReviewPolicyStatusFailed, ReviewPolicyStatusPublished) == nil {
		t.Fatal("a failed row moved straight to published")
	}
	if TransitionReviewPolicyStatus("", ReviewPolicyStatusQueued) == nil {
		t.Fatal("an unknown state moved")
	}
}

func TestReviewPolicyStatusValidate(t *testing.T) {
	good := ReviewPolicyStatus{TaskID: "t", HeadSHA: "h", Desired: PolicyStatusPending, State: ReviewPolicyStatusQueued}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]ReviewPolicyStatus{
		"NoTask":              {HeadSHA: "h", Desired: PolicyStatusPending, State: ReviewPolicyStatusQueued},
		"BlankHead":           {TaskID: "t", HeadSHA: " ", Desired: PolicyStatusPending, State: ReviewPolicyStatusQueued},
		"FailureDesired":      {TaskID: "t", HeadSHA: "h", Desired: "failure", State: ReviewPolicyStatusQueued},
		"PublishedWithout":    {TaskID: "t", HeadSHA: "h", Desired: PolicyStatusSuccess, State: ReviewPolicyStatusPublished},
		"BadPublished":        {TaskID: "t", HeadSHA: "h", Desired: PolicyStatusSuccess, Published: "error", State: ReviewPolicyStatusPublished},
		"UnknownLifecycleRow": {TaskID: "t", HeadSHA: "h", Desired: PolicyStatusSuccess, State: "skipped"},
	} {
		if err := row.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMergeBypassValidate(t *testing.T) {
	good := MergeBypass{ForgeActor: UnknownForgeActor, HeadSHA: "h", MissingAgentReviews: 1}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bypass := range map[string]MergeBypass{
		"NoHead":         {ForgeActor: "a", MissingHumanApprovals: 1},
		"NoActor":        {HeadSHA: "h", MissingHumanApprovals: 1},
		"NothingMissing": {ForgeActor: "a", HeadSHA: "h"},
		"Negative":       {ForgeActor: "a", HeadSHA: "h", MissingHumanApprovals: -1, MissingAgentReviews: 1},
	} {
		if err := bypass.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, only := range []MergeBypass{
		{ForgeActor: "a", HeadSHA: "h", TrustedAuthorApprovalNeeded: true},
		{ForgeActor: "a", HeadSHA: "h", NonAuthorApprovalNeeded: true},
		{ForgeActor: "a", HeadSHA: "h", AuthorWaiverNeeded: true},
	} {
		if err := only.Validate(); err != nil {
			t.Errorf("an author condition alone was refused: %+v %v", only, err)
		}
	}
}
