package core

import (
	"strings"
	"testing"
)

func TestTaskStartOverMatchesCancellationDomain(t *testing.T) {
	for _, state := range TaskStates() {
		cancel, cancelErr := TransitionTask(TaskState(state), TaskCancel)
		restart, restartErr := TransitionTask(TaskState(state), TaskStartOver)
		if (cancelErr == nil) != (restartErr == nil) || cancel != restart {
			t.Fatalf("state %s cancellation=%s,%v restart=%s,%v", state, cancel, cancelErr, restart, restartErr)
		}
	}
}
func TestTaskStartOverCountsCharacters(t *testing.T) {
	r := TaskStartOverRequest{TaskID: "task", RequestID: "request", Reason: strings.Repeat("ሀ", 200), Note: strings.Repeat("ሀ", 2000)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Reason += "ሀ"
	if r.Validate() == nil {
		t.Fatal("accepted overlong reason")
	}
}
