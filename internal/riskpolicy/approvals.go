package riskpolicy

// ApprovalInput holds every input of an enforcing task's approval evaluation
// (req-risk-based-review REQ-8, REQ-9). The caller loads each value live on
// every evaluation: the trust list and capability holders are never cached,
// so an edit applies the next time the task is evaluated (AC-9.3).
type ApprovalInput struct {
	// Required is the number of distinct people the task's tier, matched
	// path rules, and frozen merge gate require (Requirements.HumanApprovals).
	Required int
	// Authors are the user IDs of the people whose sessions claimed the
	// task's implementation orders (AC-8.4).
	Authors []string
	// UnknownAuthor is true when an implementation order was claimed by an
	// identity that resolves to no person. An unknown author is never
	// trusted.
	UnknownAuthor bool
	// TrustedAuthors is the repository's trusted_authors list. An empty list
	// trusts every person who holds operate_gates (AC-9.1).
	TrustedAuthors []string
	// Gatekeepers holds, for each person the evaluation names (authors,
	// approvers, and the prospective approver), whether that person holds
	// operate_gates now. A person absent from the map does not hold it.
	Gatekeepers map[string]bool
	// Approvals are the user IDs that approved the current head in the
	// current review round. Duplicates count once.
	Approvals []string
	// Waived is true when a non-author waived the trusted author's approval
	// for the current head and round (AC-8.5).
	Waived bool
	// Prospective is the person whose approval is being considered, or
	// empty. It counts as if recorded, when eligible.
	Prospective string
}

// ApprovalStatus is an evaluation's answer.
type ApprovalStatus struct {
	// Required is the number of distinct people required: Required from the
	// input, raised to one when an untrusted author calls for a person other
	// than the author (REQ-8).
	Required int `json:"required"`
	// Recorded counts the eligible distinct people whose approvals count.
	Recorded int `json:"recorded"`
	// Remaining is the number of further approvals needed: the shortfall of
	// the count, and at least one per unmet author condition.
	Remaining int `json:"remaining"`
	// TrustedAuthorApprovalNeeded is true while a trusted author's approval
	// is still required and not waived (AC-8.4).
	TrustedAuthorApprovalNeeded bool `json:"trusted_author_approval_needed"`
	// AuthorWaiverNeeded is true when a trusted author's approval is required
	// but no author's approval can count, because another author is not
	// trusted: only a non-author's waiver meets the condition (AC-8.4, AC-8.5,
	// REQ-9). No approval lowers it, so it adds nothing to Remaining.
	AuthorWaiverNeeded bool `json:"author_waiver_needed"`
	// NonAuthorApprovalNeeded is true while an untrusted author calls for an
	// approval from a person who is not an author and none counts (AC-9.2).
	NonAuthorApprovalNeeded bool `json:"non_author_approval_needed"`
	// UntrustedAuthor is true when any author is not trusted. Such a task
	// never merges automatically (AC-9.2).
	UntrustedAuthor bool `json:"untrusted_author"`
	// Satisfied is true when the count is met and every author condition
	// holds.
	Satisfied bool `json:"satisfied"`
	// ProspectiveEligible is true when Prospective is set and that person's
	// approval counts.
	ProspectiveEligible bool `json:"-"`
	// ProspectiveReason names why the prospective approval does not count.
	ProspectiveReason string `json:"-"`
}

// Reasons a person's approval does not count.
const (
	ApprovalReasonNoCapability      = "the person does not hold operate_gates"
	ApprovalReasonUntrustedAuthor   = "the person is an author who is not trusted"
	ApprovalReasonAuthorOfUntrusted = "the person is an author, and no author's approval counts while any author is not trusted"
	ApprovalReasonAlreadyCounted    = "the person's approval of this head is already recorded"
	ApprovalReasonNoPersonIdentity  = "no person is named"
)

// Trusted reports whether a person is trusted under the repository's list:
// a listed user ID, or, when the list is empty, a person who holds
// operate_gates now (AC-9.1). An empty user ID is never trusted.
func (in ApprovalInput) Trusted(userID string) bool {
	if userID == "" {
		return false
	}
	if len(in.TrustedAuthors) == 0 {
		return in.Gatekeepers[userID]
	}
	for _, trusted := range in.TrustedAuthors {
		if trusted == userID {
			return true
		}
	}
	return false
}

// EvaluateApprovals applies req-risk-based-review REQ-8 and REQ-9 to the
// approvals of one head. An approval counts only from a person who holds
// operate_gates now, and each person counts once (AC-8.3). An untrusted
// author forbids automatic merge, requires one approval from a person who is
// not an author, and makes every author's own approval uncounted, trusted
// coauthors included (AC-9.2, REQ-9; DEC-124(5)). When any author is trusted,
// one trusted author's approval is required and counts toward the number
// unless a non-author waived it (AC-8.4, AC-8.5); with an untrusted coauthor
// only the waiver meets that condition. The waiver changes no number. Agent
// verdicts are not inputs (component-risk-review invariant 10). The function
// is pure.
func EvaluateApprovals(in ApprovalInput) ApprovalStatus {
	authors := map[string]bool{}
	var trustedAuthors []string
	untrusted := in.UnknownAuthor
	for _, author := range in.Authors {
		if author == "" {
			untrusted = true
			continue
		}
		if authors[author] {
			continue
		}
		authors[author] = true
		if in.Trusted(author) {
			trustedAuthors = append(trustedAuthors, author)
		} else {
			untrusted = true
		}
	}
	required := max(in.Required, 0)
	if untrusted {
		required = max(required, 1)
	}
	eligible := func(userID string) (bool, string) {
		switch {
		case userID == "":
			return false, ApprovalReasonNoPersonIdentity
		case !in.Gatekeepers[userID]:
			return false, ApprovalReasonNoCapability
		case authors[userID] && !in.Trusted(userID):
			return false, ApprovalReasonUntrustedAuthor
		case authors[userID] && untrusted:
			// Only a change whose authors are all trusted may be approved by
			// its own author (REQ-9; DEC-124(5)), so a trusted coauthor does
			// not count either.
			return false, ApprovalReasonAuthorOfUntrusted
		}
		return true, ""
	}
	counted := map[string]bool{}
	for _, userID := range in.Approvals {
		if ok, _ := eligible(userID); ok {
			counted[userID] = true
		}
	}
	status := ApprovalStatus{Required: required, UntrustedAuthor: untrusted}
	if in.Prospective != "" {
		ok, reason := eligible(in.Prospective)
		if ok && counted[in.Prospective] {
			ok, reason = false, ApprovalReasonAlreadyCounted
		}
		status.ProspectiveEligible, status.ProspectiveReason = ok, reason
		if ok {
			counted[in.Prospective] = true
		}
	}
	status.Recorded = len(counted)
	if required >= 1 && len(trustedAuthors) > 0 && !in.Waived {
		if untrusted {
			// No author's approval counts here, so the trusted author's
			// approval can be met only by a non-author's waiver (AC-8.5).
			status.AuthorWaiverNeeded = true
		} else {
			status.TrustedAuthorApprovalNeeded = true
			for _, author := range trustedAuthors {
				if counted[author] {
					status.TrustedAuthorApprovalNeeded = false
					break
				}
			}
		}
	}
	if untrusted {
		status.NonAuthorApprovalNeeded = true
		for userID := range counted {
			if !authors[userID] {
				status.NonAuthorApprovalNeeded = false
				break
			}
		}
	}
	conditions := 0
	if status.TrustedAuthorApprovalNeeded {
		conditions++
	}
	if status.NonAuthorApprovalNeeded {
		conditions++
	}
	status.Remaining = max(required-status.Recorded, conditions)
	status.Satisfied = status.Remaining == 0 && !status.AuthorWaiverNeeded
	return status
}
