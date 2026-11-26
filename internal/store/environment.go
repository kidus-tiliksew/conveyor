package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// Environment event kinds (component-environments, Events). Each is a
// taskless workspace event.
const (
	EnvironmentCreatedEvent           = "environment.created"
	EnvironmentUpdatedEvent           = "environment.updated"
	EnvironmentArchivedEvent          = "environment.archived"
	EnvironmentRestoredEvent          = "environment.restored"
	EnvironmentKeyIssuedEvent         = "environment.key_issued"
	EnvironmentKeyRevokedEvent        = "environment.key_revoked"
	EnvironmentSuggestionIgnoredEvent = "environment.suggestion_ignored"
)

// Environment field bounds (component-environments invariants 3 to 7, 11,
// and 12).
const (
	DefaultEnvironmentTimingWindowMinutes = 60
	MaxEnvironmentTimingWindowMinutes     = 1440
	MaxEnvironmentKeys                    = 5
	environmentNameMaxCharacters          = 40
	environmentRuleMaxCharacters          = 200
	environmentGitHubNameLimit            = 10
	environmentGitHubNameMaxCharacters    = 255
	environmentBranchMaxCharacters        = 255
	environmentKeyLabelMaxCharacters      = 60
)

// EnvironmentKeyPrefix opens every environment key value. No other Conveyor
// credential carries it (component-environments invariant 15).
const EnvironmentKeyPrefix = "cv_env_"

// EnvironmentKeyUseGranularity is how often verification writes a key's
// last use (component-environments invariant 14).
const EnvironmentKeyUseGranularity = time.Minute

const (
	environmentKeyIDPrefix   = "envkey_"
	environmentKeyIDBytes    = 12
	environmentKeySecretSize = 32
)

// Version rule kinds (component-environments invariant 4).
const (
	EnvironmentCommitPattern = "commit_pattern"
	EnvironmentTagTemplate   = "tag_template"
)

// EnvironmentVersionRule says how to read a commit from a version that the
// environment's software states (req-environments AC-1.2).
type EnvironmentVersionRule struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Environment is one environment record without its keys. The JSON form is
// the environment row of component-environments Public surfaces without
// keys, which MCP list_environments serves (invariant 17).
type Environment struct {
	ID                  string                  `json:"id"`
	Workspace           string                  `json:"-"`
	Name                string                  `json:"name"`
	TrackedBranches     map[string]string       `json:"tracked_branches"`
	GitHubEnvironments  []string                `json:"github_environments"`
	VersionRule         *EnvironmentVersionRule `json:"version_rule"`
	TimingWindowMinutes int                     `json:"timing_window_minutes"`
	Archived            bool                    `json:"archived"`
	CreatedBy           string                  `json:"created_by"`
	CreatedAt           time.Time               `json:"created_at"`
}

// EnvironmentKey is a key's readable metadata. It holds no value and no
// hash.
type EnvironmentKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// EnvironmentRecord is an environment with its keys that are not revoked,
// ordered by creation time and then ID. The JSON form is the environment row.
type EnvironmentRecord struct {
	Environment
	Keys []EnvironmentKey `json:"keys"`
}

// IssuedEnvironmentKey is the answer to issuing a key, the only result that
// ever holds the value (component-environments invariant 11).
type IssuedEnvironmentKey struct {
	EnvironmentKey
	Value string `json:"value"`
}

// VerifiedEnvironmentKey is what a verified key value names. It carries no
// user, role, capability, or session (component-environments invariant 15).
// KeyID is stable for the life of the key.
type VerifiedEnvironmentKey struct {
	Workspace     string
	EnvironmentID string
	KeyID         string
	Label         string
}

// EnvironmentSuggestion is a GitHub environment name that GitHub reported
// and that the workspace neither lists nor ignores.
type EnvironmentSuggestion struct {
	GitHubEnvironment string    `json:"github_environment"`
	Repository        string    `json:"repository"`
	LastReportedAt    time.Time `json:"last_reported_at"`
}

// EnvironmentReport is one deployment report of GitHub as a suggestion reads
// it. Repository is the repository's name in the workspace configuration.
type EnvironmentReport struct {
	GitHubEnvironment string
	Repository        string
	ReportedAt        time.Time
}

// EnvironmentInput is a create request. A nil TimingWindowMinutes takes the
// default.
type EnvironmentInput struct {
	Name                string
	TrackedBranches     map[string]string
	GitHubEnvironments  []string
	VersionRule         *EnvironmentVersionRule
	TimingWindowMinutes *int
	Archived            bool
}

// EnvironmentPatch is an edit request. A nil member leaves that field
// unchanged. VersionRuleSet distinguishes an explicit null version rule,
// which clears it, from an omitted one.
type EnvironmentPatch struct {
	Name                *string
	TrackedBranches     *map[string]string
	GitHubEnvironments  *[]string
	VersionRuleSet      bool
	VersionRule         *EnvironmentVersionRule
	TimingWindowMinutes *int
	Archived            *bool
}

// EditsFields reports whether the patch names a field other than archived,
// which an archived environment refuses (component-environments invariant
// 10).
func (p EnvironmentPatch) EditsFields() bool {
	return p.Name != nil || p.TrackedBranches != nil || p.GitHubEnvironments != nil || p.VersionRuleSet || p.TimingWindowMinutes != nil
}

// EnvironmentListQuery selects environments by archived state, or every
// environment when Archived is nil.
type EnvironmentListQuery struct {
	Archived *bool
}

// Matches reports whether the query selects an environment with the given
// archived state.
func (q EnvironmentListQuery) Matches(archived bool) bool {
	return q.Archived == nil || *q.Archived == archived
}

// EnvironmentStore keeps environments, environment keys, and suggested
// environments (component-environments; component-persistence). Every
// mutation with an event runs in one transaction with its validation and its
// one workspace event, so a refused request records nothing. An environment
// of another workspace answers EnvironmentNotFoundError. Mutations of one
// workspace are serialized, so names, GitHub environment names, key slots,
// and suggestion state never race.
type EnvironmentStore interface {
	CreateEnvironment(ctx context.Context, input EnvironmentInput) (EnvironmentRecord, error)
	// UpdateEnvironment edits, archives, or restores one environment. A patch
	// that changes nothing records nothing. A missing record answers first,
	// then an archived record's refusal, and then a field value's.
	UpdateEnvironment(ctx context.Context, id string, patch EnvironmentPatch) (EnvironmentRecord, error)
	// ListEnvironments returns the selected environments without keys,
	// ordered by name without regard to case and then by ID. It reads no key.
	ListEnvironments(ctx context.Context, query EnvironmentListQuery) ([]Environment, error)
	// ListEnvironmentRecords is ListEnvironments with each environment's keys
	// that are not revoked.
	ListEnvironmentRecords(ctx context.Context, query EnvironmentListQuery) ([]EnvironmentRecord, error)
	IssueEnvironmentKey(ctx context.Context, environmentID, label string) (IssuedEnvironmentKey, error)
	// RevokeEnvironmentKey revokes the key for good. A key that is already
	// revoked stays revoked and records nothing. A key the environment never
	// had answers EnvironmentNotFoundError.
	RevokeEnvironmentKey(ctx context.Context, environmentID, keyID string) error
	// VerifyEnvironmentKey resolves a presented value from the value alone,
	// whatever workspace the context names. Every value that is not the value
	// of an unrevoked key of an unarchived environment answers
	// ErrInvalidEnvironmentKey. Any other error is a store failure. It
	// records the key's last use at most once a minute.
	VerifyEnvironmentKey(ctx context.Context, value string) (VerifiedEnvironmentKey, error)
	// CheckEnvironmentKey is VerifyEnvironmentKey without the last use, so it
	// writes nothing. Its caller records the use with what it accepts: a
	// signal's refusal records nothing (component-production-feedback
	// invariant 3).
	CheckEnvironmentKey(ctx context.Context, value string) (VerifiedEnvironmentKey, error)
	// RecordEnvironmentReport records the suggestion a deployment report
	// makes and reports whether it did. A repository outside the workspace
	// configuration, a name an unarchived environment lists, an ignored name,
	// and a report no newer than the stored one record nothing. It appends no
	// event and creates no environment.
	RecordEnvironmentReport(ctx context.Context, report EnvironmentReport) (recorded bool, err error)
	// ListEnvironmentSuggestions returns the suggestions, ordered by name
	// without regard to case.
	ListEnvironmentSuggestions(ctx context.Context) ([]EnvironmentSuggestion, error)
	// IgnoreEnvironmentSuggestion ignores the name until an environment lists
	// it. An already ignored name records nothing.
	IgnoreEnvironmentSuggestion(ctx context.Context, gitHubEnvironment string) error
}

// ErrInvalidEnvironmentKey is the one refusal of a presented key value. It
// does not say which condition failed (component-environments invariant 14).
var ErrInvalidEnvironmentKey = errors.New("invalid environment key")

// EnvironmentNotFoundError names an environment, or a key of one, that is
// missing or in another workspace.
type EnvironmentNotFoundError struct {
	ID    string
	KeyID string
}

func (e *EnvironmentNotFoundError) Error() string {
	if e.KeyID != "" {
		return fmt.Sprintf("environment %s has no key %s", e.ID, e.KeyID)
	}
	return fmt.Sprintf("environment %s was not found", e.ID)
}

// EnvironmentArchivedError names an archived environment that refuses the
// act.
type EnvironmentArchivedError struct{ ID string }

func (e *EnvironmentArchivedError) Error() string {
	return fmt.Sprintf("environment %s is archived", e.ID)
}

// EnvironmentKeyLimitError names an environment that holds the most keys it
// can.
type EnvironmentKeyLimitError struct{ ID string }

func (e *EnvironmentKeyLimitError) Error() string {
	return fmt.Sprintf("environment %s already has %d keys that are not revoked", e.ID, MaxEnvironmentKeys)
}

// EnvironmentFieldError names the request field that holds a value the rules
// refuse (component-environments invariants 3 to 7).
type EnvironmentFieldError struct {
	Field   string
	Message string
}

func (e *EnvironmentFieldError) Error() string { return e.Field + ": " + e.Message }

func environmentFieldError(field, format string, args ...any) error {
	return &EnvironmentFieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// EnvironmentTime is the time every backend records: UTC at the microsecond
// precision both durable backends keep.
func EnvironmentTime(now time.Time) time.Time { return now.UTC().Truncate(time.Microsecond) }

// NewEnvironmentID mints env-<yymmdd>-<6 hex> (feature-production-feedback-live
// FB-1).
func NewEnvironmentID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate environment ID: %w", err)
	}
	return "env-" + time.Now().UTC().Format("060102") + "-" + hex.EncodeToString(b), nil
}

// MintEnvironmentKey mints a key with its value and the SHA-256 of that
// value. A backend stores the hash and never the value.
func MintEnvironmentKey(label, createdBy string, now time.Time) (IssuedEnvironmentKey, []byte, error) {
	random := make([]byte, environmentKeyIDBytes+environmentKeySecretSize)
	if _, err := rand.Read(random); err != nil {
		return IssuedEnvironmentKey{}, nil, fmt.Errorf("generate environment key: %w", err)
	}
	id := environmentKeyIDPrefix + base64.RawURLEncoding.EncodeToString(random[:environmentKeyIDBytes])
	value := EnvironmentKeyPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(random[environmentKeyIDBytes:])
	return IssuedEnvironmentKey{
		EnvironmentKey: EnvironmentKey{ID: id, Label: label, CreatedBy: createdBy, CreatedAt: EnvironmentTime(now)},
		Value:          value,
	}, EnvironmentKeyDigest(value), nil
}

// EnvironmentKeyDigest is the SHA-256 of a presented key value.
func EnvironmentKeyDigest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// EnvironmentFold maps a name to the form that is equal for two names
// exactly when they are equal without regard to case. Backends store and
// compare this form, so no database collation decides uniqueness.
func EnvironmentFold(name string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			lowest = min(lowest, next)
		}
		return lowest
	}, name)
}

// checkEnvironmentText refuses text no durable backend can store: invalid
// UTF-8 and the NUL character.
func checkEnvironmentText(field, value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return environmentFieldError(field, "%s must be UTF-8 text without a NUL character", field)
	}
	return nil
}

// checkEnvironmentSecret refuses text that the shared redactor would change
// or that holds a credential shape, before any write, as
// refuseSecretShapedOAuthMetadata does for client metadata. Clean text is
// stored exactly, and no stored row, read, or event can carry a credential a
// person pasted into a field. The refusal never repeats the text
// (req-security-boundaries REQ-2, REQ-3; component-identity-membership
// invariant 45).
func checkEnvironmentSecret(field, value string) error {
	clean, _ := redact.New(nil).Redact(value)
	if clean != value || redact.HoldsCredentialShape(value) {
		return environmentFieldError(field, "%s must not hold a credential or other secret", field)
	}
	return nil
}

func normalizeEnvironmentText(field, value string, most int) (string, error) {
	if err := checkEnvironmentText(field, value); err != nil {
		return "", err
	}
	if err := checkEnvironmentSecret(field, value); err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if count := utf8.RuneCountInString(value); count < 1 || count > most {
		return "", environmentFieldError(field, "%s must be 1 to %d characters", field, most)
	}
	return value, nil
}

func normalizeEnvironmentName(name string) (string, error) {
	return normalizeEnvironmentText("name", name, environmentNameMaxCharacters)
}

// NormalizeEnvironmentKeyLabel applies the label rule of
// component-environments invariant 11.
func NormalizeEnvironmentKeyLabel(label string) (string, error) {
	return normalizeEnvironmentText("label", label, environmentKeyLabelMaxCharacters)
}

// NormalizeEnvironmentVersionRule applies the version rule's form rules
// (component-environments invariant 4; req-environments AC-1.2). Writers and
// the resolver in internal/feedback share it, so a stored rule is always one
// the resolver can apply.
func NormalizeEnvironmentVersionRule(rule *EnvironmentVersionRule) (*EnvironmentVersionRule, error) {
	return normalizeEnvironmentVersionRule(rule)
}

func normalizeEnvironmentVersionRule(rule *EnvironmentVersionRule) (*EnvironmentVersionRule, error) {
	const field = "version_rule"
	if rule == nil {
		return nil, nil
	}
	if err := checkEnvironmentText(field, rule.Text); err != nil {
		return nil, err
	}
	if err := checkEnvironmentSecret(field, rule.Text); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(rule.Text) > environmentRuleMaxCharacters {
		return nil, environmentFieldError(field, "version_rule text must be at most %d characters", environmentRuleMaxCharacters)
	}
	switch rule.Kind {
	case EnvironmentCommitPattern:
		if strings.Count(rule.Text, "{commit}") != 1 {
			return nil, environmentFieldError(field, "a commit pattern must hold exactly one {commit}")
		}
		// Every other character is literal text or *, so a brace outside the
		// placeholder is a malformed or foreign placeholder (FB-3).
		if strings.ContainsAny(strings.Replace(rule.Text, "{commit}", "", 1), "{}") {
			return nil, environmentFieldError(field, "a commit pattern must hold no placeholder other than {commit}")
		}
	case EnvironmentTagTemplate:
		if strings.Count(rule.Text, "{version}") != 1 || strings.Count(rule.Text, "{service}") > 1 {
			return nil, environmentFieldError(field, "a tag template must hold exactly one {version} and at most one {service}")
		}
		if strings.ContainsAny(strings.NewReplacer("{version}", "", "{service}", "").Replace(rule.Text), "{}") {
			return nil, environmentFieldError(field, "a tag template must hold no placeholder other than {version} and {service}")
		}
		if !environmentTemplateNamesTag(rule.Text) {
			return nil, environmentFieldError(field, "a tag template must name a valid Git tag when its placeholders are replaced")
		}
	default:
		return nil, environmentFieldError(field, "version_rule kind must be %s or %s", EnvironmentCommitPattern, EnvironmentTagTemplate)
	}
	copied := *rule
	return &copied, nil
}

// environmentTemplateNamesTag reports whether a tag template names a valid Git
// tag once each placeholder holds a safe sample value. Text that makes a tag
// invalid for the samples makes it invalid for every value (req-environments
// AC-1.2). The fixed first component leaves the form of a tag, which unlike a
// branch can be named refs/heads/..., and git refuses a tag that opens with -.
func environmentTemplateNamesTag(template string) bool {
	tag := strings.NewReplacer("{version}", "1.2.3", "{service}", "api").Replace(template)
	return !strings.HasPrefix(tag, "-") && gitx.LegalBranchName("tag/"+tag)
}

func checkEnvironmentTimingWindow(minutes int) error {
	if minutes < 1 || minutes > MaxEnvironmentTimingWindowMinutes {
		return environmentFieldError("timing_window_minutes", "timing_window_minutes must be a whole number from 1 to %d", MaxEnvironmentTimingWindowMinutes)
	}
	return nil
}

func normalizeEnvironmentBranches(branches map[string]string) (map[string]string, error) {
	const field = "tracked_branches"
	out := make(map[string]string, len(branches))
	for _, repository := range slices.Sorted(maps.Keys(branches)) {
		branch := branches[repository]
		for _, text := range []string{repository, branch} {
			if err := checkEnvironmentText(field, text); err != nil {
				return nil, err
			}
			if err := checkEnvironmentSecret(field, text); err != nil {
				return nil, err
			}
		}
		if utf8.RuneCountInString(branch) > environmentBranchMaxCharacters || !gitx.LegalBranchName(branch) {
			return nil, environmentFieldError(field, "the branch of repository %q is not a legal branch name of at most %d characters", repository, environmentBranchMaxCharacters)
		}
		out[repository] = branch
	}
	return out, nil
}

// NormalizeEnvironmentGitHubName trims one GitHub environment name and
// applies its length rule. field is the request field the refusal names.
func NormalizeEnvironmentGitHubName(field, name string) (string, error) {
	return normalizeEnvironmentText(field, name, environmentGitHubNameMaxCharacters)
}

// normalizeEnvironmentGitHubNames trims the names and drops a repeat of an
// earlier one, compared without regard to case.
func normalizeEnvironmentGitHubNames(names []string) ([]string, error) {
	const field = "github_environments"
	out := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		name, err := NormalizeEnvironmentGitHubName(field, name)
		if err != nil {
			return nil, err
		}
		if fold := EnvironmentFold(name); !seen[fold] {
			seen[fold] = true
			out = append(out, name)
		}
	}
	if len(out) > environmentGitHubNameLimit {
		return nil, environmentFieldError(field, "github_environments must list at most %d names", environmentGitHubNameLimit)
	}
	return out, nil
}

// NormalizeEnvironmentInput applies the field rules to a create request.
// The rules that need the workspace are CheckEnvironmentRepositories and
// CheckEnvironmentConflicts.
func NormalizeEnvironmentInput(input EnvironmentInput) (EnvironmentInput, error) {
	var err error
	if input.Name, err = normalizeEnvironmentName(input.Name); err != nil {
		return EnvironmentInput{}, err
	}
	if input.TrackedBranches, err = normalizeEnvironmentBranches(input.TrackedBranches); err != nil {
		return EnvironmentInput{}, err
	}
	if input.GitHubEnvironments, err = normalizeEnvironmentGitHubNames(input.GitHubEnvironments); err != nil {
		return EnvironmentInput{}, err
	}
	if input.VersionRule, err = normalizeEnvironmentVersionRule(input.VersionRule); err != nil {
		return EnvironmentInput{}, err
	}
	window := DefaultEnvironmentTimingWindowMinutes
	if input.TimingWindowMinutes != nil {
		window = *input.TimingWindowMinutes
	}
	if err := checkEnvironmentTimingWindow(window); err != nil {
		return EnvironmentInput{}, err
	}
	input.TimingWindowMinutes = &window
	return input, nil
}

// NormalizeEnvironmentPatch applies the field rules to the members an edit
// names.
func NormalizeEnvironmentPatch(patch EnvironmentPatch) (EnvironmentPatch, error) {
	if patch.Name != nil {
		name, err := normalizeEnvironmentName(*patch.Name)
		if err != nil {
			return EnvironmentPatch{}, err
		}
		patch.Name = &name
	}
	if patch.TrackedBranches != nil {
		branches, err := normalizeEnvironmentBranches(*patch.TrackedBranches)
		if err != nil {
			return EnvironmentPatch{}, err
		}
		patch.TrackedBranches = &branches
	}
	if patch.GitHubEnvironments != nil {
		names, err := normalizeEnvironmentGitHubNames(*patch.GitHubEnvironments)
		if err != nil {
			return EnvironmentPatch{}, err
		}
		patch.GitHubEnvironments = &names
	}
	if patch.VersionRuleSet {
		rule, err := normalizeEnvironmentVersionRule(patch.VersionRule)
		if err != nil {
			return EnvironmentPatch{}, err
		}
		patch.VersionRule = rule
	} else {
		patch.VersionRule = nil
	}
	if patch.TimingWindowMinutes != nil {
		if err := checkEnvironmentTimingWindow(*patch.TimingWindowMinutes); err != nil {
			return EnvironmentPatch{}, err
		}
	}
	return patch, nil
}

// NormalizeEnvironmentReport applies the field rules to a deployment report.
func NormalizeEnvironmentReport(report EnvironmentReport) (EnvironmentReport, error) {
	var err error
	if report.GitHubEnvironment, err = NormalizeEnvironmentGitHubName("github_environment", report.GitHubEnvironment); err != nil {
		return EnvironmentReport{}, err
	}
	// The repository is stored with the suggestion, so it meets the same rule.
	if err := checkEnvironmentText("repository", report.Repository); err != nil {
		return EnvironmentReport{}, err
	}
	if err := checkEnvironmentSecret("repository", report.Repository); err != nil {
		return EnvironmentReport{}, err
	}
	if report.ReportedAt.IsZero() {
		return EnvironmentReport{}, environmentFieldError("reported_at", "reported_at is required")
	}
	report.ReportedAt = EnvironmentTime(report.ReportedAt)
	return report, nil
}

// EnvironmentRepositories returns the repository names of a stored workspace
// configuration document.
func EnvironmentRepositories(configYAML string) (map[string]bool, error) {
	document, err := config.DecodeStoredWorkspaceDocument([]byte(configYAML))
	if err != nil {
		return nil, fmt.Errorf("decode stored workspace config: %w", err)
	}
	names := make(map[string]bool, len(document.Repos))
	for _, repository := range document.Repos {
		names[repository.Name] = true
	}
	return names, nil
}

// CheckEnvironmentRepositories refuses tracked branches that name a
// repository outside the workspace configuration (component-environments
// invariant 6).
func CheckEnvironmentRepositories(branches map[string]string, repositories map[string]bool) error {
	for _, repository := range slices.Sorted(maps.Keys(branches)) {
		if !repositories[repository] {
			return environmentFieldError("tracked_branches", "%q is not a repository of the workspace", repository)
		}
	}
	return nil
}

// CheckEnvironmentConflicts refuses a name or a GitHub environment name that
// another unarchived environment of the workspace holds
// (component-environments invariants 3 and 7). environments can hold the
// candidate itself and archived environments, which never conflict.
func CheckEnvironmentConflicts(candidate Environment, environments []Environment) error {
	name := EnvironmentFold(candidate.Name)
	listed := make(map[string]string, len(candidate.GitHubEnvironments))
	for _, github := range candidate.GitHubEnvironments {
		listed[EnvironmentFold(github)] = github
	}
	others := slices.Clone(environments)
	SortEnvironments(others)
	for _, other := range others {
		if other.ID == candidate.ID || other.Archived {
			continue
		}
		if EnvironmentFold(other.Name) == name {
			return environmentFieldError("name", "environment %q already has this name", other.Name)
		}
	}
	for _, other := range others {
		if other.ID == candidate.ID || other.Archived {
			continue
		}
		for _, github := range other.GitHubEnvironments {
			if mine, taken := listed[EnvironmentFold(github)]; taken {
				return environmentFieldError("github_environments", "environment %q already lists GitHub environment %q", other.Name, mine)
			}
		}
	}
	return nil
}

// EnvironmentListing returns the unarchived environment that lists the
// GitHub environment name, compared without regard to case.
func EnvironmentListing(environments []Environment, github string) (Environment, bool) {
	fold := EnvironmentFold(github)
	for _, environment := range environments {
		if environment.Archived {
			continue
		}
		for _, name := range environment.GitHubEnvironments {
			if EnvironmentFold(name) == fold {
				return environment, true
			}
		}
	}
	return Environment{}, false
}

// CheckEnvironmentNameUnlisted refuses to ignore a GitHub environment name
// that an unarchived environment lists, because no suggestion of it exists.
func CheckEnvironmentNameUnlisted(environments []Environment, github string) error {
	if environment, listed := EnvironmentListing(environments, github); listed {
		return environmentFieldError("github_environment", "environment %q lists GitHub environment %q", environment.Name, github)
	}
	return nil
}

// NewEnvironment builds the record of a normalized create request.
func NewEnvironment(workspace, createdBy string, input EnvironmentInput, now time.Time) (Environment, error) {
	id, err := NewEnvironmentID()
	if err != nil {
		return Environment{}, err
	}
	return CloneEnvironment(Environment{
		ID: id, Workspace: workspace, Name: input.Name, TrackedBranches: input.TrackedBranches, GitHubEnvironments: input.GitHubEnvironments,
		VersionRule: input.VersionRule, TimingWindowMinutes: *input.TimingWindowMinutes, Archived: input.Archived, CreatedBy: createdBy, CreatedAt: EnvironmentTime(now),
	}), nil
}

// CloneEnvironment copies the record with a non-nil map and slice.
func CloneEnvironment(environment Environment) Environment {
	branches := make(map[string]string, len(environment.TrackedBranches))
	maps.Copy(branches, environment.TrackedBranches)
	environment.TrackedBranches = branches
	environment.GitHubEnvironments = append([]string{}, environment.GitHubEnvironments...)
	if environment.VersionRule != nil {
		rule := *environment.VersionRule
		environment.VersionRule = &rule
	}
	return environment
}

// SortEnvironments orders environments by name without regard to case and
// then by ID.
func SortEnvironments(environments []Environment) {
	sort.SliceStable(environments, func(i, j int) bool {
		a, b := EnvironmentFold(environments[i].Name), EnvironmentFold(environments[j].Name)
		if a != b {
			return a < b
		}
		return environments[i].ID < environments[j].ID
	})
}

// SortEnvironmentKeys orders keys by creation time and then ID.
func SortEnvironmentKeys(keys []EnvironmentKey) {
	sort.SliceStable(keys, func(i, j int) bool {
		if !keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
			return keys[i].CreatedAt.Before(keys[j].CreatedAt)
		}
		return keys[i].ID < keys[j].ID
	})
}

// SortEnvironmentSuggestions orders suggestions by name without regard to
// case.
func SortEnvironmentSuggestions(suggestions []EnvironmentSuggestion) {
	sort.SliceStable(suggestions, func(i, j int) bool {
		return EnvironmentFold(suggestions[i].GitHubEnvironment) < EnvironmentFold(suggestions[j].GitHubEnvironment)
	})
}

// EnvironmentRecords pairs each environment with its keys. The environments
// keep their order.
func EnvironmentRecords(environments []Environment, keys map[string][]EnvironmentKey) []EnvironmentRecord {
	records := make([]EnvironmentRecord, len(environments))
	for i, environment := range environments {
		own := append([]EnvironmentKey{}, keys[environment.ID]...)
		SortEnvironmentKeys(own)
		records[i] = EnvironmentRecord{Environment: environment, Keys: own}
	}
	return records
}

// EnvironmentChange is the old and new value of one changed field.
type EnvironmentChange struct {
	Old any `json:"old"`
	New any `json:"new"`
}

// EnvironmentEventPayload is the payload of an environment event. The
// workspace ID scopes the event on the volatile backend, which keeps
// workspace events without a task in one list.
type EnvironmentEventPayload struct {
	WorkspaceID   string                       `json:"workspace_id"`
	EnvironmentID string                       `json:"environment_id"`
	Changes       map[string]EnvironmentChange `json:"changes"`
}

// EnvironmentKeyEventPayload is the payload of a key event. It holds no key
// value and no hash (component-environments invariant 9).
type EnvironmentKeyEventPayload struct {
	WorkspaceID   string `json:"workspace_id"`
	EnvironmentID string `json:"environment_id"`
	KeyID         string `json:"key_id"`
	Label         string `json:"label"`
}

// EnvironmentSuggestionEventPayload is the payload of a suggestion event.
type EnvironmentSuggestionEventPayload struct {
	WorkspaceID       string                       `json:"workspace_id"`
	GitHubEnvironment string                       `json:"github_environment"`
	Changes           map[string]EnvironmentChange `json:"changes"`
}

// EnvironmentCreatedPayload records every field of a new environment as a
// change from null.
func EnvironmentCreatedPayload(environment Environment) json.RawMessage {
	environment = CloneEnvironment(environment)
	return core.JSONPayload(EnvironmentEventPayload{WorkspaceID: environment.Workspace, EnvironmentID: environment.ID, Changes: map[string]EnvironmentChange{
		"name":                  {New: environment.Name},
		"tracked_branches":      {New: environment.TrackedBranches},
		"github_environments":   {New: environment.GitHubEnvironments},
		"version_rule":          {New: environment.VersionRule},
		"timing_window_minutes": {New: environment.TimingWindowMinutes},
		"archived":              {New: environment.Archived},
	}})
}

// EnvironmentKeyPayload is the payload of a key's issue or revocation.
func EnvironmentKeyPayload(workspace, environmentID, keyID, label string) json.RawMessage {
	return core.JSONPayload(EnvironmentKeyEventPayload{WorkspaceID: workspace, EnvironmentID: environmentID, KeyID: keyID, Label: label})
}

// EnvironmentSuggestionIgnoredPayload is the payload of an ignored name.
func EnvironmentSuggestionIgnoredPayload(workspace, github string) json.RawMessage {
	return core.JSONPayload(EnvironmentSuggestionEventPayload{WorkspaceID: workspace, GitHubEnvironment: github, Changes: map[string]EnvironmentChange{
		"ignored": {Old: false, New: true},
	}})
}

// CheckEnvironmentEditable refuses a patch that names a field other than
// archived on an archived environment (component-environments invariant 10).
// A backend runs it on the record it holds under its write guard, before
// NormalizeEnvironmentPatch, so the refusal does not depend on the values the
// patch carries.
func CheckEnvironmentEditable(current Environment, patch EnvironmentPatch) error {
	if current.Archived && patch.EditsFields() {
		return &EnvironmentArchivedError{ID: current.ID}
	}
	return nil
}

// ApplyEnvironmentPatch applies a normalized patch to the current
// environment and returns the next environment, the event kind, and the
// event payload. An empty kind means the patch changes nothing and records
// nothing.
func ApplyEnvironmentPatch(current Environment, patch EnvironmentPatch) (Environment, string, json.RawMessage) {
	current = CloneEnvironment(current)
	next := CloneEnvironment(current)
	changes := map[string]EnvironmentChange{}
	if patch.Name != nil && *patch.Name != current.Name {
		next.Name = *patch.Name
		changes["name"] = EnvironmentChange{Old: current.Name, New: next.Name}
	}
	if patch.TrackedBranches != nil && !maps.Equal(*patch.TrackedBranches, current.TrackedBranches) {
		next.TrackedBranches = maps.Clone(*patch.TrackedBranches)
		changes["tracked_branches"] = EnvironmentChange{Old: current.TrackedBranches, New: next.TrackedBranches}
	}
	if patch.GitHubEnvironments != nil && !slices.Equal(*patch.GitHubEnvironments, current.GitHubEnvironments) {
		next.GitHubEnvironments = slices.Clone(*patch.GitHubEnvironments)
		changes["github_environments"] = EnvironmentChange{Old: current.GitHubEnvironments, New: next.GitHubEnvironments}
	}
	if patch.VersionRuleSet && !sameEnvironmentRule(patch.VersionRule, current.VersionRule) {
		next.VersionRule = patch.VersionRule
		changes["version_rule"] = EnvironmentChange{Old: current.VersionRule, New: next.VersionRule}
	}
	if patch.TimingWindowMinutes != nil && *patch.TimingWindowMinutes != current.TimingWindowMinutes {
		next.TimingWindowMinutes = *patch.TimingWindowMinutes
		changes["timing_window_minutes"] = EnvironmentChange{Old: current.TimingWindowMinutes, New: next.TimingWindowMinutes}
	}
	if len(changes) == 0 && (patch.Archived == nil || *patch.Archived == current.Archived) {
		return current, "", nil
	}
	kind := EnvironmentUpdatedEvent
	if patch.Archived != nil && *patch.Archived != current.Archived {
		next.Archived = *patch.Archived
		changes["archived"] = EnvironmentChange{Old: current.Archived, New: next.Archived}
		kind = EnvironmentRestoredEvent
		if next.Archived {
			kind = EnvironmentArchivedEvent
		}
	}
	next = CloneEnvironment(next)
	return next, kind, core.JSONPayload(EnvironmentEventPayload{WorkspaceID: current.Workspace, EnvironmentID: current.ID, Changes: changes})
}

func sameEnvironmentRule(a, b *EnvironmentVersionRule) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// EnvironmentRow is the stored form of the fields a durable backend
// keeps as text: the fold of the name, and the branches and GitHub
// environment names as JSON.
type EnvironmentRow struct {
	NameFold           string
	TrackedBranches    string
	GitHubEnvironments string
	RuleKind, RuleText *string
}

// NewEnvironmentRow renders an environment for a durable backend's row.
func NewEnvironmentRow(environment Environment) EnvironmentRow {
	environment = CloneEnvironment(environment)
	columns := EnvironmentRow{
		NameFold:           EnvironmentFold(environment.Name),
		TrackedBranches:    string(core.JSONPayload(environment.TrackedBranches)),
		GitHubEnvironments: string(core.JSONPayload(environment.GitHubEnvironments)),
	}
	if rule := environment.VersionRule; rule != nil {
		columns.RuleKind, columns.RuleText = &rule.Kind, &rule.Text
	}
	return columns
}

// ApplyEnvironmentRow reads the stored form back into an environment.
func ApplyEnvironmentRow(environment *Environment, branches, names []byte, ruleKind, ruleText *string) error {
	if err := json.Unmarshal(branches, &environment.TrackedBranches); err != nil {
		return fmt.Errorf("decode environment tracked branches: %w", err)
	}
	if err := json.Unmarshal(names, &environment.GitHubEnvironments); err != nil {
		return fmt.Errorf("decode environment GitHub names: %w", err)
	}
	if ruleKind != nil && ruleText != nil {
		environment.VersionRule = &EnvironmentVersionRule{Kind: *ruleKind, Text: *ruleText}
	}
	*environment = CloneEnvironment(*environment)
	return nil
}
