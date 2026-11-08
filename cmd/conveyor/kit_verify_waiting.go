package main

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// waitingExplanationFallback is the sentence a waiting outcome keeps when the
// success evaluator refuses for a reason the gap helper cannot name.
const waitingExplanationFallback = "required assertion, output, or operator completion evidence is missing"

// waitingExplanationIDLimit caps the assertion IDs and output types one
// explanation names; the rest are counted.
const waitingExplanationIDLimit = 10

// waitingExplanation states what a waiting attempt lacks and the remedy. The
// attempt's report is terminal and immutable (component-verification-service
// invariant 20), so the remedy is a new attempt, never more evidence for this
// one (req-verification-kits AC-3.2).
func waitingExplanation(gaps store.VerificationSuccessGaps) string {
	if gaps.Unnamed || gaps.Empty() {
		return waitingExplanationFallback
	}
	budget := waitingExplanationIDLimit
	take := func(n int) int {
		shown := min(n, budget)
		budget -= shown
		return shown
	}
	var clauses []string
	items := 0
	if n := len(gaps.MissingAssertions); n > 0 {
		items += n
		shown := take(n)
		names := strings.Join(gaps.MissingAssertions[:shown], ", ")
		switch {
		case shown == 0:
			clauses = append(clauses, fmt.Sprintf("%d required assertions have no passing result", n))
		case n == 1:
			clauses = append(clauses, "required assertion "+names+" has no passing result")
		case shown < n:
			clauses = append(clauses, fmt.Sprintf("required assertions %s, and %d more have no passing result", names, n-shown))
		default:
			clauses = append(clauses, "required assertions "+names+" have no passing result")
		}
	}
	if n := len(gaps.ShortOutputs); n > 0 {
		items += n
		shown := take(n)
		names := make([]string, 0, shown)
		for _, o := range gaps.ShortOutputs[:shown] {
			names = append(names, fmt.Sprintf("%s has %d of %d", o.Type, o.Have, o.Minimum))
		}
		switch {
		case shown == 0 && n == 1:
			clauses = append(clauses, "1 evidence output is below its minimum_items")
		case shown == 0:
			clauses = append(clauses, fmt.Sprintf("%d evidence outputs are below their minimum_items", n))
		case shown < n:
			clauses = append(clauses, fmt.Sprintf("evidence output below its minimum_items: %s, and %d more", strings.Join(names, ", "), n-shown))
		default:
			clauses = append(clauses, "evidence output below its minimum_items: "+strings.Join(names, ", "))
		}
	}
	if gaps.MissingOperatorObservation {
		items++
		clauses = append(clauses, "the operator observation is missing")
	}
	pronoun := "it"
	if items > 1 {
		pronoun = "them"
	}
	exercise := items - boolCount(gaps.MissingOperatorObservation)
	var send string
	switch {
	case exercise == 0:
		send = "Have the operator record it"
	case gaps.MissingOperatorObservation:
		send = "Have the exercise send the missing evidence through the relay and the operator record the observation"
	default:
		send = "Have the exercise send " + pronoun + " through the relay"
	}
	return strings.Join(clauses, "; ") + ". This attempt has ended and takes no more evidence. " + send + ", or register a contract that doesn't require " + pronoun + ", then start a new attempt."
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
