package core

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SystemDesign is the stable identity for one factory-resident mechanism
// document. Content and governed scope live on immutable versions
// (design-document-corpus).
type SystemDesign struct {
	ID             string    `json:"id"`
	Slug           string    `json:"slug"`
	Title          string    `json:"title"`
	Category       string    `json:"category"`
	CurrentVersion int       `json:"current_version,omitempty"`
	Workspace      string    `json:"workspace"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type SystemDesignOrigin string

const (
	SystemDesignOriginPlanning       SystemDesignOrigin = "planning_session"
	SystemDesignOriginImplementation SystemDesignOrigin = "implementation_deliberation"
	SystemDesignOriginOperator       SystemDesignOrigin = "operator"
)

func (o SystemDesignOrigin) Valid() bool {
	return o == SystemDesignOriginPlanning || o == SystemDesignOriginImplementation || o == SystemDesignOriginOperator
}

// GovernedScope is one repository and its normalized repository-relative path
// globs. Categories are deliberately operator-named; scopes are the canonical
// component identity used by drift and lineage.
type GovernedScope struct {
	Repository string   `yaml:"repo" json:"repository"`
	Paths      []string `yaml:"paths" json:"paths"`
}

type SystemDesignVersion struct {
	DocumentID      string             `json:"document_id"`
	Version         int                `json:"version"`
	Content         string             `json:"content"`
	Governs         []GovernedScope    `json:"governs"`
	Origin          SystemDesignOrigin `json:"origin"`
	OriginSessionID string             `json:"origin_session_id,omitempty"`
	OriginTaskID    string             `json:"origin_task_id,omitempty"`
	Confirmed       bool               `json:"confirmed"`
	ConfirmedBy     string             `json:"confirmed_by,omitempty"`
	ConfirmedAt     time.Time          `json:"confirmed_at,omitempty"`
	Dismissed       bool               `json:"dismissed"`
	DismissedBy     string             `json:"dismissed_by,omitempty"`
	DismissedAt     time.Time          `json:"dismissed_at,omitempty"`
	Workspace       string             `json:"workspace"`
	CreatedAt       time.Time          `json:"created_at"`
	// Deduplicated is response metadata: proposal calls set it when an
	// equivalent pending implementation proposal already existed. It is not
	// persisted as part of the immutable version.
	Deduplicated bool `json:"deduplicated"`
}

var systemDesignIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateSystemDesignID keeps document identifiers safe in chi route
// segments and unambiguous inside the colon-delimited lineage vocabulary.
func ValidateSystemDesignID(id string) error {
	if !systemDesignIDPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("system design id must use letters, numbers, dot, underscore, or hyphen")
	}
	return nil
}

type SystemDesignVersionDiff struct {
	From SystemDesignVersion `json:"from"`
	To   SystemDesignVersion `json:"to"`
}

var governsFence = regexp.MustCompile("(?s)```conveyor:governs[\\t ]*\\r?\\n(.*?)\\r?\\n```")

// ParseGovernedScopes validates the complete design document and returns the
// sole conveyor:governs fence. Keeping scope derived from Content prevents a
// caller from presenting one scope while persisting another.
func ParseGovernedScopes(content string) ([]GovernedScope, error) {
	matches := governsFence.FindAllStringSubmatch(content, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("system design must contain exactly one conveyor:governs fence")
	}
	var scopes []GovernedScope
	if err := yaml.Unmarshal([]byte(matches[0][1]), &scopes); err != nil {
		return nil, fmt.Errorf("parse conveyor:governs fence: %w", err)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("conveyor:governs must declare at least one repository scope")
	}
	seenRepo := map[string]bool{}
	for i := range scopes {
		scopes[i].Repository = strings.TrimSpace(scopes[i].Repository)
		if scopes[i].Repository == "" || strings.ContainsAny(scopes[i].Repository, "/\\") {
			return nil, fmt.Errorf("governed repository must be a configured repository name")
		}
		if seenRepo[scopes[i].Repository] {
			return nil, fmt.Errorf("governed repository %q is declared more than once", scopes[i].Repository)
		}
		seenRepo[scopes[i].Repository] = true
		if len(scopes[i].Paths) == 0 {
			return nil, fmt.Errorf("governed repository %q requires at least one path glob", scopes[i].Repository)
		}
		seenPath := map[string]bool{}
		for j, glob := range scopes[i].Paths {
			glob = strings.TrimSpace(strings.ReplaceAll(glob, "\\", "/"))
			if glob == "" || strings.HasPrefix(glob, "/") || glob == ".." || strings.HasPrefix(glob, "../") || strings.Contains(glob, "/../") {
				return nil, fmt.Errorf("governed path %q must be repository-relative", glob)
			}
			if strings.ContainsAny(glob, "[]") {
				return nil, fmt.Errorf("governed path glob %q uses unsupported character-class syntax", glob)
			}
			if _, err := path.Match(strings.ReplaceAll(glob, "**", "*"), "validation"); err != nil {
				return nil, fmt.Errorf("invalid governed path glob %q: %w", glob, err)
			}
			if seenPath[glob] {
				return nil, fmt.Errorf("governed path glob %q is duplicated", glob)
			}
			seenPath[glob] = true
			scopes[i].Paths[j] = glob
		}
		sort.Strings(scopes[i].Paths)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Repository < scopes[j].Repository })
	return scopes, nil
}

func NormalizeSystemDesignVersion(version *SystemDesignVersion) error {
	if !version.Origin.Valid() {
		return fmt.Errorf("invalid system design origin %q", version.Origin)
	}
	switch version.Origin {
	case SystemDesignOriginPlanning:
		if strings.TrimSpace(version.OriginSessionID) == "" || version.OriginTaskID != "" {
			return fmt.Errorf("planning system design origin requires only origin_session_id")
		}
	case SystemDesignOriginImplementation:
		if strings.TrimSpace(version.OriginTaskID) == "" || version.OriginSessionID != "" {
			return fmt.Errorf("implementation system design origin requires only origin_task_id")
		}
	case SystemDesignOriginOperator:
		if version.OriginSessionID != "" || version.OriginTaskID != "" {
			return fmt.Errorf("operator system design origin cannot name a session or task")
		}
	}
	version.Content = NormalizeSystemDesignContent(version.Content)
	scopes, err := ParseGovernedScopes(version.Content)
	if err != nil {
		return err
	}
	version.Governs = scopes
	return nil
}

// NormalizeSystemDesignContent defines proposal identity without changing
// interior Markdown: line endings become LF and only document-edge whitespace
// is removed.
func NormalizeSystemDesignContent(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.TrimSpace(content)
}

// MatchGovernedPath implements repository-relative doublestar matching without
// granting absolute or parent-traversing paths. ** spans path separators;
// ordinary * and ? remain segment-local.
func MatchGovernedPath(glob, changedPath string) bool {
	changedPath = strings.TrimPrefix(strings.ReplaceAll(changedPath, "\\", "/"), "./")
	if changedPath == "" || strings.HasPrefix(changedPath, "/") || strings.Contains(changedPath, "../") {
		return false
	}
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteByte('$')
	return regexp.MustCompile(b.String()).MatchString(changedPath)
}

// GovernedDesignMatch carries the deterministic changed paths that place one
// confirmed System Design document in authority for a repository diff.
type GovernedDesignMatch struct {
	Design        GovernanceDesignContext
	MatchingPaths []string
}

// ResolveGovernedDesigns applies the same repository-relative governs fence
// used by drift detection to a submitted branch diff. Document and path order
// are stable so append-only context events are deterministic
// (req-260811-228be6 REQ-5/AC-5.1, AC-5.4).
func ResolveGovernedDesigns(designs []GovernanceDesignContext, repository string, changedPaths []string) []GovernedDesignMatch {
	matches := make([]GovernedDesignMatch, 0)
	for _, design := range designs {
		paths := make([]string, 0)
		for _, scope := range design.Governs {
			if scope.Repository != repository {
				continue
			}
			for _, changed := range changedPaths {
				for _, glob := range scope.Paths {
					if MatchGovernedPath(glob, changed) {
						paths = append(paths, changed)
						break
					}
				}
			}
		}
		if len(paths) == 0 {
			continue
		}
		sort.Strings(paths)
		paths = slices.Compact(paths)
		matches = append(matches, GovernedDesignMatch{Design: design, MatchingPaths: paths})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Design.ID < matches[j].Design.ID })
	return matches
}

type DecisionOrigin string

const (
	DecisionOriginPlanning       DecisionOrigin = "planning_session"
	DecisionOriginImplementation DecisionOrigin = "implementation_deliberation"
	DecisionOriginOperator       DecisionOrigin = "operator"
)

type DecisionStatus string

const (
	DecisionProposed   DecisionStatus = "proposed"
	DecisionConfirmed  DecisionStatus = "confirmed"
	DecisionDismissed  DecisionStatus = "dismissed"
	DecisionSuperseded DecisionStatus = "superseded"
)

type DecisionSupersessionSweepStatus string

const (
	DecisionSweepOpen                  DecisionSupersessionSweepStatus = "open"
	DecisionSweepDismissed             DecisionSupersessionSweepStatus = "dismissed"
	DecisionSweepAutoCleared           DecisionSupersessionSweepStatus = "auto_cleared"
	DecisionSweepTierRequirement                                       = "requirement"
	DecisionSweepTierSystemDesign                                      = "system_design"
	DecisionSweepTierReferenceDocument                                 = "reference_document"
)

type DecisionSupersessionSweepEntry struct {
	DecisionID           string                          `json:"decision_id"`
	SupersededDecisionID string                          `json:"superseded_decision_id"`
	DocumentID           string                          `json:"document_id"`
	DocumentTier         string                          `json:"document_tier"`
	Status               DecisionSupersessionSweepStatus `json:"status"`
	DetectedBy           string                          `json:"detected_by"`
	DetectedAt           time.Time                       `json:"detected_at"`
	ResolvedBy           string                          `json:"resolved_by,omitempty"`
	ResolvedAt           time.Time                       `json:"resolved_at,omitempty"`
}

type DecisionSupersessionSweep struct {
	Clean   bool                             `json:"clean"`
	Entries []DecisionSupersessionSweepEntry `json:"entries"`
}

type Decision struct {
	ID                   string                    `json:"id"`
	Statement            string                    `json:"statement"`
	Context              string                    `json:"context"`
	AlternativesRejected string                    `json:"alternatives_rejected"`
	Status               DecisionStatus            `json:"status"`
	Origin               DecisionOrigin            `json:"origin"`
	OriginSessionID      string                    `json:"origin_session_id,omitempty"`
	OriginTaskID         string                    `json:"origin_task_id,omitempty"`
	Supersedes           string                    `json:"supersedes,omitempty"`
	ConfirmedBy          string                    `json:"confirmed_by,omitempty"`
	ConfirmedAt          time.Time                 `json:"confirmed_at,omitempty"`
	DismissedBy          string                    `json:"dismissed_by,omitempty"`
	DismissedAt          time.Time                 `json:"dismissed_at,omitempty"`
	SupersededBy         string                    `json:"superseded_by,omitempty"`
	Sweep                DecisionSupersessionSweep `json:"sweep"`
	Workspace            string                    `json:"workspace"`
	CreatedAt            time.Time                 `json:"created_at"`
}

// ContainsDecisionToken applies the corpus whole-token rule. Decision IDs use
// ASCII word characters around their numeric suffix, so regexp word boundaries
// distinguish DEC-1 from DEC-18 without interpreting adjacent prose as a cite.
func ContainsDecisionToken(content, decisionID string) bool {
	if !decisionIDPattern.MatchString(decisionID) {
		return false
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(decisionID) + `\b`).MatchString(content)
}

// PendingProposal is the workspace-level, read-only projection of authority an
// operator still owes the factory. It is derived from immutable proposal rows;
// it is never pipeline state and never gates work (REQ-1, REQ-3).
type PendingProposal struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Tier          string    `json:"tier"`
	Version       int       `json:"version,omitempty"`
	OriginType    string    `json:"origin_type"`
	OriginID      string    `json:"origin_id,omitempty"`
	TargetKind    string    `json:"target_kind,omitempty"`
	Justification string    `json:"justification,omitempty"`
	ProposedAt    time.Time `json:"proposed_at"`
}

// GovernanceDesignContext is the immutable portion of a confirmed System
// Design version rendered to and validated for one review claim.
type GovernanceDesignContext struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	Category           string          `json:"category"`
	Version            int             `json:"version"`
	Content            string          `json:"content"`
	Governs            []GovernedScope `json:"governs"`
	PinnedAtAttachment bool            `json:"pinned_at_attachment,omitempty"`
}

// PendingSystemDesignProposal is observable review/implementation context,
// never governance authority. ProposalEventID identifies the append-only event
// that created the immutable version. Confirmed distinguishes proposal evidence
// that an operator confirmed after the task proposed it; dismissed versions are
// never included.
type PendingSystemDesignProposal struct {
	DocumentID      string `json:"document_id"`
	Version         int    `json:"version"`
	ProposalEventID int64  `json:"proposal_event_id"`
	OriginTaskID    string `json:"origin_task_id"`
	Confirmed       bool   `json:"confirmed,omitempty"`
}

// GovernanceSnapshot pins repository-scoped design authority and workspace-wide
// decision authority. Its separately non-authoritative task-origin proposal
// observations refresh at review claim time. Non-nil empty slices distinguish
// a current empty snapshot from a legacy missing pin.
type GovernanceSnapshot struct {
	Designs                []GovernanceDesignContext     `json:"designs"`
	Decisions              []Decision                    `json:"decisions"`
	PendingDesignProposals []PendingSystemDesignProposal `json:"pending_design_proposals"`
	ResolutionNotes        []string                      `json:"resolution_notes,omitempty"`
}

var decisionIDPattern = regexp.MustCompile(`^DEC-[1-9][0-9]*$`)

func ValidateDecision(decision Decision) error {
	if decision.ID != "" && !decisionIDPattern.MatchString(decision.ID) {
		return fmt.Errorf("decision id must use DEC-n")
	}
	if decision.ID != "" {
		ordinal, err := strconv.ParseInt(strings.TrimPrefix(decision.ID, "DEC-"), 10, 32)
		if err != nil || ordinal < 1 {
			return fmt.Errorf("decision id numeric part must fit int32")
		}
	}
	if strings.TrimSpace(decision.Statement) == "" || strings.TrimSpace(decision.Context) == "" || strings.TrimSpace(decision.AlternativesRejected) == "" {
		return fmt.Errorf("decision statement, context, and alternatives_rejected are required")
	}
	if decision.Origin != DecisionOriginPlanning && decision.Origin != DecisionOriginImplementation && decision.Origin != DecisionOriginOperator {
		return fmt.Errorf("invalid decision origin %q", decision.Origin)
	}
	if decision.Origin == DecisionOriginPlanning && decision.OriginSessionID == "" {
		return fmt.Errorf("planning decision origin requires origin_session_id")
	}
	if decision.Origin == DecisionOriginImplementation && decision.OriginTaskID == "" {
		return fmt.Errorf("implementation decision origin requires origin_task_id")
	}
	return nil
}

// GovernanceAssessment is the separate design/decision review contract. The
// lists classify distinct outcomes and are therefore normalized and disjoint.
type GovernanceAssessment struct {
	// Applicable is the legacy wire field. New callers use the two independent
	// fields below; when supplied, applicable must agree with design_applicable.
	Applicable       *bool    `json:"applicable,omitempty"`
	DesignApplicable *bool    `json:"design_applicable,omitempty"`
	DecisionCitable  *bool    `json:"decision_citable,omitempty"`
	CitedIDs         []string `json:"cited_ids"`
	UnknownIDs       []string `json:"unknown_ids"`
	UngovernedIDs    []string `json:"ungoverned_ids"`
	SupersededIDs    []string `json:"superseded_ids"`
	Conflicts        []string `json:"conflicts"`
	legacyApplicable bool
}

func (value *GovernanceAssessment) UnmarshalJSON(data []byte) error {
	type wire GovernanceAssessment
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = GovernanceAssessment(decoded)
	value.legacyApplicable = value.Applicable != nil && value.DesignApplicable == nil && value.DecisionCitable == nil
	return nil
}

// UsesLegacyApplicable reports that the payload supplied only the historical
// applicability bit. Validation maps that bit to design scope and derives
// decision citability from the pinned authority for wire compatibility.
func (value *GovernanceAssessment) UsesLegacyApplicable() bool {
	return value != nil && value.legacyApplicable
}

func NormalizeGovernanceAssessment(value *GovernanceAssessment) error {
	if value == nil {
		return nil
	}
	if value.DesignApplicable == nil && value.DecisionCitable == nil {
		if value.Applicable == nil {
			return fmt.Errorf("governance assessment requires design_applicable and decision_citable (or legacy applicable)")
		}
		value.legacyApplicable = true
		design, decision := *value.Applicable, *value.Applicable
		value.DesignApplicable, value.DecisionCitable = &design, &decision
	} else if value.DesignApplicable == nil || value.DecisionCitable == nil {
		return fmt.Errorf("governance assessment requires both design_applicable and decision_citable")
	}
	if value.Applicable != nil && *value.Applicable != *value.DesignApplicable {
		return fmt.Errorf("governance assessment legacy applicable must match design_applicable")
	}
	lists := []*[]string{&value.CitedIDs, &value.UnknownIDs, &value.UngovernedIDs, &value.SupersededIDs, &value.Conflicts}
	seen := map[string]int{}
	for i, list := range lists {
		unique := map[string]bool{}
		out := make([]string, 0, len(*list))
		for _, raw := range *list {
			item := strings.TrimSpace(raw)
			if item == "" || unique[item] {
				continue
			}
			if prior, exists := seen[item]; exists && prior != i {
				return fmt.Errorf("governance assessment finding %q appears in more than one category", item)
			}
			seen[item], unique[item] = i, true
			out = append(out, item)
		}
		sort.Strings(out)
		*list = out
	}
	return nil
}

func SystemDesignVersionLineageID(id string, version int) string {
	return fmt.Sprintf("%s:v%d", id, version)
}
func RepoPathComponentLineageID(repository, glob string) string { return repository + ":" + glob }
