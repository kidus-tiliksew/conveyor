package core

import "testing"

func TestVerificationOperationTransition(t *testing.T) {
	for _, path := range [][]string{
		{"registered", "dispatching", "completed"},
		{"registered", "outcome_unknown", "unknown", "not_applied", "dispatching", "applied"},
	} {
		for i := 1; i < len(path); i++ {
			if err := VerificationOperationTransition(path[i-1], path[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, state := range []string{"registered", "completed", "applied", "unknown", "outcome_unknown"} {
		if state != "registered" && VerificationOperationTransition(state, "dispatching") == nil {
			t.Fatalf("%s admitted dispatch", state)
		}
		if VerificationOperationTransition(state, "registered") == nil {
			t.Fatalf("%s reset identity", state)
		}
	}
}

func TestVerificationOperationSubjectSurvivesSourceChange(t *testing.T) {
	a := VerificationSubject{Kind: "kit", KitID: "kit", ExerciseID: "exercise", ContentDigest: "old"}
	b := a
	b.ContentDigest = "new"
	b.KitVersion = "2"
	if VerificationOperationSubject(a) != VerificationOperationSubject(b) {
		t.Fatal("source change hid original operation")
	}
}

func TestVerificationProviderReferenceSanitation(t *testing.T) {
	got := SanitizeVerificationProviderReference("https://user:secret@provider.invalid/resource/42?token=secret#private")
	if got != "https://provider.invalid/resource/42" {
		t.Fatal(got)
	}
}
