package riskpolicy

import (
	"encoding/json"
	"go/build"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func intp(v int) *int { return &v }

func row(agent, human int) *SettingsRow {
	return &SettingsRow{AgentReviews: intp(agent), HumanApprovals: intp(human)}
}

// req-risk-based-review AC-1.2, AC-1.3: closed modes, complete explicit
// tables, and no value lower than the tier below it.
func TestSettingsResolve(t *testing.T) {
	full := func() *SettingsTable { return &SettingsTable{Low: row(1, 0), Medium: row(2, 1), High: row(3, 2)} }
	for _, tc := range []struct {
		name     string
		settings Settings
		want     *Policy
		err      string
	}{
		{name: "record default table", settings: Settings{Mode: ModeRecord}, want: &Policy{Mode: ModeRecord, Tiers: DefaultTable()}},
		{name: "enforce default table", settings: Settings{Mode: ModeEnforce}, want: &Policy{Mode: ModeEnforce, Tiers: DefaultTable()}},
		{name: "off keeps stated table", settings: Settings{Mode: ModeOff, Tiers: full()}, want: &Policy{Mode: ModeOff, Tiers: Table{Requirement{1, 0}, Requirement{2, 1}, Requirement{3, 2}}}},
		{name: "explicit table", settings: Settings{Mode: ModeEnforce, Tiers: full()}, want: &Policy{Mode: ModeEnforce, Tiers: Table{Requirement{1, 0}, Requirement{2, 1}, Requirement{3, 2}}}},
		{name: "equal tiers", settings: Settings{Mode: ModeEnforce, Tiers: &SettingsTable{Low: row(1, 1), Medium: row(1, 1), High: row(1, 1)}}, want: &Policy{Mode: ModeEnforce, Tiers: Table{Requirement{1, 1}, Requirement{1, 1}, Requirement{1, 1}}}},
		{name: "empty mode", settings: Settings{}, err: "risk_policy.mode"},
		{name: "unknown mode", settings: Settings{Mode: "strict"}, err: "risk_policy.mode"},
		{name: "missing row", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(1, 0), High: row(2, 2)}}, err: "risk_policy.tiers.medium is required"},
		{name: "missing agent value", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: &SettingsRow{HumanApprovals: intp(0)}, Medium: row(1, 1), High: row(2, 2)}}, err: "risk_policy.tiers.low.agent_reviews is required"},
		{name: "missing human value", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(1, 0), Medium: row(1, 1), High: &SettingsRow{AgentReviews: intp(2)}}}, err: "risk_policy.tiers.high.human_approvals is required"},
		{name: "zero agent reviews", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(0, 0), Medium: row(1, 1), High: row(2, 2)}}, err: "risk_policy.tiers.low.agent_reviews must be at least 1"},
		{name: "negative human approvals", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(1, -1), Medium: row(1, 1), High: row(2, 2)}}, err: "risk_policy.tiers.low.human_approvals must be zero or more"},
		{name: "agent decreases", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(1, 0), Medium: row(3, 1), High: row(2, 2)}}, err: "risk_policy.tiers.high.agent_reviews must not be lower than medium"},
		{name: "human decreases", settings: Settings{Mode: ModeRecord, Tiers: &SettingsTable{Low: row(1, 1), Medium: row(1, 0), High: row(2, 2)}}, err: "risk_policy.tiers.medium.human_approvals must not be lower than low"},
		{name: "off still validates a stated table", settings: Settings{Mode: ModeOff, Tiers: &SettingsTable{Low: row(0, 0), Medium: row(1, 1), High: row(2, 2)}}, err: "agent_reviews must be at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.settings.Resolve()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Resolve() err=%v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, *tc.want) {
				t.Fatalf("Resolve()=%+v, %v; want %+v", got, err, *tc.want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("resolved policy does not validate: %v", err)
			}
			again, err := SettingsFor(got).Resolve()
			if err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("SettingsFor round trip=%+v, %v", again, err)
			}
		})
	}
}

// Wire decoding tells an omitted table from an incomplete one and refuses a
// value that is not a whole number or a tier that does not exist.
func TestSettingsDecoding(t *testing.T) {
	var omitted Settings
	if err := json.Unmarshal([]byte(`{"mode":"enforce"}`), &omitted); err != nil || omitted.Tiers != nil {
		t.Fatalf("omitted table decoded as %+v, %v", omitted, err)
	}
	var incomplete Settings
	if err := json.Unmarshal([]byte(`{"mode":"enforce","tiers":{"low":{"agent_reviews":1,"human_approvals":0}}}`), &incomplete); err != nil {
		t.Fatal(err)
	}
	if _, err := incomplete.Resolve(); err == nil || !strings.Contains(err.Error(), "medium is required") {
		t.Fatalf("incomplete table err=%v", err)
	}
	var fraction Settings
	if err := json.Unmarshal([]byte(`{"mode":"enforce","tiers":{"low":{"agent_reviews":1.5,"human_approvals":0}}}`), &fraction); err == nil {
		t.Fatal("JSON accepted a fractional agent review count")
	}
	decoder := yaml.NewDecoder(strings.NewReader("mode: record\ntiers:\n  critical: {agent_reviews: 1, human_approvals: 0}\n"))
	decoder.KnownFields(true)
	var unknown Settings
	if err := decoder.Decode(&unknown); err == nil {
		t.Fatal("YAML accepted an unknown tier")
	}
	if err := yaml.Unmarshal([]byte("mode: record\ntiers:\n  low: {agent_reviews: one, human_approvals: 0}\n"), &unknown); err == nil {
		t.Fatal("YAML accepted a non-integer agent review count")
	}
	if err := yaml.Unmarshal([]byte("mode: record\ntiers:\n  low: {agent_reviews: 1.5, human_approvals: 0}\n"), &unknown); err == nil || !strings.Contains(err.Error(), "cannot unmarshal !!float into int") {
		t.Fatalf("YAML fractional agent review count err=%v", err)
	}
	if err := yaml.Unmarshal([]byte("mode: record\ntiers:\n  low: {agent_reviews: 1, human_approvals: 0, extra: 1}\n"), &unknown); err == nil {
		t.Fatal("YAML accepted an unknown row key")
	}
}

func TestSettingsCloneIsIndependent(t *testing.T) {
	source := &Settings{Mode: ModeEnforce, Tiers: &SettingsTable{Low: row(1, 0), Medium: row(1, 1), High: row(2, 2)}}
	clone := source.Clone()
	*clone.Tiers.High.AgentReviews = 9
	clone.Tiers.Low = nil
	if *source.Tiers.High.AgentReviews != 2 || source.Tiers.Low == nil {
		t.Fatalf("clone aliases its source: %+v", source.Tiers)
	}
	if (*Settings)(nil).Clone() != nil {
		t.Fatal("nil clone is not nil")
	}
}

// AC-1.4: the warning names the required and available counts; off warns
// nothing.
func TestCheckHumanApprovals(t *testing.T) {
	enforce := &Policy{Mode: ModeEnforce, Tiers: DefaultTable()}
	if got := CheckHumanApprovals(enforce, 1); got == nil || *got != (HumanApprovalWarning{Required: 2, Available: 1}) {
		t.Fatalf("warning=%+v", got)
	}
	if got := CheckHumanApprovals(enforce, 2); got != nil {
		t.Fatalf("enough people still warned: %+v", got)
	}
	if got := CheckHumanApprovals(&Policy{Mode: ModeRecord, Tiers: DefaultTable()}, 0); got == nil || got.Required != 2 || got.Available != 0 {
		t.Fatalf("record warning=%+v", got)
	}
	if got := CheckHumanApprovals(&Policy{Mode: ModeOff, Tiers: DefaultTable()}, 0); got != nil {
		t.Fatalf("off warned: %+v", got)
	}
	if got := CheckHumanApprovals(nil, 0); got != nil {
		t.Fatalf("absent policy warned: %+v", got)
	}
}

// component-risk-review invariant 1: three ordered tiers and nothing else.
func TestTierOrder(t *testing.T) {
	if !(TierLow.Rank() < TierMedium.Rank() && TierMedium.Rank() < TierHigh.Rank()) {
		t.Fatal("tiers are not ordered low, medium, high")
	}
	for _, name := range []string{"", "critical", "LOW", "none"} {
		if _, err := ParseTier(name); err == nil {
			t.Fatalf("ParseTier(%q) accepted an unknown tier", name)
		}
	}
	if MaxTier(TierHigh, "critical") != TierHigh || MaxTier("", TierLow) != TierLow {
		t.Fatal("an invalid tier won MaxTier")
	}
}

// AC-3.1: schema version, closed schema, one document, size limit, and the
// rule fields.
func TestParseDeclaration(t *testing.T) {
	valid := "schema_version: 1\nrules:\n  - paths: [internal/store/**]\n    min_tier: high\n    description: Store schema\n    min_human_approvals: 1\n"
	declaration, err := ParseDeclaration([]byte(valid))
	if err != nil || len(declaration.Rules) != 1 || declaration.Rules[0].MinTier != TierHigh || *declaration.Rules[0].MinHumanApprovals != 1 {
		t.Fatalf("valid declaration=%+v, %v", declaration, err)
	}
	if declaration, err := ParseDeclaration([]byte("schema_version: 1\nrules: []\n")); err != nil || len(declaration.Rules) != 0 {
		t.Fatalf("empty rule list=%+v, %v", declaration, err)
	}
	for _, tc := range []struct{ name, content, err string }{
		{"empty", "", "exactly one YAML document"},
		{"two documents", valid + "---\n" + valid, "exactly one YAML document"},
		{"schema version", strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1), "schema_version must be 1"},
		{"missing schema version", strings.Replace(valid, "schema_version: 1\n", "", 1), "schema_version must be 1"},
		{"unknown top-level key", valid + "owners: []\n", "owners"},
		{"unknown rule key", strings.Replace(valid, "    description:", "    severity: x\n    description:", 1), "severity"},
		{"empty paths", strings.Replace(valid, "[internal/store/**]", "[]", 1), "rules[0].paths must not be empty"},
		{"missing paths", strings.Replace(valid, "  - paths: [internal/store/**]\n    min_tier", "  - min_tier", 1), "rules[0].paths must not be empty"},
		{"absolute glob", strings.Replace(valid, "internal/store/**", "/etc/**", 1), "rules[0].paths[0]"},
		{"parent glob", strings.Replace(valid, "internal/store/**", "../x", 1), "rules[0].paths[0]"},
		{"class glob", strings.Replace(valid, "internal/store/**", "'internal/[ab]'", 1), "rules[0].paths[0]"},
		{"min tier", strings.Replace(valid, "min_tier: high", "min_tier: critical", 1), "rules[0].min_tier"},
		{"missing min tier", strings.Replace(valid, "    min_tier: high\n", "", 1), "rules[0].min_tier"},
		{"blank description", strings.Replace(valid, "Store schema", "\"  \"", 1), "rules[0].description"},
		{"negative approvals", strings.Replace(valid, "min_human_approvals: 1", "min_human_approvals: -1", 1), "min_human_approvals must be zero or more"},
		{"fractional approvals", strings.Replace(valid, "min_human_approvals: 1", "min_human_approvals: 1.5", 1), "risk.yaml"},
		{"not a mapping", "- a\n", "risk.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDeclaration([]byte(tc.content)); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("ParseDeclaration err=%v, want %q", err, tc.err)
			}
		})
	}
	oversize := valid + "# " + strings.Repeat("x", MaxDeclarationBytes) + "\n"
	if _, err := ParseDeclaration([]byte(oversize)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err=%v", err)
	}
	limit := valid + "# " + strings.Repeat("x", MaxDeclarationBytes-len(valid)-3) + "\n"
	if len(limit) != MaxDeclarationBytes {
		t.Fatalf("limit fixture is %d bytes", len(limit))
	}
	if _, err := ParseDeclaration([]byte(limit)); err != nil {
		t.Fatalf("declaration at the limit refused: %v", err)
	}
}

// The declaration uses the System Design governed-path glob dialect, not
// filepath.Match: ** spans directories, **/ matches zero directories, and *
// and ? stay inside one segment.
func TestDeclarationGlobDialect(t *testing.T) {
	declaration := []byte("schema_version: 1\nrules:\n" +
		"  - {paths: ['internal/**/schema.sql'], min_tier: medium, description: schema}\n" +
		"  - {paths: ['cmd/*.go'], min_tier: low, description: commands}\n" +
		"  - {paths: ['web/?.ts'], min_tier: high, description: one letter}\n")
	for _, tc := range []struct {
		path    string
		matches []int
	}{
		{"internal/schema.sql", []int{0}},
		{"internal/store/postgres/schema.sql", []int{0}},
		{"cmd/main.go", []int{1}},
		{"cmd/conveyor/main.go", nil},
		{"web/a.ts", []int{2}},
		{"web/ab.ts", nil},
		{"./cmd/main.go", []int{1}},
		{"cmd\\main.go", []int{1}},
	} {
		got := PathMinimums(PathInput{Declaration: DeclarationInput{State: DeclarationPresent, Bytes: declaration}, ChangedPaths: []string{tc.path}, ChangedPathsComplete: true})
		var indexes []int
		for _, match := range got.Matches {
			indexes = append(indexes, match.Index)
		}
		if !reflect.DeepEqual(indexes, tc.matches) {
			t.Fatalf("%s matched rules %v, want %v", tc.path, indexes, tc.matches)
		}
	}
}

// AC-3.3, AC-3.4, AC-3.5, AC-5.4; component-risk-review invariants 5 and 7.
func TestPathMinimums(t *testing.T) {
	declared := func(content string) DeclarationInput {
		return DeclarationInput{State: DeclarationPresent, Bytes: []byte(content)}
	}
	lowering := declared("schema_version: 1\nrules:\n  - {paths: ['.github/**', '.conveyor/risk.yaml'], min_tier: low, description: lower, min_human_approvals: 0}\n")
	sensitive := declared("schema_version: 1\nrules:\n  - {paths: ['internal/store/**'], min_tier: high, description: store, min_human_approvals: 3}\n  - {paths: ['docs/**'], min_tier: medium, description: docs}\n")
	absent := DeclarationInput{State: DeclarationAbsent}
	for _, tc := range []struct {
		name    string
		in      PathInput
		tier    Tier
		human   int
		sources []RuleSource
		reasons int
	}{
		{"absent declaration, ordinary path", PathInput{Declaration: absent, ChangedPaths: []string{"cmd/main.go"}, ChangedPathsComplete: true}, TierLow, 0, nil, 0},
		{"absent declaration, workflow", PathInput{Declaration: absent, ChangedPaths: []string{".github/workflows/ci.yml"}, ChangedPathsComplete: true}, TierMedium, 1, []RuleSource{RuleBuiltin}, 0},
		{"absent declaration, declaration file", PathInput{Declaration: absent, ChangedPaths: []string{".conveyor/risk.yaml"}, ChangedPathsComplete: true}, TierMedium, 1, []RuleSource{RuleBuiltin}, 0},
		{"declaration cannot lower built-ins", PathInput{Declaration: lowering, ChangedPaths: []string{".github/workflows/ci.yml", ".conveyor/risk.yaml"}, ChangedPathsComplete: true}, TierMedium, 1, []RuleSource{RuleBuiltin, RuleBuiltin, RuleDeclared}, 0},
		{"declared rules raise", PathInput{Declaration: sensitive, ChangedPaths: []string{"internal/store/x.go", "docs/a.md"}, ChangedPathsComplete: true}, TierHigh, 3, []RuleSource{RuleDeclared, RuleDeclared}, 0},
		{"declared rule without approvals", PathInput{Declaration: sensitive, ChangedPaths: []string{"docs/a.md"}, ChangedPathsComplete: true}, TierMedium, 0, []RuleSource{RuleDeclared}, 0},
		{"malformed bytes", PathInput{Declaration: declared("schema_version: 9\n"), ChangedPaths: []string{"cmd/main.go"}, ChangedPathsComplete: true}, TierHigh, 0, nil, 1},
		{"malformed state", PathInput{Declaration: DeclarationInput{State: DeclarationMalformed, Reason: "bad yaml"}, ChangedPaths: []string{".github/workflows/x.yml"}, ChangedPathsComplete: true}, TierHigh, 1, []RuleSource{RuleBuiltin}, 1},
		{"unavailable", PathInput{Declaration: DeclarationInput{State: DeclarationUnavailable}, ChangedPathsComplete: true}, TierHigh, 0, nil, 1},
		{"unknown state", PathInput{Declaration: DeclarationInput{State: "surprise"}, ChangedPathsComplete: true}, TierHigh, 0, nil, 1},
		{"zero state", PathInput{ChangedPathsComplete: true}, TierHigh, 0, nil, 1},
		{"incomplete changed paths", PathInput{Declaration: absent, ChangedPaths: []string{"cmd/main.go"}, ChangedPathsReason: "300 files"}, TierHigh, 0, nil, 1},
		{"both fallbacks", PathInput{Declaration: DeclarationInput{State: DeclarationUnavailable}}, TierHigh, 0, nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PathMinimums(tc.in)
			var sources []RuleSource
			for _, match := range got.Matches {
				sources = append(sources, match.Source)
			}
			if got.Tier != tc.tier || got.HumanApprovals != tc.human || !reflect.DeepEqual(sources, tc.sources) || len(got.Reasons) != tc.reasons {
				t.Fatalf("PathMinimums=%+v; want tier %s human %d sources %v reasons %d", got, tc.tier, tc.human, tc.sources, tc.reasons)
			}
			again := PathMinimums(tc.in)
			if !reflect.DeepEqual(got, again) {
				t.Fatalf("PathMinimums is not deterministic: %+v then %+v", got, again)
			}
		})
	}
	order := PathMinimums(PathInput{Declaration: sensitive, ChangedPaths: []string{"internal/store/b.go", "internal/store/a.go", "internal/store/a.go"}, ChangedPathsComplete: true})
	if !reflect.DeepEqual(order.Matches[0].Paths, []string{"internal/store/a.go", "internal/store/b.go"}) {
		t.Fatalf("matched paths are not sorted and unique: %v", order.Matches[0].Paths)
	}
}

// AC-5.5; component-risk-review invariants 3 and 4: the tier is the largest
// input and never falls below the current tier.
func TestTaskTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   TierInput
		want Tier
	}{
		{"no inputs", TierInput{}, TierLow},
		{"path minimum", TierInput{PathMinimum: TierMedium}, TierMedium},
		{"any approved plan", TierInput{PlanTiers: []Tier{TierHigh, TierLow}}, TierHigh},
		{"later lower plan keeps earlier", TierInput{PlanTiers: []Tier{TierMedium, TierLow}}, TierMedium},
		{"reviewer rating", TierInput{PathMinimum: TierLow, HeadRatings: []Tier{TierLow, TierHigh}}, TierHigh},
		{"person raise", TierInput{PersonRaises: []Tier{TierMedium}}, TierMedium},
		{"current tier never falls", TierInput{Current: TierHigh, PathMinimum: TierLow, PlanTiers: []Tier{TierLow}}, TierHigh},
		{"unknown values add nothing", TierInput{Current: "critical", HeadRatings: []Tier{"", "x"}}, TierLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TaskTier(tc.in); got != tc.want {
				t.Fatalf("TaskTier=%s, want %s", got, tc.want)
			}
		})
	}
	// Adding any input never lowers the result.
	base := TierInput{PathMinimum: TierMedium}
	for _, extra := range Tiers {
		with := base
		with.HeadRatings = []Tier{extra}
		if TaskTier(with).Rank() < TaskTier(base).Rank() {
			t.Fatalf("adding %s lowered the tier", extra)
		}
	}
}

// REQ-2, AC-1.1, AC-1.6, AC-2.1; component-risk-review invariant 2.
func TestCalculateRequirements(t *testing.T) {
	table := Table{Low: Requirement{1, 0}, Medium: Requirement{2, 1}, High: Requirement{3, 2}}
	enforce := &Policy{Mode: ModeEnforce, Tiers: table}
	record := &Policy{Mode: ModeRecord, Tiers: table}
	off := &Policy{Mode: ModeOff, Tiers: table}
	for _, tc := range []struct {
		name       string
		in         RequirementInput
		effective  Requirements
		calculated Requirements
		enforced   bool
	}{
		{"absent policy keeps floors", RequirementInput{Tier: TierHigh, Floors: Floors{SeatMinimum: 1}}, Requirements{1, 0}, Requirements{1, 0}, false},
		{"off keeps floors", RequirementInput{Policy: off, Tier: TierHigh, PathHumanApprovals: 4, Floors: Floors{SeatMinimum: 1, MergeGate: true}}, Requirements{1, 1}, Requirements{1, 1}, false},
		{"record calculates but does not enforce", RequirementInput{Policy: record, Tier: TierHigh, Floors: Floors{SeatMinimum: 1}}, Requirements{1, 0}, Requirements{3, 2}, false},
		{"enforce low", RequirementInput{Policy: enforce, Tier: TierLow, Floors: Floors{SeatMinimum: 1}}, Requirements{1, 0}, Requirements{1, 0}, true},
		{"enforce high", RequirementInput{Policy: enforce, Tier: TierHigh, Floors: Floors{SeatMinimum: 1}}, Requirements{3, 2}, Requirements{3, 2}, true},
		{"seat floor above tier", RequirementInput{Policy: enforce, Tier: TierMedium, Floors: Floors{SeatMinimum: 4}}, Requirements{4, 1}, Requirements{4, 1}, true},
		{"merge gate gives one approval at low", RequirementInput{Policy: enforce, Tier: TierLow, Floors: Floors{SeatMinimum: 1, MergeGate: true}}, Requirements{1, 1}, Requirements{1, 1}, true},
		{"path approvals above tier", RequirementInput{Policy: enforce, Tier: TierMedium, PathHumanApprovals: 3, Floors: Floors{SeatMinimum: 1}}, Requirements{2, 3}, Requirements{2, 3}, true},
		{"unknown tier counts as high", RequirementInput{Policy: enforce, Tier: "", Floors: Floors{SeatMinimum: 1}}, Requirements{3, 2}, Requirements{3, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculateRequirements(tc.in)
			if got.Effective != tc.effective || got.Calculated != tc.calculated || got.Enforced != tc.enforced {
				t.Fatalf("CalculateRequirements=%+v; want effective %+v calculated %+v enforced %v", got, tc.effective, tc.calculated, tc.enforced)
			}
			floor := Requirements{AgentReviews: tc.in.Floors.SeatMinimum}
			if tc.in.Floors.MergeGate {
				floor.HumanApprovals = 1
			}
			if got.Effective.AgentReviews < floor.AgentReviews || got.Effective.HumanApprovals < floor.HumanApprovals {
				t.Fatalf("requirements %+v fell below the frozen floors %+v", got.Effective, floor)
			}
		})
	}
	// Raising the tier never lowers a requirement.
	for i := 1; i < len(Tiers); i++ {
		lower := CalculateRequirements(RequirementInput{Policy: enforce, Tier: Tiers[i-1], Floors: Floors{SeatMinimum: 1}})
		higher := CalculateRequirements(RequirementInput{Policy: enforce, Tier: Tiers[i], Floors: Floors{SeatMinimum: 1}})
		if higher.Effective.AgentReviews < lower.Effective.AgentReviews || higher.Effective.HumanApprovals < lower.Effective.HumanApprovals {
			t.Fatalf("%s requires less than %s", Tiers[i], Tiers[i-1])
		}
	}
}

// component-risk-review Boundary: the package performs no I/O. Its non-test
// imports are limited to pure packages.
func TestPackagePerformsNoIO(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"bytes": true, "errors": true, "fmt": true, "io": true, "regexp": true, "sort": true, "strings": true,
		"github.com/kidus-tiliksew/conveyor/internal/governedpath": true, "gopkg.in/yaml.v3": true,
	}
	for _, imported := range pkg.Imports {
		if !allowed[imported] {
			t.Fatalf("internal/riskpolicy imports %s; the package must stay pure", imported)
		}
	}
}
