package store

import (
	"encoding/json"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// Contract JSON and hashes captured on the base commit, before
// verification.Assertion replaced the []string required-assertion list
// (feature-verification-kit-execution VK-3.1). Stored obligation and
// selection-subject contracts must keep decoding to the same bytes and
// digests, or in-flight verification contexts would stop matching.
const (
	preSchema2ObligationContract = `{"argv":["true"],"cwd":"","evidence_outputs":null,"id":"assertions","inputs":null,"kind":"script","operations":null,"permissions":null,"prerequisites":null,"required_assertions":["required-check","second"],"retry_policy":"safe_to_replay","safety_basis":"read only","stages":null,"supports":null,"timeout_seconds":30}`
	preSchema2ObligationHash     = "e6f675106292e1fc46476a785f86579cecbb7c0145d36cbec030191fcbf78d52"
	preSchema2SubjectContract    = `{"Contract":{"argv":["private-command"],"cwd":"","evidence_outputs":null,"id":"kit-check","inputs":null,"kind":"script","operations":null,"permissions":null,"prerequisites":null,"required_assertions":["kit-assertion"],"retry_policy":"safe_to_replay","safety_basis":"read only","stages":null,"supports":null,"timeout_seconds":30},"Subject":{"kind":""}}`
	preSchema2SubjectHash        = "967ad51ab3a642ca61c7f2f6cdc7cf43a21a838880659f43f55795a1c77dd545"
)

func TestPreSchema2StoredContractsKeepBytesAndDigests(t *testing.T) {
	var contract verification.Exercise
	if err := json.Unmarshal([]byte(preSchema2ObligationContract), &contract); err != nil {
		t.Fatal(err)
	}
	if ids := contract.AssertionIDs(); len(ids) != 2 || ids[0] != "required-check" || ids[1] != "second" {
		t.Fatalf("assertion IDs = %v", ids)
	}
	if got := verificationJSON(contract); string(got) != preSchema2ObligationContract || verificationHash(got) != preSchema2ObligationHash {
		t.Fatalf("obligation contract changed:\n%s\n%s", got, verificationHash(got))
	}
	var subject VerificationSubjectContract
	if err := json.Unmarshal([]byte(preSchema2SubjectContract), &subject); err != nil {
		t.Fatal(err)
	}
	if got := verificationJSON(subject); string(got) != preSchema2SubjectContract || verificationHash(got) != preSchema2SubjectHash {
		t.Fatalf("selection subject contract changed:\n%s\n%s", got, verificationHash(got))
	}
}

func TestDescribedContractRoundTrip(t *testing.T) {
	contract := verification.Exercise{ID: "described", RequiredAssertions: []verification.Assertion{{ID: "bare"}, {ID: "described", Description: "The observed state matches."}}}
	raw := verificationJSON(contract)
	var again verification.Exercise
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if len(again.RequiredAssertions) != 2 || again.RequiredAssertions[1].Description != "The observed state matches." || verificationHash(verificationJSON(again)) != verificationHash(raw) {
		t.Fatalf("described contract did not round-trip: %s", raw)
	}
}
