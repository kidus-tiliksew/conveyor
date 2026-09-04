package main

import "testing"

func TestQueueShadowEnabledReadsEnvironment(t *testing.T) {
	cases := map[string]bool{"": false, "off": false, "0": false, "no": false, "on": true, "ON": true, " true ": true, "1": true, "yes": true, "maybe": false}
	for value, want := range cases {
		got := queueShadowEnabled(func(string) string { return value })
		if got != want {
			t.Errorf("CONVEYOR_QUEUE_SHADOW=%q: enabled=%t want %t", value, got, want)
		}
	}
}
