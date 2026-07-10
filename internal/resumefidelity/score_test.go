package resumefidelity

import (
	"strings"
	"testing"
)

func TestScoreAnswer(t *testing.T) {
	t.Parallel()
	answer := ProbeAnswer{
		Decision: "lease_epoch",
		Rationale: []string{
			"It fences stale workers after ownership changes.",
			"It makes recovery deterministic under event replay.",
		},
		RejectedAlternative: "heartbeat_only fails under a network partition because a stale worker can remain active",
		ContinuityMarker:    continuityMarker,
	}
	score := scoreAnswer(answer)
	if score.Core != 4 || score.Extended != 5 {
		t.Fatalf("score = %+v, want core 4 extended 5", score)
	}
	if !score.NativeMarker {
		t.Fatal("native marker was not credited")
	}
}

func TestScoreAnswerDoesNotCreditUnknownMarker(t *testing.T) {
	t.Parallel()
	answer := ProbeAnswer{
		Decision:            "lease_epoch",
		Rationale:           []string{"fence stale workers", "deterministic event replay"},
		RejectedAlternative: "heartbeat_only is unsafe during a partition",
		ContinuityMarker:    "unknown",
	}
	score := scoreAnswer(answer)
	if score.Core != 4 || score.Extended != 4 || score.NativeMarker {
		t.Fatalf("score = %+v, want core 4 extended 4", score)
	}
}

func TestCompareRequiresRecallMarkerAndCost(t *testing.T) {
	t.Parallel()
	resume := ProbeResult{
		Score: RecallScore{Core: 4, Extended: 5, NativeMarker: true},
		Usage: TokenUsage{EffectiveTokens: 100},
	}
	cold := ProbeResult{
		Score: RecallScore{Core: 4, Extended: 4},
		Usage: TokenUsage{EffectiveTokens: 100},
	}
	comparison := compare(resume, cold)
	if !comparison.ResumeQualified {
		t.Fatalf("comparison = %+v, want qualified", comparison)
	}

	resume.Usage.EffectiveTokens = 126
	if compare(resume, cold).ResumeQualified {
		t.Fatal("resume over the cost ceiling qualified")
	}
	resume.Usage.EffectiveTokens = 100
	resume.Score.NativeMarker = false
	if compare(resume, cold).ResumeQualified {
		t.Fatal("resume without native marker qualified")
	}
}

func TestRoutingDefaultSeparatesMatchingAndCrossVersion(t *testing.T) {
	t.Parallel()
	scenarios := []ScenarioResult{
		{Name: "same_sandbox", Comparison: Comparison{ResumeQualified: true}},
		{Name: "fresh_sandbox", Comparison: Comparison{ResumeQualified: true}},
		{Name: "version_bump", Comparison: Comparison{ResumeQualified: false}},
	}
	routing := routingDefault(scenarios)
	if routing.MatchingVersion != "resume_plus_snapshot" {
		t.Fatalf("matching version = %q", routing.MatchingVersion)
	}
	if routing.CrossVersion != "snapshot_cold_start" {
		t.Fatalf("cross version = %q", routing.CrossVersion)
	}
	if routing.SnapshotFallback != "always_required" {
		t.Fatalf("snapshot fallback = %q", routing.SnapshotFallback)
	}
}

func TestMarkdownReportStatesTokenProxyAndHostBoundary(t *testing.T) {
	t.Parallel()
	report := markdownReport(Result{
		RunID:        "run",
		HostBoundary: "host-equivalent restore boundary",
		Scenarios: []ScenarioResult{{
			Name: "same_sandbox",
		}},
	})
	for _, expected := range []string{"host-equivalent restore boundary", "token-cost proxy", "Calibrated routing default"} {
		if !strings.Contains(report, expected) {
			t.Fatalf("report missing %q:\n%s", expected, report)
		}
	}
}
