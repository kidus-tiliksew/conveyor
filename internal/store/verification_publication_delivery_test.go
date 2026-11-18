package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestConvergeVerificationDelivery(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	t.Run("stalePendingSupersedes", func(t *testing.T) {
		d := core.VerificationDelivery{ID: "old", Generation: 1, State: "pending"}
		next, enqueue, err := ConvergeVerificationDelivery(d, 2, now)
		if err != nil || enqueue || next.State != "superseded" {
			t.Fatalf("next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
	})

	t.Run("staleRetryingSupersedes", func(t *testing.T) {
		d := core.VerificationDelivery{ID: "old", Generation: 1, State: "retrying"}
		next, enqueue, err := ConvergeVerificationDelivery(d, 3, now)
		if err != nil || enqueue || next.State != "superseded" {
			t.Fatalf("next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
	})

	t.Run("latestPendingEnqueues", func(t *testing.T) {
		d := core.VerificationDelivery{ID: "cur", Generation: 2, State: "pending"}
		next, enqueue, err := ConvergeVerificationDelivery(d, 2, now)
		if err != nil || !enqueue || next.State != "pending" {
			t.Fatalf("next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
	})

	t.Run("headMismatchClearsRetry", func(t *testing.T) {
		d := core.VerificationDelivery{ID: "mis", Generation: 1, State: "failed", ErrorClass: "head_mismatch", NextAttemptAt: &past}
		next, enqueue, err := ConvergeVerificationDelivery(d, 1, now)
		if err != nil || enqueue || next.NextAttemptAt != nil || next.State != "failed" {
			t.Fatalf("next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
		next, enqueue, err = ConvergeVerificationDelivery(d, 2, now)
		if err != nil || enqueue || next.NextAttemptAt != nil {
			t.Fatalf("stale mismatch next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
	})

	t.Run("forgeFailedRetriesWhenDue", func(t *testing.T) {
		d := core.VerificationDelivery{ID: "forge", Generation: 1, State: "failed", ErrorClass: "forge", NextAttemptAt: &past}
		next, enqueue, err := ConvergeVerificationDelivery(d, 1, now)
		if err != nil || !enqueue || next.State != "pending" {
			t.Fatalf("next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
		d.NextAttemptAt = &future
		next, enqueue, err = ConvergeVerificationDelivery(d, 1, now)
		if err != nil || enqueue || next.State != "failed" {
			t.Fatalf("future retry next=%+v enqueue=%t err=%v", next, enqueue, err)
		}
	})
}

func TestFailDiscardedVerificationDelivery(t *testing.T) {
	now := time.Now().UTC()
	next, err := FailDiscardedVerificationDelivery(core.VerificationDelivery{ID: "p", State: "pending"}, now)
	if err != nil || next.State != "failed" || next.ErrorClass != "forge" || next.NextAttemptAt != nil {
		t.Fatalf("pending discarded=%+v err=%v", next, err)
	}
	if next.ErrorMessage == "exhausted" || next.ErrorMessage == "" {
		t.Fatalf("copied discard reason: %q", next.ErrorMessage)
	}
	next, err = FailDiscardedVerificationDelivery(core.VerificationDelivery{ID: "r", State: "retrying"}, now)
	if err != nil || next.State != "failed" || next.ErrorClass != "forge" {
		t.Fatalf("retrying discarded=%+v err=%v", next, err)
	}
	kept, err := FailDiscardedVerificationDelivery(core.VerificationDelivery{ID: "f", State: "failed", ErrorClass: "head_mismatch"}, now)
	if err != nil || kept.State != "failed" || kept.ErrorClass != "head_mismatch" {
		t.Fatalf("already failed=%+v err=%v", kept, err)
	}
}

func TestPrepareVerificationDeliverySupersedesAllPending(t *testing.T) {
	now := time.Now().UTC()
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	vc := VerificationContext{ID: "ctx"}
	pub := VerificationPublication{ID: "src", ContextID: "ctx", Generation: 2, PullRequestNumber: 9, HeadSHA: head}
	rows := []VerificationRow{
		{Table: "verification_contexts", ID: "ctx", TaskID: "task", Body: verificationJSON(vc)},
		{Table: "verification_publications", ID: "src", TaskID: "task", ContextID: "ctx", Body: verificationJSON(pub)},
	}
	prior := []core.VerificationDelivery{
		{ID: "g0", WorkspaceID: "ws", TaskID: "task", ContextID: "ctx", SourcePublicationID: "older", SourceGeneration: 0, Repository: "o/r", PullRequestNumber: 9, Generation: 0, State: "pending"},
		{ID: "g1", WorkspaceID: "ws", TaskID: "task", ContextID: "ctx", SourcePublicationID: "mid", SourceGeneration: 1, Repository: "o/r", PullRequestNumber: 9, Generation: 1, State: "retrying"},
	}
	_, updates, err := PrepareVerificationDelivery("ws", "task", VerificationPR{Repository: "o/r", Number: 9, Head: head}, rows, prior, "src", now)
	if err != nil {
		t.Fatal(err)
	}
	var pending, superseded int
	for _, d := range updates {
		switch d.State {
		case "pending":
			pending++
			if d.Generation <= 1 {
				t.Fatalf("old generation left pending: %+v", d)
			}
		case "superseded":
			superseded++
		}
	}
	if pending != 1 || superseded < 2 {
		t.Fatalf("pending=%d superseded=%d updates=%d", pending, superseded, len(updates))
	}
}
