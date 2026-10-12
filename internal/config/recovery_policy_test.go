package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryPolicyDefaultsAndBounds(t *testing.T) {
	deployment := &Config{Workspace: "demo", MaxBounces: 3, WorkOrderQueueTimeoutText: "12h", Review: ReviewPanel{Seats: []ReviewSeat{{}}}}
	parsed, err := ParseWorkspaceDocument([]byte("workspace: demo\nmax_bounces: 3\nwork_order_queue_timeout: 12h\nreview:\n  seats: [{}]\n"), deployment, "test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Recovery.AutomaticRecoveries != 2 || parsed.Recovery.Window != 24*time.Hour || parsed.Recovery.WindowText != "24h" {
		t.Fatalf("default recovery=%+v", parsed.Recovery)
	}
	accepted := []string{
		"recovery:\n  automatic_recoveries: 0\n  window: 24h\n",
		"recovery:\n  automatic_recoveries: 10\n  window: 1h\n",
	}
	for _, section := range accepted {
		got, parseErr := ParseWorkspaceDocument([]byte("workspace: demo\nmax_bounces: 3\nwork_order_queue_timeout: 12h\n"+section+"review:\n  seats: [{}]\n"), deployment, "test")
		if parseErr != nil {
			t.Fatalf("accepted %q err=%v", section, parseErr)
		}
		if section == accepted[0] && got.Recovery.AutomaticRecoveries != 0 {
			t.Fatalf("bound 0 parsed as %d", got.Recovery.AutomaticRecoveries)
		}
		if section == accepted[1] && (got.Recovery.AutomaticRecoveries != 10 || got.Recovery.Window != time.Hour) {
			t.Fatalf("bound 10 parsed as %+v", got.Recovery)
		}
	}
	refused := []string{
		"recovery:\n  automatic_recoveries: 11\n  window: 24h\n",
		"recovery:\n  automatic_recoveries: -1\n  window: 24h\n",
		"recovery:\n  automatic_recoveries: 2\n  window: 0s\n",
		"recovery:\n  automatic_recoveries: 2\n  window: -1h\n",
		"recovery:\n  automatic_recoveries: 2\n  window: garbage\n",
	}
	for _, section := range refused {
		_, parseErr := ParseWorkspaceDocument([]byte("workspace: demo\nmax_bounces: 3\nwork_order_queue_timeout: 12h\n"+section+"review:\n  seats: [{}]\n"), deployment, "test")
		if parseErr == nil || !strings.Contains(parseErr.Error(), "recovery.") {
			t.Fatalf("refused %q err=%v", section, parseErr)
		}
	}
}

func TestRecoveryPolicyMarshalRoundTrip(t *testing.T) {
	deployment := &Config{Workspace: "demo", MaxBounces: 3, WorkOrderQueueTimeoutText: "12h", Review: ReviewPanel{Seats: []ReviewSeat{{}}}}
	parsed, err := ParseWorkspaceDocument([]byte("workspace: demo\nmax_bounces: 3\nwork_order_queue_timeout: 12h\nrecovery:\n  automatic_recoveries: 0\n  window: 24h\nreview:\n  seats: [{}]\n"), deployment, "test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "automatic_recoveries: 0") || !strings.Contains(string(data), "window: 24h") {
		t.Fatalf("marshal lost recovery: %s", data)
	}
	again, err := ParseWorkspaceDocument(data, deployment, "round-trip")
	if err != nil || again.Recovery.AutomaticRecoveries != 0 || again.Recovery.Window != 24*time.Hour {
		t.Fatalf("round-trip %+v err=%v", again.Recovery, err)
	}
	encoded, err := parsed.PolicyDocument().MarshalJSON()
	if err != nil || !strings.Contains(string(encoded), `"automatic_recoveries":0`) || !strings.Contains(string(encoded), `"window":"24h"`) {
		t.Fatalf("json=%s err=%v", encoded, err)
	}
}

func TestFreezePolicyIgnoresRecovery(t *testing.T) {
	base := &Config{Workspace: "demo", MaxBounces: 4, WorkOrderQueueTimeoutText: "12h", Review: ReviewPanel{Seats: []ReviewSeat{{}}}, Routing: Routing{Stages: map[string]StageRoute{
		"spec": {TimeoutText: "30m"}, "implement": {TimeoutText: "4h"}, "review": {TimeoutText: "1h"}, "verify": {TimeoutText: "1h"},
	}}}
	if err := base.Recovery.Normalize(); err != nil {
		t.Fatal(err)
	}
	off := *base
	off.Recovery.AutomaticRecoveries = 0
	off.Recovery.WindowText = "1h"
	off.Recovery.Window = time.Hour
	if !reflect.DeepEqual(base.FreezePolicy(), off.FreezePolicy()) {
		t.Fatalf("freeze changed with recovery: %+v vs %+v", base.FreezePolicy(), off.FreezePolicy())
	}
}

func TestReadClockPolicy(t *testing.T) {
	policy, timeout, err := ReadClockPolicy(nil)
	if err != nil || policy.AutomaticRecoveries != 2 || policy.Window != 24*time.Hour || timeout != DefaultWorkOrderQueueTimeout {
		t.Fatalf("default policy=%+v timeout=%s err=%v", policy, timeout, err)
	}
	policy, timeout, err = ReadClockPolicy([]byte("recovery:\n  automatic_recoveries: 0\n  window: 1h\nwork_order_queue_timeout: 12h\nrepos: []\n"))
	if err != nil || policy.AutomaticRecoveries != 0 || policy.Window != time.Hour || timeout != 12*time.Hour {
		t.Fatalf("parsed policy=%+v timeout=%s err=%v", policy, timeout, err)
	}
	if _, _, err = ReadClockPolicy([]byte("recovery:\n  automatic_recoveries: 11\n  window: 1h\n")); err == nil {
		t.Fatal("bound 11 accepted")
	}
}
