package resumefidelity

import (
	"fmt"
	"strings"
)

const resumeCostCeiling = 1.25

func scoreAnswer(answer ProbeAnswer) RecallScore {
	decision := normalizedContains(answer.Decision, "lease_epoch")
	rationale := strings.Join(answer.Rationale, " ")
	staleWorker := containsAll(rationale, "stale", "fenc")
	replay := containsAll(rationale, "determin", "replay")
	rejected := normalizedContains(answer.RejectedAlternative, "heartbeat") &&
		(normalizedContains(answer.RejectedAlternative, "partition") || normalizedContains(answer.RejectedAlternative, "stale"))
	marker := strings.EqualFold(strings.TrimSpace(answer.ContinuityMarker), continuityMarker)

	core := boolInt(decision) + boolInt(staleWorker) + boolInt(replay) + boolInt(rejected)
	return RecallScore{
		Decision:            decision,
		StaleWorkerFencing:  staleWorker,
		DeterministicReplay: replay,
		RejectedHeartbeat:   rejected,
		NativeMarker:        marker,
		Core:                core,
		Extended:            core + boolInt(marker),
	}
}

func compare(resume, cold ProbeResult) Comparison {
	ratio := 0.0
	if cold.Usage.EffectiveTokens > 0 {
		ratio = float64(resume.Usage.EffectiveTokens) / float64(cold.Usage.EffectiveTokens)
	}
	qualified := resume.CLIError == "" && resume.ParseError == "" &&
		resume.Score.Core >= cold.Score.Core && resume.Score.NativeMarker &&
		cold.Usage.EffectiveTokens > 0 && ratio <= resumeCostCeiling
	return Comparison{
		CoreScoreDelta:      resume.Score.Core - cold.Score.Core,
		ExtendedScoreDelta:  resume.Score.Extended - cold.Score.Extended,
		EffectiveTokenDelta: resume.Usage.EffectiveTokens - cold.Usage.EffectiveTokens,
		ResumeToColdRatio:   ratio,
		ResumeQualified:     qualified,
	}
}

func recommendation(c Comparison) string {
	if c.ResumeQualified {
		return fmt.Sprintf("prefer resume+snapshot (recall preserved; effective-token ratio %.2f <= %.2f)", c.ResumeToColdRatio, resumeCostCeiling)
	}
	return fmt.Sprintf("prefer snapshot cold start (resume did not meet recall+cost gate; effective-token ratio %.2f)", c.ResumeToColdRatio)
}

func routingDefault(scenarios []ScenarioResult) RoutingDefault {
	byName := make(map[string]ScenarioResult, len(scenarios))
	for _, scenario := range scenarios {
		byName[scenario.Name] = scenario
	}
	sameOK := byName["same_sandbox"].Comparison.ResumeQualified
	freshOK := byName["fresh_sandbox"].Comparison.ResumeQualified
	bumpOK := byName["version_bump"].Comparison.ResumeQualified

	matching := "snapshot_cold_start"
	if sameOK && freshOK {
		matching = "resume_plus_snapshot"
	}
	cross := "snapshot_cold_start"
	if bumpOK {
		cross = "resume_plus_snapshot"
	}

	return RoutingDefault{
		Harness:          "codex",
		MatchingVersion:  matching,
		CrossVersion:     cross,
		SnapshotFallback: "always_required",
		Rationale: fmt.Sprintf(
			"The deterministic gate requires preserved core recall, native-marker recall, and resume effective tokens at or below %.0f%% of cold start. same=%t fresh=%t version_bump=%t.",
			resumeCostCeiling*100, sameOK, freshOK, bumpOK,
		),
	}
}

func normalizedContains(s, fragment string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(s)), strings.ToLower(fragment))
}

func containsAll(s string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !normalizedContains(s, fragment) {
			return false
		}
	}
	return true
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
