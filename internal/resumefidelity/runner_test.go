package resumefidelity

import "testing"

func TestParseCodexEvents(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"thread.started","thread_id":"019-test"}
{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50,"reasoning_output_tokens":20}}
{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":100,"output_tokens":25,"reasoning_output_tokens":10}}
`)
	session, usage := parseCodexEvents(data)
	if session != "019-test" {
		t.Fatalf("session = %q", session)
	}
	if usage.InputTokens != 1200 || usage.CachedInputTokens != 900 || usage.OutputTokens != 75 || usage.ReasoningOutputTokens != 30 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.EffectiveTokens != 375 {
		t.Fatalf("effective tokens = %d, want 375", usage.EffectiveTokens)
	}
}

func TestParseProbeAnswerAcceptsCodeFence(t *testing.T) {
	t.Parallel()
	data := []byte("```json\n{\"decision\":\"lease_epoch\",\"rationale\":[\"fence stale workers\",\"deterministic replay\"],\"rejected_alternative\":\"heartbeat_only fails on partition\",\"continuity_marker\":\"unknown\"}\n```\n")
	answer, err := parseProbeAnswer(data)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Decision != "lease_epoch" || answer.ContinuityMarker != "unknown" {
		t.Fatalf("answer = %+v", answer)
	}
}
