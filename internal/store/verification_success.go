package store

import (
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func ValidateVerificationSuccess(snapshot VerificationSnapshot, runID string, exit *int) error {
	var attempt *VerificationAttempt
	for i := range snapshot.Attempts {
		if snapshot.Attempts[i].ID == runID {
			attempt = &snapshot.Attempts[i]
		}
	}
	if attempt == nil {
		return ErrVerificationAccess
	}
	var contract *verification.Exercise
	for _, s := range snapshot.Selections {
		for _, subject := range s.Subjects {
			if subject.Subject == attempt.Subject {
				v := subject.Contract
				contract = &v
			}
		}
	}
	for _, o := range snapshot.Obligations {
		if attempt.Subject.Kind == "ordinary" && o.ID == attempt.Subject.ObligationID && o.Digest == attempt.Subject.ContractDigest {
			v := o.Contract
			contract = &v
		}
	}
	if contract == nil {
		return ErrVerificationState
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && (exit == nil || *exit != 0) {
		return ErrVerificationState
	}
	counts := map[string]int{}
	assertions := map[string]core.AssertionResultPayload{}
	evidence := map[string]core.VerificationEvidence{}
	execution, interaction := false, false
	for _, record := range snapshot.Evidence {
		e := record.Envelope
		if e.RunID != runID || e.Subject != attempt.Subject {
			continue
		}
		evidence[e.ID] = e
		counts[e.Type]++
		switch e.Type {
		case "assertion_result":
			var p core.AssertionResultPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				return ErrVerificationInvalid
			}
			if _, ok := assertions[p.AssertionID]; ok {
				return ErrVerificationInvalid
			}
			assertions[p.AssertionID] = p
		case "execution_report":
			var p core.ExecutionReportPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.ExitCode != nil && *p.ExitCode == 0 && p.TimedOut != nil && !*p.TimedOut && p.Cancelled != nil && !*p.Cancelled {
				execution = true
			}
		case "operator_observation":
			interaction = true
		}
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && !execution {
		return ErrVerificationState
	}
	if (contract.Kind == "interactive" || contract.Kind == "hybrid") && !interaction {
		return ErrVerificationState
	}
	if contract.Kind == "observation" && len(contract.EvidenceOutputs) == 0 {
		return ErrVerificationState
	}
	for _, output := range contract.EvidenceOutputs {
		if counts[output.Type] < output.MinimumItems {
			return ErrVerificationState
		}
	}
	for _, id := range contract.RequiredAssertions {
		p, ok := assertions[id]
		if !ok || p.Outcome != "pass" || len(p.Supporting) == 0 {
			return ErrVerificationState
		}
		for _, ref := range p.Supporting {
			if ref.EvidenceID != "" {
				if _, ok := evidence[ref.EvidenceID]; !ok {
					return ErrVerificationInvalid
				}
			} else {
				found := false
				for _, e := range evidence {
					for _, a := range e.Artifacts {
						if a.ArtifactID == ref.ArtifactID && a.SHA256 == ref.SHA256 {
							found = true
						}
					}
				}
				if !found {
					return ErrVerificationInvalid
				}
			}
		}
	}
	completed := map[string]bool{}
	for _, op := range snapshot.Operations {
		bound := op.RunID == runID
		for _, successor := range op.Successors {
			bound = bound || successor.RunID == runID
		}
		if !bound {
			continue
		}
		if len(op.History) == 0 {
			return ErrVerificationState
		}
		state := op.History[len(op.History)-1].State
		if state != "applied" && state != "completed" {
			return ErrVerificationState
		}
		completed[op.StepID+":"+op.Target] = true
	}
	for _, op := range contract.Operations {
		if !completed[op.ID+":"+op.TargetBinding] {
			return ErrVerificationState
		}
	}
	return nil
}
