package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Requirements are living intent documents (spec §4.2 item 1, §21.46 change 2).
// A requirement is versioned and confirmed, never gated: every revision — chat
// edit or drift amendment — creates a version an operator confirms, and the
// approval gate stays on blueprints (§13.1). The corpus is flat; there is no
// hierarchy to curate.

var (
	requirementStatementIDPattern = regexp.MustCompile(`^REQ-[1-9][0-9]*$`)
	acceptanceCriterionIDPattern  = regexp.MustCompile(`^AC-([1-9][0-9]*)\.([1-9][0-9]*)$`)
)

// RequirementUserStory is optional framing around a normative statement. It
// remains data, not a replacement for the stable REQ-n statement itself.
type RequirementUserStory struct {
	AsA    string `yaml:"as_a" json:"as_a"`
	IWant  string `yaml:"i_want" json:"i_want"`
	SoThat string `yaml:"so_that" json:"so_that"`
}

// AcceptanceCriterion is one independently citable, parent-qualified outcome.
type AcceptanceCriterion struct {
	ID        string `yaml:"id" json:"id"`
	Statement string `yaml:"statement" json:"statement"`
}

// RequirementOrigin records why a version exists. Origin is provenance, not
// authority: no origin confirms itself.
type RequirementOrigin string

const (
	// RequirementOriginChat is a planning-session revision (spec §9).
	RequirementOriginChat RequirementOrigin = "chat"
	// RequirementOriginDriftAmendment is the monitor's requirements_amended
	// reconciliation outcome proposing a version (spec §4.2 item 2).
	RequirementOriginDriftAmendment RequirementOrigin = "drift_amendment"
	// RequirementOriginFeatureMigration seeds the corpus from a retired
	// feature-tree node. Seeds carry the node's accumulated text verbatim and
	// stay pending until an operator confirms them (spec §21.46 change 2).
	RequirementOriginFeatureMigration RequirementOrigin = "feature_migration"
)

func (o RequirementOrigin) Valid() bool {
	return o == RequirementOriginChat || o == RequirementOriginDriftAmendment ||
		o == RequirementOriginFeatureMigration
}

// RequirementStatement is one enumerable statement carrying a stable ID that
// acceptance criteria, verdicts, and in-repo citations can reference. A REQ-n
// outlives every blueprint that serves it, so IDs are never recycled.
type RequirementStatement struct {
	ID                 string                `yaml:"id" json:"id"`
	Statement          string                `yaml:"statement" json:"statement"`
	UserStory          *RequirementUserStory `yaml:"user_story,omitempty" json:"user_story,omitempty"`
	AcceptanceCriteria []AcceptanceCriterion `yaml:"acceptance_criteria,omitempty" json:"acceptance_criteria,omitempty"`
}

// ServedRequirementContext is the confirmed intent a task reaches through
// canonical lineage. It is supplied to implement/review role contracts so
// REQ-n citations are checked against durable authority, not guessed context.
type ServedRequirementContext struct {
	ID         string                 `json:"id"`
	Title      string                 `json:"title"`
	Version    int                    `json:"version"`
	Statements []RequirementStatement `json:"statements"`
}

// RequirementCitationAssessment is the reviewer's structured assessment of
// implementation citations. Findings are observational review evidence; they
// never create lineage or alter confirmed requirement authority (spec §4.2 item 4).
type RequirementCitationAssessment struct {
	Applicable  bool     `json:"applicable"`
	CitedIDs    []string `json:"cited_ids"`
	UnknownIDs  []string `json:"unknown_ids"`
	UnservedIDs []string `json:"unserved_ids"`
	Conflicts   []string `json:"conflicts"`
}

// Requirement is the document identity. Prose and statements live on versions;
// the row records only which version is currently confirmed.
type Requirement struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// CurrentVersion is zero until an operator confirms a first version.
	CurrentVersion int `json:"current_version,omitempty"`
	// StatementHighWaterMark is the largest REQ-n ever used by this document.
	// It is monotonic so a retired statement's ID is never reissued to a
	// different statement in a later revision.
	StatementHighWaterMark int       `json:"statement_high_water_mark"`
	Workspace              string    `json:"workspace"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// RequirementVersion is an immutable proposed or confirmed revision.
type RequirementVersion struct {
	RequirementID   string                 `json:"requirement_id"`
	Version         int                    `json:"version"`
	Content         string                 `json:"content"`
	Statements      []RequirementStatement `json:"statements"`
	Origin          RequirementOrigin      `json:"origin"`
	OriginSessionID string                 `json:"origin_session_id,omitempty"`
	OriginDriftID   string                 `json:"origin_drift_id,omitempty"`
	DerivedFrom     *RequirementDerivation `json:"derived_from,omitempty"`
	Confirmed       bool                   `json:"confirmed"`
	ConfirmedBy     string                 `json:"confirmed_by,omitempty"`
	ConfirmedAt     time.Time              `json:"confirmed_at,omitempty"`
	Workspace       string                 `json:"workspace"`
	CreatedAt       time.Time              `json:"created_at"`
}

// RequirementDerivation retains version-scoped informative provenance without
// granting the upload normative authority or a citation identity.
type RequirementDerivation struct {
	DocumentID    string `json:"document_id"`
	Version       int    `json:"version"`
	SectionAnchor string `json:"section_anchor"`
	TargetID      string `json:"target_id"`
}

// RequirementServesState is the operator-owned lifecycle of a proposed
// blueprint-to-requirement relationship (spec §4.2 item 1). Machinery may
// propose a relation, but only a human confirmation makes it authoritative.
type RequirementServesState string

const (
	RequirementServesProposed  RequirementServesState = "proposed"
	RequirementServesConfirmed RequirementServesState = "confirmed"
	RequirementServesDismissed RequirementServesState = "dismissed"
)

func (s RequirementServesState) Valid() bool {
	return s == RequirementServesProposed || s == RequirementServesConfirmed ||
		s == RequirementServesDismissed
}

// RequirementServesSource records which control-plane path proposed a serves
// relationship. It is provenance, never confirmation authority.
type RequirementServesSource string

const (
	RequirementServesPlanning RequirementServesSource = "planning"
	RequirementServesTriage   RequirementServesSource = "triage"
	RequirementServesOperator RequirementServesSource = "operator"
)

func (s RequirementServesSource) Valid() bool {
	return s == RequirementServesPlanning || s == RequirementServesTriage ||
		s == RequirementServesOperator
}

// RequirementServesLink is the auditable, workspace-scoped projection of one
// blueprint serving one requirement. Event IDs retain exact provenance; actor
// fields make terminal operator decisions directly renderable.
type RequirementServesLink struct {
	BlueprintTaskID  string                  `json:"blueprint_task_id"`
	RequirementID    string                  `json:"requirement_id"`
	State            RequirementServesState  `json:"state"`
	Source           RequirementServesSource `json:"source"`
	CreatedByEventID int64                   `json:"created_by_event_id"`
	DecisionEventID  int64                   `json:"decision_event_id,omitempty"`
	ProposedBy       string                  `json:"proposed_by"`
	DecidedBy        string                  `json:"decided_by,omitempty"`
	Workspace        string                  `json:"workspace"`
	CreatedAt        time.Time               `json:"created_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
}

// PlanningSessionStatus is the durable session lifecycle (spec §9).
type PlanningSessionStatus string

const (
	PlanningSessionActive    PlanningSessionStatus = "active"
	PlanningSessionFinalized PlanningSessionStatus = "finalized"
	PlanningSessionAbandoned PlanningSessionStatus = "abandoned"
)

func (s PlanningSessionStatus) Valid() bool {
	return s == PlanningSessionActive || s == PlanningSessionFinalized ||
		s == PlanningSessionAbandoned
}

// PlanningSessionGoal is the artifact a session declares it is working toward
// (spec §9, §21.57). It is set at creation and never updated: it steers the
// agent's finalize target so a requirement-drafting conversation does not skip
// to a blueprint. `open` keeps the historical unconstrained behavior.
type PlanningSessionGoal string

const (
	PlanningGoalRequirement PlanningSessionGoal = "requirement"
	PlanningGoalBlueprint   PlanningSessionGoal = "blueprint"
	PlanningGoalOpen        PlanningSessionGoal = "open"
)

func (g PlanningSessionGoal) Valid() bool {
	return g == PlanningGoalRequirement || g == PlanningGoalBlueprint ||
		g == PlanningGoalOpen
}

// NormalizePlanningSessionGoal defaults an absent goal to `open` — which is
// exactly how pre-goal rows read — and rejects anything outside the three. It
// is the single spelling of that rule for every layer that accepts a goal
// (spec §21.57 change 3).
func NormalizePlanningSessionGoal(goal PlanningSessionGoal) (PlanningSessionGoal, error) {
	if goal == "" {
		return PlanningGoalOpen, nil
	}
	if !goal.Valid() {
		return "", fmt.Errorf(
			"planning session goal %q is invalid; want requirement, blueprint, or open", goal)
	}
	return goal, nil
}

// ProvisionalTitle names a session before it has produced anything. It is a
// per-goal label, so concurrently drafting sessions do share it until one
// finalizes and adopts its artifact's title (spec §21.57 change 3).
func (g PlanningSessionGoal) ProvisionalTitle() string {
	switch g {
	case PlanningGoalRequirement:
		return "Drafting requirement…"
	case PlanningGoalBlueprint:
		return "Planning work…"
	default:
		return "Exploring…"
	}
}

// PlanningSession is a durable planning chat. It produces at most one artifact
// — a requirement version or a blueprint parent task — and grants no approval
// authority over either (spec §9, §13.1).
type PlanningSession struct {
	ID     string                `json:"id"`
	Title  string                `json:"title,omitempty"`
	Status PlanningSessionStatus `json:"status"`
	// Goal is immutable declared intent (spec §21.57). Pre-goal rows read back
	// as `open`, which is exactly their historical behavior.
	Goal PlanningSessionGoal `json:"goal"`
	// Model, Effort, ExplorationOutputTokens, and PinnedRevisions are the
	// immutable serving provenance selected when the session starts. Existing
	// pre-provenance rows remain readable with zero values (spec §21.50-§21.52).
	Model                   string            `json:"model,omitempty"`
	Effort                  string            `json:"effort,omitempty"`
	ExplorationOutputTokens int               `json:"exploration_output_tokens,omitempty"`
	ExplorationTokensUsed   int               `json:"exploration_tokens_used,omitempty"`
	PrimaryRepo             string            `json:"primary_repo,omitempty"`
	PinnedRevisions         map[string]string `json:"pinned_revisions,omitempty"`
	// RequirementContextID is set when the session was opened from a
	// requirement ("Plan work"), which is what auto-proposes a serves link.
	RequirementContextID string `json:"requirement_context_id,omitempty"`
	// ProducedRequirementID and ProducedTaskID are mutually exclusive.
	ProducedRequirementID string    `json:"produced_requirement_id,omitempty"`
	ProducedTaskID        string    `json:"produced_task_id,omitempty"`
	TranscriptArtifactID  string    `json:"transcript_artifact_id,omitempty"`
	Workspace             string    `json:"workspace"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	FinalizedAt           time.Time `json:"finalized_at,omitempty"`
}

// PlanningMessageRole mirrors the AI SDK message roles the transport restores.
type PlanningMessageRole string

const (
	PlanningMessageUser      PlanningMessageRole = "user"
	PlanningMessageAssistant PlanningMessageRole = "assistant"
	PlanningMessageSystem    PlanningMessageRole = "system"
	PlanningMessageTool      PlanningMessageRole = "tool"
)

func (r PlanningMessageRole) Valid() bool {
	return r == PlanningMessageUser || r == PlanningMessageAssistant ||
		r == PlanningMessageSystem || r == PlanningMessageTool
}

// PlanningMessage is one persisted turn. Parts holds the AI SDK UI-message
// parts verbatim so a restored session renders exactly what streamed,
// including tool activity, without re-deriving it from Content.
type PlanningMessage struct {
	SessionID string              `json:"session_id"`
	Seq       int                 `json:"seq"`
	Role      PlanningMessageRole `json:"role"`
	Content   string              `json:"content"`
	Parts     json.RawMessage     `json:"parts,omitempty"`
	Workspace string              `json:"workspace"`
	CreatedAt time.Time           `json:"created_at"`
}

// RequirementStatementNumber returns the n in REQ-n.
func RequirementStatementNumber(id string) (int, bool) {
	if !requirementStatementIDPattern.MatchString(id) {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(id, "REQ-"))
	if err != nil {
		return 0, false
	}
	return number, true
}

// AcceptanceCriterionNumber returns parent n and child m in AC-n.m.
func AcceptanceCriterionNumber(id string) (int, int, bool) {
	match := acceptanceCriterionIDPattern.FindStringSubmatch(id)
	if len(match) != 3 {
		return 0, 0, false
	}
	parent, parentErr := strconv.Atoi(match[1])
	child, childErr := strconv.Atoi(match[2])
	return parent, child, parentErr == nil && childErr == nil
}

// RequirementStatementIDs returns every normative identity carried by one
// statement, including its independently citable children.
func RequirementStatementIDs(statement RequirementStatement) []string {
	ids := []string{statement.ID}
	for _, criterion := range statement.AcceptanceCriteria {
		ids = append(ids, criterion.ID)
	}
	return ids
}

// ValidateRequirementStatements enforces the shape of one statement block.
// Prose is deliberately unconstrained (spec §4.2 item 1) — only the machine
// block is validated.
func ValidateRequirementStatements(statements []RequirementStatement) error {
	seen := map[string]bool{}
	for index, statement := range statements {
		if _, ok := RequirementStatementNumber(statement.ID); !ok {
			return fmt.Errorf("requirement statement %d has invalid id %q; want REQ-n", index+1, statement.ID)
		}
		if seen[statement.ID] {
			return fmt.Errorf("requirement statements contain duplicate id %q", statement.ID)
		}
		seen[statement.ID] = true
		if strings.TrimSpace(statement.Statement) == "" {
			return fmt.Errorf("requirement statement %s is empty", statement.ID)
		}
		if story := statement.UserStory; story != nil {
			if strings.TrimSpace(story.AsA) == "" || strings.TrimSpace(story.IWant) == "" || strings.TrimSpace(story.SoThat) == "" {
				return fmt.Errorf("requirement statement %s user_story requires non-empty as_a, i_want, and so_that", statement.ID)
			}
		}
		parent, _ := RequirementStatementNumber(statement.ID)
		criteria := map[string]bool{}
		for criterionIndex, criterion := range statement.AcceptanceCriteria {
			criterionParent, _, ok := AcceptanceCriterionNumber(criterion.ID)
			if !ok {
				return fmt.Errorf("requirement statement %s acceptance criterion %d has invalid id %q; want AC-%d.m", statement.ID, criterionIndex+1, criterion.ID, parent)
			}
			if criterionParent != parent {
				return fmt.Errorf("acceptance criterion %s belongs under REQ-%d, not %s", criterion.ID, criterionParent, statement.ID)
			}
			if criteria[criterion.ID] {
				return fmt.Errorf("requirement statement %s contains duplicate acceptance criterion id %q", statement.ID, criterion.ID)
			}
			criteria[criterion.ID] = true
			if strings.TrimSpace(criterion.Statement) == "" {
				return fmt.Errorf("acceptance criterion %s is empty", criterion.ID)
			}
		}
	}
	return nil
}

// ValidateRequirementRevision enforces REQ-n stability across revisions
// against the document's monotonic high-water mark. A carried-over ID may be
// reworded — that is what revising intent means — but an ID that was never
// issued must be greater than every ID the document has ever used, so a
// retired statement's identity is never reassigned.
//
// issuedIDs is every REQ-n the document has ever carried, not just its latest
// version's. Scoping it to the immediate predecessor would strand a document
// that dropped a statement in an unconfirmed proposal: reinstating that same
// statement from the still-confirmed text would look like reuse of a retired
// identifier, and because versions cannot be discarded there would be no way
// out. An ID belongs to its statement for the document's whole life, so the
// check that matters is whether the ID is new to the *document*.
func ValidateRequirementRevision(highWaterMark int, issuedIDs []string, next []RequirementStatement) error {
	if err := ValidateRequirementStatements(next); err != nil {
		return err
	}
	issued := map[string]bool{}
	acHighWater := map[int]int{}
	for _, id := range issuedIDs {
		issued[id] = true
		if parent, child, ok := AcceptanceCriterionNumber(id); ok && child > acHighWater[parent] {
			acHighWater[parent] = child
		}
	}
	for _, statement := range next {
		if !issued[statement.ID] {
			number, _ := RequirementStatementNumber(statement.ID)
			if number <= highWaterMark {
				return fmt.Errorf("requirement statement %s reuses a retired identifier; new statements must exceed REQ-%d", statement.ID, highWaterMark)
			}
		}
		for _, criterion := range statement.AcceptanceCriteria {
			if issued[criterion.ID] {
				continue
			}
			parent, child, _ := AcceptanceCriterionNumber(criterion.ID)
			if child <= acHighWater[parent] {
				return fmt.Errorf("acceptance criterion %s reuses a retired identifier; new criteria under REQ-%d must exceed AC-%d.%d", criterion.ID, parent, parent, acHighWater[parent])
			}
		}
	}
	return nil
}

// ValidateRequirementOrigin enforces that a version names the act that
// produced it. Provenance is what makes a pending version auditable (spec
// §4.2 item 1): a chat revision carries its planning session and a
// requirements_amended revision carries its drift record, so every proposal an
// operator is asked to confirm traces back to something they can open. The two
// identifiers are exclusive — a version has exactly one origin.
func ValidateRequirementOrigin(version RequirementVersion) error {
	if !version.Origin.Valid() {
		return fmt.Errorf("invalid requirement origin %q", version.Origin)
	}
	session := strings.TrimSpace(version.OriginSessionID)
	drift := strings.TrimSpace(version.OriginDriftID)
	switch version.Origin {
	case RequirementOriginChat:
		if session == "" {
			return fmt.Errorf("requirement origin %s requires the planning session that revised it", version.Origin)
		}
		if drift != "" {
			return fmt.Errorf("requirement origin %s must not carry a drift id", version.Origin)
		}
	case RequirementOriginDriftAmendment:
		if drift == "" {
			return fmt.Errorf("requirement origin %s requires the drift record that amended it", version.Origin)
		}
		if session != "" {
			return fmt.Errorf("requirement origin %s must not carry a planning session id", version.Origin)
		}
	case RequirementOriginFeatureMigration:
		// A seed is produced by migration 046, not by a session or a drift.
		if session != "" || drift != "" {
			return fmt.Errorf("requirement origin %s carries no session or drift id", version.Origin)
		}
	}
	return nil
}

// RequirementStatementHighWaterMark returns the largest REQ-n in a block.
func RequirementStatementHighWaterMark(statements []RequirementStatement) int {
	highest := 0
	for _, statement := range statements {
		if number, ok := RequirementStatementNumber(statement.ID); ok && number > highest {
			highest = number
		}
	}
	return highest
}

// ConfirmableRequirementVersion reports whether a proposed version may become
// the confirmed intent. A migration seed is deliberately allowed to carry no
// statements so the retired node's text survives verbatim without Conveyor
// inventing intent; confirmation is where a real statement block is required.
func ConfirmableRequirementVersion(version RequirementVersion) error {
	if strings.TrimSpace(version.Content) == "" {
		return fmt.Errorf("requirement version %d has no prose", version.Version)
	}
	if len(version.Statements) == 0 {
		return fmt.Errorf("requirement version %d has no REQ-n statements to confirm", version.Version)
	}
	return ValidateRequirementStatements(version.Statements)
}

var (
	requirementSlugStripPattern = regexp.MustCompile(`[^a-z0-9]+`)
	requirementSlugTrimPattern  = regexp.MustCompile(`^-+|-+$`)
)

// RequirementSlug derives the stable, human-readable document handle. Callers
// own collision resolution because the corpus is workspace-unique by slug.
func RequirementSlug(title string) string {
	slug := requirementSlugStripPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), "-")
	slug = requirementSlugTrimPattern.ReplaceAllString(slug, "")
	if slug == "" {
		return "requirement"
	}
	// The strip pattern has already reduced the slug to [a-z0-9-], so bytes and
	// runes coincide and a byte bound cannot split a character.
	const maxSlugBytes = 80
	if len(slug) > maxSlugBytes {
		slug = requirementSlugTrimPattern.ReplaceAllString(slug[:maxSlugBytes], "")
	}
	return slug
}

// RequirementSlugCandidate returns the deterministic candidate for one
// workspace-local allocation attempt. Ordinal one keeps the bare slug; later
// attempts append -n while preserving the 80-byte storage contract.
func RequirementSlugCandidate(base string, ordinal int) string {
	base = RequirementSlug(base)
	if ordinal <= 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(ordinal)
	limit := 80 - len(suffix)
	if limit < 1 {
		return ""
	}
	if len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	if base == "" {
		return ""
	}
	return base + suffix
}
