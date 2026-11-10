package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"gopkg.in/yaml.v3"
)

// preRiskContractJSON is the exact encoding a contract with these values had
// before risk review existed.
const preRiskContractJSON = `{"verify_stage":true,"max_bounces":3,"stage_timeouts":{"implement":"4h","review":"1h","spec":"30m","verify":"47m"},"review":{"seats":[{},{}],"fail_fast":true,"distinct_models":true},"refresh_review":"delta"}`

func riskContract(risk *riskpolicy.Policy) PolicyContract {
	return PolicyContract{
		VerifyStage: true, MaxBounces: 3,
		StageTimeouts: PolicyStageTimeouts{Spec: "30m", Implement: "4h", Review: "1h", Verify: "47m"},
		Review:        ReviewPanel{Seats: make([]ReviewSeat, 2), FailFast: true, DistinctModels: true},
		RefreshReview: RefreshReviewDelta, Risk: risk,
	}
}

// req-risk-based-review AC-1.1: an absent or off risk policy stores the same
// contract bytes as before this change, and a stored row of that shape reads
// back with no risk policy.
func TestRiskPolicyOffKeepsContractBytes(t *testing.T) {
	for name, risk := range map[string]*riskpolicy.Policy{
		"absent": nil,
		"off":    {Mode: riskpolicy.ModeOff, Tiers: riskpolicy.DefaultTable()},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(riskContract(risk))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != preRiskContractJSON {
				t.Fatalf("contract bytes changed:\n got %s\nwant %s", data, preRiskContractJSON)
			}
		})
	}
	var historical PolicyContract
	if err := json.Unmarshal([]byte(preRiskContractJSON), &historical); err != nil {
		t.Fatal(err)
	}
	if historical.Risk != nil || !historical.HasFrozenPolicy() {
		t.Fatalf("historical contract read as %+v", historical)
	}
	var storedOff PolicyContract
	if err := json.Unmarshal([]byte(withRisk(`"risk":{"mode":"off","tiers":{"low":{"agent_reviews":1,"human_approvals":0},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`)), &storedOff); err != nil {
		t.Fatal(err)
	}
	if storedOff.Risk != nil {
		t.Fatalf("a stored off policy decoded as %+v", storedOff.Risk)
	}
	cfg := validConfig()
	if frozen := cfg.FreezePolicy(); frozen.Risk != nil {
		t.Fatalf("a workspace without risk_policy froze %+v", frozen.Risk)
	}
	cfg.RiskPolicy = &riskpolicy.Settings{Mode: riskpolicy.ModeOff}
	if frozen := cfg.FreezePolicy(); frozen.Risk != nil {
		t.Fatalf("a workspace with mode off froze %+v", frozen.Risk)
	}
}

// AC-1.1: a workspace document without risk_policy marshals exactly as
// before, with no risk_policy key in YAML or JSON.
func TestWorkspaceDocumentWithoutRiskPolicyHasNoKey(t *testing.T) {
	cfg, err := normalizeDeployment(validConfig(), "test")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := MarshalPolicyDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	served, err := json.Marshal(cfg.PolicyDocument())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("risk_policy")) || bytes.Contains(served, []byte("risk_policy")) {
		t.Fatalf("absent risk policy was written:\n%s\n%s", stored, served)
	}
	parsed, err := ParseWorkspaceDocument(stored, validConfig(), "round trip")
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, again) || parsed.RiskPolicy != nil {
		t.Fatalf("round trip changed the stored document:\n%s\n%s", stored, again)
	}
}

// AC-1.2, AC-1.3: saving validates the policy and states the default table
// when none was given; the stored document round-trips the full table.
func TestParseWorkspaceDocumentRiskPolicy(t *testing.T) {
	parse := func(t *testing.T, risk string) (*Config, error) {
		t.Helper()
		cfg, err := normalizeDeployment(validConfig(), "test")
		if err != nil {
			t.Fatal(err)
		}
		document, err := yaml.Marshal(cfg.PolicyDocument())
		if err != nil {
			t.Fatal(err)
		}
		return ParseWorkspaceDocument(append(document, []byte(risk)...), validConfig(), "risk")
	}
	parsed, err := parse(t, "risk_policy:\n  mode: enforce\n")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := parsed.RiskPolicyValue()
	if err != nil || policy == nil || policy.Mode != riskpolicy.ModeEnforce || policy.Tiers != riskpolicy.DefaultTable() {
		t.Fatalf("default table policy=%+v err=%v", policy, err)
	}
	stored, err := MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte("risk_policy:")) || !bytes.Contains(stored, []byte("agent_reviews: 2")) {
		t.Fatalf("stored document does not state the table:\n%s", stored)
	}
	reparsed, err := ParseWorkspaceDocument(stored, validConfig(), "stored")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := reparsed.RiskPolicyValue(); again == nil || *again != *policy {
		t.Fatalf("stored policy reparsed as %+v", again)
	}
	explicit, err := parse(t, "risk_policy:\n  mode: record\n  tiers:\n    low: {agent_reviews: 1, human_approvals: 0}\n    medium: {agent_reviews: 2, human_approvals: 0}\n    high: {agent_reviews: 3, human_approvals: 1}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := explicit.RiskPolicyValue(); got == nil || got.Tiers.High != (riskpolicy.Requirement{AgentReviews: 3, HumanApprovals: 1}) {
		t.Fatalf("explicit table=%+v", got)
	}
	for name, risk := range map[string]string{
		"unknown mode":    "risk_policy:\n  mode: strict\n",
		"incomplete":      "risk_policy:\n  mode: record\n  tiers:\n    low: {agent_reviews: 1, human_approvals: 0}\n",
		"zero agents":     "risk_policy:\n  mode: record\n  tiers:\n    low: {agent_reviews: 0, human_approvals: 0}\n    medium: {agent_reviews: 1, human_approvals: 1}\n    high: {agent_reviews: 2, human_approvals: 2}\n",
		"decreasing":      "risk_policy:\n  mode: record\n  tiers:\n    low: {agent_reviews: 2, human_approvals: 0}\n    medium: {agent_reviews: 1, human_approvals: 1}\n    high: {agent_reviews: 2, human_approvals: 2}\n",
		"fraction":        "risk_policy:\n  mode: record\n  tiers:\n    low: {agent_reviews: 1.5, human_approvals: 0}\n    medium: {agent_reviews: 2, human_approvals: 1}\n    high: {agent_reviews: 2, human_approvals: 2}\n",
		"unknown tier":    "risk_policy:\n  mode: record\n  tiers:\n    critical: {agent_reviews: 1, human_approvals: 0}\n",
		"unknown setting": "risk_policy:\n  mode: record\n  trusted: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, risk); err == nil {
				t.Fatalf("invalid risk policy %q was accepted", risk)
			}
		})
	}
}

// AC-1.5; feature-risk-based-review RR-1: an enabled policy freezes by value,
// round-trips through the stored contract, and a later workspace edit leaves
// the frozen policy and the task's projected configuration unchanged.
func TestRiskPolicyFreezesByValue(t *testing.T) {
	agents, humans := 3, 2
	cfg := validConfig()
	cfg.RiskPolicy = &riskpolicy.Settings{Mode: riskpolicy.ModeEnforce, Tiers: &riskpolicy.SettingsTable{
		Low:    &riskpolicy.SettingsRow{AgentReviews: intPtr(1), HumanApprovals: intPtr(0)},
		Medium: &riskpolicy.SettingsRow{AgentReviews: intPtr(2), HumanApprovals: intPtr(1)},
		High:   &riskpolicy.SettingsRow{AgentReviews: &agents, HumanApprovals: &humans},
	}}
	frozen := cfg.FreezePolicy()
	want := riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.Table{
		Low: riskpolicy.Requirement{AgentReviews: 1}, Medium: riskpolicy.Requirement{AgentReviews: 2, HumanApprovals: 1}, High: riskpolicy.Requirement{AgentReviews: 3, HumanApprovals: 2},
	}}
	if frozen.Risk == nil || *frozen.Risk != want || !frozen.HasFrozenPolicy() {
		t.Fatalf("frozen risk=%+v", frozen.Risk)
	}
	data, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"risk":{"mode":"enforce","tiers":{"low":{"agent_reviews":1,"human_approvals":0}`)) {
		t.Fatalf("stored contract does not hold the policy: %s", data)
	}
	var restored PolicyContract
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Risk == nil || *restored.Risk != want {
		t.Fatalf("restored risk=%+v", restored.Risk)
	}
	// The workspace edits its policy in place and then replaces it.
	agents, humans = 9, 9
	cfg.RiskPolicy.Mode = riskpolicy.ModeRecord
	if *frozen.Risk != want || *restored.Risk != want {
		t.Fatalf("workspace edit reached the frozen policy: %+v %+v", frozen.Risk, restored.Risk)
	}
	cfg.RiskPolicy = &riskpolicy.Settings{Mode: riskpolicy.ModeOff}
	projected := cfg.WithPolicy(restored)
	got, err := projected.RiskPolicyValue()
	if err != nil || got == nil || *got != want {
		t.Fatalf("task projection read %+v, %v; want the frozen policy", got, err)
	}
	if legacy, _ := cfg.WithPolicy(PolicyContract{}).RiskPolicyValue(); legacy != nil {
		t.Fatalf("a contract without risk inherited the workspace policy %+v", legacy)
	}
	// Planning bundle members carry the same frozen value.
	cfg.RiskPolicy = &riskpolicy.Settings{Mode: riskpolicy.ModeRecord}
	member, ok := cfg.DefaultPolicy()
	if !ok || member.Risk == nil || member.Risk.Mode != riskpolicy.ModeRecord || member.Risk.Tiers != riskpolicy.DefaultTable() {
		t.Fatalf("default policy risk=%+v ok=%v", member.Risk, ok)
	}
}

// A stored contract whose risk policy fails validation is refused with the
// fixed shape error rather than read as a weaker policy.
func TestPolicyContractRefusesInvalidStoredRisk(t *testing.T) {
	invalid := withRisk(`"risk":{"mode":"enforce","tiers":{"low":{"agent_reviews":0,"human_approvals":0},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`)
	var decoded PolicyContract
	if err := json.Unmarshal([]byte(invalid), &decoded); err == nil || err.Error() != errPolicyContractShape.Error() {
		t.Fatalf("invalid stored risk err=%v", err)
	}
}

func intPtr(v int) *int { return &v }

// withRisk appends a risk member to the pre-risk contract encoding.
func withRisk(member string) string {
	return strings.TrimSuffix(preRiskContractJSON, "}") + "," + member + "}"
}
