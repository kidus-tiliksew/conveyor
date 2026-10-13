package core

import "testing"

func TestSystemActorConstructor(t *testing.T) {
	if got := SystemActor(); got != (Actor{ID: "conveyor", Role: ActorSystem}) || !got.Complete() {
		t.Fatalf("canonical system actor = %+v", got)
	}
	for _, subsystem := range []string{"dispatcher", "queue:job-1", "system"} {
		if got := SystemActor(subsystem); got != (Actor{ID: subsystem, Role: ActorSystem}) {
			t.Fatalf("SystemActor(%q) = %+v", subsystem, got)
		}
	}
	if got := SystemActor("  "); got.ID != SystemActorID || got.Role != ActorSystem {
		t.Fatalf("blank subsystem = %+v, want canonical identity", got)
	}
	if got := SystemActor(" dispatcher "); got.ID != "dispatcher" {
		t.Fatalf("subsystem whitespace retained: %+v", got)
	}
	for _, actor := range []Actor{{}, {ID: "conveyor"}, {Role: ActorSystem}, {ID: " ", Role: ActorSystem}} {
		if actor.Complete() {
			t.Fatalf("incomplete actor %+v reported complete", actor)
		}
	}
}
