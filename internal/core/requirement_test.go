package core

import (
	"strings"
	"testing"
)

func TestRequirementStatementNumberRejectsUnstableIDForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
		want int
		ok   bool
	}{
		{name: "single digit", id: "REQ-1", want: 1, ok: true},
		{name: "multi digit", id: "REQ-42", want: 42, ok: true},
		// REQ-0 and REQ-01 are rejected so a statement has exactly one spelling:
		// the high-water-mark comparison is numeric, and two spellings of the
		// same number would let a retired identity be reissued.
		{name: "zero", id: "REQ-0"},
		{name: "leading zero", id: "REQ-01"},
		{name: "lowercase prefix", id: "req-1"},
		{name: "no number", id: "REQ-"},
		{name: "empty", id: ""},
		{name: "trailing garbage", id: "REQ-1a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			number, ok := RequirementStatementNumber(test.id)
			if ok != test.ok || number != test.want {
				t.Fatalf("RequirementStatementNumber(%q) = %d, %t; want %d, %t", test.id, number, ok, test.want, test.ok)
			}
		})
	}
}

func TestValidateRequirementStatements(t *testing.T) {
	t.Parallel()
	good := []RequirementStatement{
		{ID: "REQ-1", Statement: "Operators confirm every requirement version."},
		{ID: "REQ-2", Statement: "Statement IDs are never recycled."},
	}
	if err := ValidateRequirementStatements(good); err != nil {
		t.Fatalf("valid statement block rejected: %v", err)
	}
	// An empty block is deliberately valid: migration 046 seeds carry a retired
	// feature node's text with no statements, and confirmation — not this
	// shape check — is where a real block becomes mandatory.
	if err := ValidateRequirementStatements(nil); err != nil {
		t.Fatalf("empty statement block rejected: %v", err)
	}

	tests := []struct {
		name       string
		statements []RequirementStatement
		want       string
	}{
		{
			name:       "invalid id",
			statements: []RequirementStatement{{ID: "REQ-01", Statement: "Ship it."}},
			want:       `invalid id "REQ-01"`,
		},
		{
			name: "duplicate id",
			statements: []RequirementStatement{
				{ID: "REQ-1", Statement: "Ship it."},
				{ID: "REQ-1", Statement: "Ship it twice."},
			},
			want: `duplicate id "REQ-1"`,
		},
		{
			// Whitespace-only text would give a citable ID nothing to cite.
			name:       "whitespace-only statement",
			statements: []RequirementStatement{{ID: "REQ-1", Statement: "   \n\t"}},
			want:       "REQ-1 is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRequirementStatements(test.statements)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRequirementStatementsV2(t *testing.T) {
	statements := []RequirementStatement{{
		ID: "REQ-2", Statement: "Operators can verify delivery.",
		UserStory: &RequirementUserStory{AsA: "release operator", IWant: "a durable check", SoThat: "delivery is reviewable"},
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "AC-2.1", Statement: "When delivery finishes, the system shall retain its evidence."},
			{ID: "AC-2.2", Statement: "When evidence is opened, the system shall show its origin."},
		},
	}}
	if err := ValidateRequirementStatements(statements); err != nil {
		t.Fatalf("v2 statements rejected: %v", err)
	}
	for name, mutate := range map[string]func(*RequirementStatement){
		"partial story": func(statement *RequirementStatement) { statement.UserStory.SoThat = "" },
		"wrong parent":  func(statement *RequirementStatement) { statement.AcceptanceCriteria[0].ID = "AC-3.1" },
		"empty AC":      func(statement *RequirementStatement) { statement.AcceptanceCriteria[0].Statement = " " },
		"duplicate AC":  func(statement *RequirementStatement) { statement.AcceptanceCriteria[1].ID = "AC-2.1" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := statements[0]
			story := *copy.UserStory
			copy.UserStory = &story
			copy.AcceptanceCriteria = append([]AcceptanceCriterion(nil), copy.AcceptanceCriteria...)
			mutate(&copy)
			if err := ValidateRequirementStatements([]RequirementStatement{copy}); err == nil {
				t.Fatal("invalid v2 statement accepted")
			}
		})
	}
}

func TestValidateRequirementRevisionDoesNotRecycleAcceptanceCriteria(t *testing.T) {
	next := []RequirementStatement{{ID: "REQ-1", Statement: "Stable.", AcceptanceCriteria: []AcceptanceCriterion{{ID: "AC-1.2", Statement: "Reused."}}}}
	if err := ValidateRequirementRevision(1, []string{"REQ-1", "AC-1.1", "AC-1.3"}, next); err == nil || !strings.Contains(err.Error(), "reuses a retired identifier") {
		t.Fatalf("recycled AC result = %v", err)
	}
	next[0].AcceptanceCriteria[0].ID = "AC-1.4"
	if err := ValidateRequirementRevision(1, []string{"REQ-1", "AC-1.1", "AC-1.3"}, next); err != nil {
		t.Fatalf("monotonic AC rejected: %v", err)
	}
}

func TestValidateRequirementRevisionKeepsStatementIdentityStable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		highWaterMark int
		issuedIDs     []string
		next          []RequirementStatement
		wantErr       bool
	}{
		{
			// Rewording is what revising intent means; the ID stays put.
			name:          "carried-over id reworded",
			highWaterMark: 2,
			issuedIDs:     []string{"REQ-1", "REQ-2"},
			next: []RequirementStatement{
				{ID: "REQ-1", Statement: "Reworded but the same intent."},
				{ID: "REQ-2", Statement: "Also reworded."},
			},
		},
		{
			name:          "new id above the mark",
			highWaterMark: 2,
			issuedIDs:     []string{"REQ-1", "REQ-2"},
			next: []RequirementStatement{
				{ID: "REQ-1", Statement: "Kept."},
				{ID: "REQ-3", Statement: "Added."},
			},
		},
		{
			// A never-issued ID at or below the mark would hand a retired
			// statement's identity to different intent, breaking every citation.
			name:          "never-issued id at the mark",
			highWaterMark: 5,
			issuedIDs:     []string{"REQ-1"},
			next:          []RequirementStatement{{ID: "REQ-5", Statement: "Squatting on a retired id."}},
			wantErr:       true,
		},
		{
			name:          "never-issued id below the mark",
			highWaterMark: 5,
			issuedIDs:     []string{"REQ-1"},
			next:          []RequirementStatement{{ID: "REQ-3", Statement: "Squatting on a retired id."}},
			wantErr:       true,
		},
		{
			// The regression this rule was fixed for: issuedIDs spans every
			// version the document ever had, not just its latest. A proposal
			// that dropped REQ-2 must not strand the document — reinstating
			// REQ-2 from the still-confirmed text is a carry-over, not reuse.
			name:          "reinstating an id the latest version dropped",
			highWaterMark: 2,
			issuedIDs:     []string{"REQ-1", "REQ-2"},
			next: []RequirementStatement{
				{ID: "REQ-1", Statement: "Kept throughout."},
				{ID: "REQ-2", Statement: "Reinstated after an unconfirmed proposal dropped it."},
			},
		},
		{
			// Gap-filling: REQ-2 was never used even though the mark is 4.
			name:          "gap-filling a never-used id below the mark",
			highWaterMark: 4,
			issuedIDs:     []string{"REQ-1", "REQ-3", "REQ-4"},
			next:          []RequirementStatement{{ID: "REQ-2", Statement: "Filling a gap."}},
			wantErr:       true,
		},
		{
			name:          "statement shape still enforced",
			highWaterMark: 0,
			next:          []RequirementStatement{{ID: "REQ-0", Statement: "Invalid id."}},
			wantErr:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRequirementRevision(test.highWaterMark, test.issuedIDs, test.next)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateRequirementRevision err = %v, wantErr = %t", err, test.wantErr)
			}
		})
	}
}

func TestValidateRequirementOriginRequiresExactlyOneProvenance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version RequirementVersion
		wantErr bool
	}{
		{
			name:    "chat with session",
			version: RequirementVersion{Origin: RequirementOriginChat, OriginSessionID: "session-1"},
		},
		{
			// A pending chat version an operator cannot trace back to a session
			// is not auditable.
			name:    "chat without session",
			version: RequirementVersion{Origin: RequirementOriginChat},
			wantErr: true,
		},
		{
			name:    "chat carrying a drift id",
			version: RequirementVersion{Origin: RequirementOriginChat, OriginSessionID: "session-1", OriginDriftID: "drift-1"},
			wantErr: true,
		},
		{
			name:    "drift amendment with drift",
			version: RequirementVersion{Origin: RequirementOriginDriftAmendment, OriginDriftID: "drift-1"},
		},
		{
			name:    "drift amendment without drift",
			version: RequirementVersion{Origin: RequirementOriginDriftAmendment},
			wantErr: true,
		},
		{
			name:    "drift amendment carrying a session id",
			version: RequirementVersion{Origin: RequirementOriginDriftAmendment, OriginDriftID: "drift-1", OriginSessionID: "session-1"},
			wantErr: true,
		},
		{
			// A seed is produced by the migration itself, so neither identifier
			// exists to name.
			name:    "feature migration with neither",
			version: RequirementVersion{Origin: RequirementOriginFeatureMigration},
		},
		{
			name:    "feature migration with a session id",
			version: RequirementVersion{Origin: RequirementOriginFeatureMigration, OriginSessionID: "session-1"},
			wantErr: true,
		},
		{
			name:    "feature migration with a drift id",
			version: RequirementVersion{Origin: RequirementOriginFeatureMigration, OriginDriftID: "drift-1"},
			wantErr: true,
		},
		{
			name:    "operator with neither",
			version: RequirementVersion{Origin: RequirementOriginOperator},
		},
		{
			name:    "operator with a session id",
			version: RequirementVersion{Origin: RequirementOriginOperator, OriginSessionID: "session-1"},
			wantErr: true,
		},
		{
			name:    "operator with a drift id",
			version: RequirementVersion{Origin: RequirementOriginOperator, OriginDriftID: "drift-1"},
			wantErr: true,
		},
		{
			name:    "unknown origin",
			version: RequirementVersion{Origin: RequirementOrigin("operator_edit"), OriginSessionID: "session-1"},
			wantErr: true,
		},
		{
			name:    "empty origin",
			version: RequirementVersion{},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRequirementOrigin(test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateRequirementOrigin(%+v) err = %v, wantErr = %t", test.version, err, test.wantErr)
			}
		})
	}
}

func TestConfirmableRequirementVersionDemandsProseAndStatements(t *testing.T) {
	t.Parallel()
	valid := RequirementVersion{
		Version:    2,
		Content:    "Operators own intent.",
		Statements: []RequirementStatement{{ID: "REQ-1", Statement: "Every version is confirmed."}},
	}
	if err := ConfirmableRequirementVersion(valid); err != nil {
		t.Fatalf("confirmable version rejected: %v", err)
	}

	tests := []struct {
		name    string
		version RequirementVersion
		want    string
	}{
		{
			name:    "no prose",
			version: RequirementVersion{Version: 1, Statements: valid.Statements},
			want:    "has no prose",
		},
		{
			// A migration seed may carry zero statements, but confirmation is
			// exactly where the statement block stops being optional (§21.46).
			name:    "no statements",
			version: RequirementVersion{Version: 1, Content: "Seeded verbatim from a retired feature node."},
			want:    "no REQ-n statements to confirm",
		},
		{
			name:    "invalid statement",
			version: RequirementVersion{Version: 1, Content: "Intent.", Statements: []RequirementStatement{{ID: "REQ-1", Statement: " "}}},
			want:    "REQ-1 is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ConfirmableRequirementVersion(test.version)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRequirementSlugDerivesAStableHandle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "lowercases", title: "Planning Sessions", want: "planning-sessions"},
		{name: "collapses runs of punctuation", title: "REQ  ids -- never   recycled!", want: "req-ids-never-recycled"},
		{name: "trims edges", title: "  ***Living intent***  ", want: "living-intent"},
		// Multi-byte titles reduce to their ASCII-alphanumeric runs rather than
		// leaking raw bytes into a URL-bearing handle.
		{name: "unicode", title: "Café Ünicode Requirement", want: "caf-nicode-requirement"},
		{name: "reduces to nothing", title: "— ✅ —", want: "requirement"},
		{name: "empty", title: "", want: "requirement"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := RequirementSlug(test.title); got != test.want {
				t.Fatalf("RequirementSlug(%q) = %q, want %q", test.title, got, test.want)
			}
		})
	}

	// The bound is a storage contract, and truncation must not leave a dangling
	// separator that reads as a partial word.
	long := RequirementSlug(strings.Repeat("word ", 30))
	if len(long) > 80 {
		t.Fatalf("RequirementSlug length = %d, want <= 80: %q", len(long), long)
	}
	if strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Fatalf("truncated slug has a dangling dash: %q", long)
	}
	if !strings.HasPrefix(long, "word-word-") {
		t.Fatalf("truncated slug lost its leading words: %q", long)
	}
}

func TestRequirementSlugCandidatePreservesBoundWhileDisambiguating(t *testing.T) {
	if got := RequirementSlugCandidate("Auth", 1); got != "auth" {
		t.Fatalf("first candidate=%q", got)
	}
	if got := RequirementSlugCandidate("Auth", 2); got != "auth-2" {
		t.Fatalf("second candidate=%q", got)
	}
	got := RequirementSlugCandidate(strings.Repeat("x", 80), 123)
	if len(got) != 80 || !strings.HasSuffix(got, "-123") {
		t.Fatalf("bounded candidate=%q len=%d", got, len(got))
	}
}

func TestArtifactValidateAttachmentTargetKeepsOwnerExclusive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		artifact Artifact
		wantErr  bool
	}{
		// An artifact may float at workspace scope with no owner at all.
		{name: "unattached", artifact: Artifact{}},
		{name: "task only", artifact: Artifact{TaskID: "task-1"}},
		{name: "feature only", artifact: Artifact{FeatureID: "feature-1"}},
		{name: "requirement only", artifact: Artifact{RequirementID: "req-1"}},
		{name: "task and feature", artifact: Artifact{TaskID: "task-1", FeatureID: "feature-1"}, wantErr: true},
		{name: "task and requirement", artifact: Artifact{TaskID: "task-1", RequirementID: "req-1"}, wantErr: true},
		{name: "feature and requirement", artifact: Artifact{FeatureID: "feature-1", RequirementID: "req-1"}, wantErr: true},
		{name: "all three", artifact: Artifact{TaskID: "task-1", FeatureID: "feature-1", RequirementID: "req-1"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.artifact.ValidateAttachmentTarget()
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateAttachmentTarget(%+v) err = %v, wantErr = %t", test.artifact, err, test.wantErr)
			}
		})
	}
}
