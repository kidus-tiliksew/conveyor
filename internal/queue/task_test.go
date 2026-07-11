package queue

import "testing"

func TestDispatchQueueIsStableAndWorkspaceScoped(t *testing.T) {
	t.Parallel()
	first := DispatchQueue("acme")
	if first != DispatchQueue("acme") {
		t.Fatal("queue name is not stable")
	}
	if first == DispatchQueue("other") {
		t.Fatal("workspace queues collided")
	}
	if len(first) != len("dispatch_")+16 {
		t.Fatalf("queue name = %q", first)
	}
}
