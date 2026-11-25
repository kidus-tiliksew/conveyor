package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// Signal bounds (component-production-feedback invariant 3).
const (
	MinFeedbackSignalSeconds     = 10
	MaxFeedbackSignalSeconds     = 86400
	MaxFeedbackServices          = 200
	MaxFeedbackVersions          = 10
	feedbackNameMaxCharacters    = 100
	feedbackVersionMaxCharacters = 200
	feedbackCommitMinCharacters  = 7
	feedbackCommitMaxCharacters  = 40
)

// FeedbackSourceSignal is the source kind of a change that a watcher signal
// made. GitHub is the other one, and nothing records it yet
// (component-production-feedback invariant 6).
const FeedbackSourceSignal = "signal"

// FeedbackVersion is one version exactly as it was reported. An empty member
// was not stated. Two versions of a service are the same version only when
// all three members are equal byte for byte.
type FeedbackVersion struct {
	Repository string
	Commit     string
	Version    string
}

// Digest identifies the version within its service. The JSON array keeps the
// members apart, so no database collation, padding, or delimiter can make two
// versions equal.
func (v FeedbackVersion) Digest() string {
	raw, _ := json.Marshal([]string{v.Repository, v.Commit, v.Version})
	return feedbackDigest(raw)
}

// FeedbackServiceDigest identifies a service within its environment.
func FeedbackServiceDigest(name string) string { return feedbackDigest([]byte(name)) }

func feedbackDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FeedbackSignalVersion is one version entry of a signal.
type FeedbackSignalVersion struct {
	Commit  string
	Version string
}

// FeedbackSignalService is one service entry of a signal.
type FeedbackSignalService struct {
	Name       string
	Repository string
	Versions   []FeedbackSignalVersion
}

// FeedbackSignal is one signal. EnvironmentID and KeyID come from the
// verified key and ArrivedAt is the server's time of arrival, so nothing in
// the body chooses a scope or a time (component-production-feedback
// invariants 2 and 4).
type FeedbackSignal struct {
	EnvironmentID     string
	KeyID             string
	ArrivedAt         time.Time
	NextSignalSeconds int
	Services          []FeedbackSignalService
}

// FeedbackOrigin says what made a change: the source, the key's label for a
// signal, and the actor (component-production-feedback invariant 6;
// req-environments AC-2.4).
type FeedbackOrigin struct {
	Source string
	Label  string
	Actor  string
}

// FeedbackRunningVersion is one version a service runs. ReportedAt is the
// arrival of the newest signal that named the service. A version is stored
// unresolved.
type FeedbackRunningVersion struct {
	EnvironmentID string
	Service       string
	FeedbackVersion
	DeployID   string
	DeployedAt time.Time
	ReportedAt time.Time
	Resolved   bool
}

// FeedbackDeploy is one deploy record. StoppedAt and StopOrigin are nil while
// the service still runs the version.
type FeedbackDeploy struct {
	ID            string
	EnvironmentID string
	Service       string
	FeedbackVersion
	Resolved   bool
	DeployedAt time.Time
	Origin     FeedbackOrigin
	StoppedAt  *time.Time
	StopOrigin *FeedbackOrigin
}

// FeedbackSignalState is the newest accepted signal of one environment.
type FeedbackSignalState struct {
	EnvironmentID     string
	LastSignalAt      time.Time
	NextSignalSeconds int
	Origin            FeedbackOrigin
}

// Listening reports whether the environment is listening at now: until twice
// the newest signal's interval has passed (component-production-feedback
// invariant 13; req-production-feedback AC-2.1).
func (s FeedbackSignalState) Listening(now time.Time) bool {
	return now.Before(s.LastSignalAt.Add(2 * time.Duration(s.NextSignalSeconds) * time.Second))
}

// FeedbackStore keeps the running versions, the deploy records, and the
// newest signal of each environment (component-production-feedback;
// component-persistence). It appends no event and changes no task, work
// order, or notice (invariant 1; DEC-149(1)).
type FeedbackStore interface {
	// RecordFeedbackSignal records one signal in one transaction: its deploys
	// and stops, the newest signal, and the key's last use at most once a
	// minute. Signals of one environment are serialized. A key that is
	// revoked, or whose environment is archived, by the time of the write
	// answers ErrInvalidEnvironmentKey, and a value the rules refuse answers
	// FeedbackFieldError. Either records nothing.
	RecordFeedbackSignal(ctx context.Context, signal FeedbackSignal) error
	// ListFeedbackSignals returns the newest signal of each named environment
	// that has one, by environment ID.
	ListFeedbackSignals(ctx context.Context, environmentIDs []string) (map[string]FeedbackSignalState, error)
	// ListFeedbackRunningVersions returns what an environment's services run,
	// ordered by service and then by version.
	ListFeedbackRunningVersions(ctx context.Context, environmentID string) ([]FeedbackRunningVersion, error)
	// ListFeedbackDeploys returns an environment's deploy records, oldest
	// first and then by service and version.
	ListFeedbackDeploys(ctx context.Context, environmentID string) ([]FeedbackDeploy, error)
}

// FeedbackFieldError names the field of a signal, as a path, that holds a
// value the rules refuse. Neither member repeats a submitted value.
type FeedbackFieldError struct {
	Field   string
	Message string
}

func (e *FeedbackFieldError) Error() string { return e.Field + ": " + e.Message }

func feedbackFieldError(field, format string, args ...any) error {
	return &FeedbackFieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// checkFeedbackText refuses text outside its bounds, text no durable backend
// can store, and text that the shared redactor would change or that holds a
// credential shape. The refusal never repeats the text
// (component-production-feedback invariant 3; req-security-boundaries REQ-2,
// REQ-3).
func checkFeedbackText(redactor *redact.Redactor, field, value string, most int) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return feedbackFieldError(field, "must be UTF-8 text without a NUL character")
	}
	if count := utf8.RuneCountInString(value); count < 1 || count > most {
		return feedbackFieldError(field, "must be 1 to %d characters", most)
	}
	if clean, _ := redactor.Redact(value); clean != value || redact.HoldsCredentialShape(value) {
		return feedbackFieldError(field, "must not hold a credential or other secret")
	}
	return nil
}

func feedbackCommitValid(commit string) bool {
	if len(commit) < feedbackCommitMinCharacters || len(commit) > feedbackCommitMaxCharacters {
		return false
	}
	return strings.Trim(commit, "0123456789abcdefABCDEF") == ""
}

// FeedbackServiceField and FeedbackVersionField are the paths that a refusal
// names.
func FeedbackServiceField(service int, member string) string {
	return fmt.Sprintf("services[%d].%s", service, member)
}

func FeedbackVersionField(service, version int, member string) string {
	return fmt.Sprintf("services[%d].versions[%d].%s", service, version, member)
}

// NormalizeFeedbackSignal applies the rules of component-production-feedback
// invariant 3 that need no stored state. Names, commits, and versions are
// kept exactly as sent. CheckFeedbackRepositories is the rule that needs the
// workspace.
func NormalizeFeedbackSignal(signal FeedbackSignal) (FeedbackSignal, error) {
	if signal.ArrivedAt.IsZero() {
		return FeedbackSignal{}, fmt.Errorf("a feedback signal needs its arrival time")
	}
	signal.ArrivedAt = EnvironmentTime(signal.ArrivedAt)
	if signal.NextSignalSeconds < MinFeedbackSignalSeconds || signal.NextSignalSeconds > MaxFeedbackSignalSeconds {
		return FeedbackSignal{}, feedbackFieldError("next_signal_seconds", "must be a whole number from %d to %d", MinFeedbackSignalSeconds, MaxFeedbackSignalSeconds)
	}
	if len(signal.Services) < 1 || len(signal.Services) > MaxFeedbackServices {
		return FeedbackSignal{}, feedbackFieldError("services", "must hold 1 to %d entries", MaxFeedbackServices)
	}
	redactor := redact.New(nil)
	seen := make(map[string]bool, len(signal.Services))
	for i, service := range signal.Services {
		if err := checkFeedbackText(redactor, FeedbackServiceField(i, "name"), service.Name, feedbackNameMaxCharacters); err != nil {
			return FeedbackSignal{}, err
		}
		if seen[service.Name] {
			return FeedbackSignal{}, feedbackFieldError(FeedbackServiceField(i, "name"), "must differ from the name of every other service of the signal")
		}
		seen[service.Name] = true
		if len(service.Versions) < 1 || len(service.Versions) > MaxFeedbackVersions {
			return FeedbackSignal{}, feedbackFieldError(FeedbackServiceField(i, "versions"), "must hold 1 to %d entries", MaxFeedbackVersions)
		}
		for j, version := range service.Versions {
			if version.Commit == "" && version.Version == "" {
				return FeedbackSignal{}, feedbackFieldError(fmt.Sprintf("services[%d].versions[%d]", i, j), "must state a commit, a version, or both")
			}
			if version.Commit != "" && !feedbackCommitValid(version.Commit) {
				return FeedbackSignal{}, feedbackFieldError(FeedbackVersionField(i, j, "commit"), "must be %d to %d hexadecimal characters", feedbackCommitMinCharacters, feedbackCommitMaxCharacters)
			}
			if version.Version != "" {
				if err := checkFeedbackText(redactor, FeedbackVersionField(i, j, "version"), version.Version, feedbackVersionMaxCharacters); err != nil {
					return FeedbackSignal{}, err
				}
			}
		}
	}
	return signal, nil
}

// CheckFeedbackRepositories refuses a stated repository outside the workspace
// configuration (component-production-feedback invariant 3).
func CheckFeedbackRepositories(signal FeedbackSignal, repositories map[string]bool) error {
	for i, service := range signal.Services {
		if service.Repository != "" && !repositories[service.Repository] {
			return feedbackFieldError(FeedbackServiceField(i, "repository"), "must name a repository of the workspace")
		}
	}
	return nil
}

// FeedbackSignalOrigin is the origin of everything a signal under the key
// records.
func FeedbackSignalOrigin(label string) FeedbackOrigin {
	return FeedbackOrigin{Source: FeedbackSourceSignal, Label: label, Actor: SystemActor().ID}
}

// FeedbackSignalServices returns the names of the services a signal names.
func FeedbackSignalServices(signal FeedbackSignal) []string {
	names := make([]string, len(signal.Services))
	for i, service := range signal.Services {
		names[i] = service.Name
	}
	return names
}

// FeedbackChanges is what one signal changes.
type FeedbackChanges struct {
	// Deploys are the new deploy records, each still running, with the
	// running version of the same index in Started.
	Deploys []FeedbackDeploy
	Started []FeedbackRunningVersion
	// Stopped are the running versions the signal no longer names.
	Stopped []FeedbackRunningVersion
	// Reported are the services whose newest report is this signal.
	Reported []string
}

// PlanFeedbackChanges compares a normalized signal with the running versions
// of the services it names. Every backend records exactly this result. A
// version named twice is one version, and a version that comes back after it
// stopped is a new deploy. A service that a later arrival already reported
// keeps that report, so a signal that was delayed on the way to the store
// never moves a service back (component-production-feedback invariant 5;
// req-production-feedback AC-1.3).
func PlanFeedbackChanges(signal FeedbackSignal, origin FeedbackOrigin, running []FeedbackRunningVersion) (FeedbackChanges, error) {
	current := map[string][]FeedbackRunningVersion{}
	for _, version := range running {
		current[version.Service] = append(current[version.Service], version)
	}
	var changes FeedbackChanges
	for _, service := range signal.Services {
		before := current[service.Name]
		if slices.ContainsFunc(before, func(v FeedbackRunningVersion) bool { return v.ReportedAt.After(signal.ArrivedAt) }) {
			continue
		}
		named := map[FeedbackVersion]bool{}
		for _, entry := range service.Versions {
			version := FeedbackVersion{Repository: service.Repository, Commit: entry.Commit, Version: entry.Version}
			if named[version] {
				continue
			}
			named[version] = true
			if slices.ContainsFunc(before, func(v FeedbackRunningVersion) bool { return v.FeedbackVersion == version }) {
				continue
			}
			id, err := newFeedbackDeployID()
			if err != nil {
				return FeedbackChanges{}, err
			}
			changes.Deploys = append(changes.Deploys, FeedbackDeploy{
				ID: id, EnvironmentID: signal.EnvironmentID, Service: service.Name, FeedbackVersion: version, DeployedAt: signal.ArrivedAt, Origin: origin,
			})
			changes.Started = append(changes.Started, FeedbackRunningVersion{
				EnvironmentID: signal.EnvironmentID, Service: service.Name, FeedbackVersion: version, DeployID: id, DeployedAt: signal.ArrivedAt, ReportedAt: signal.ArrivedAt,
			})
		}
		for _, version := range before {
			if !named[version.FeedbackVersion] {
				changes.Stopped = append(changes.Stopped, version)
			}
		}
		changes.Reported = append(changes.Reported, service.Name)
	}
	return changes, nil
}

func newFeedbackDeployID() (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate deploy ID: %w", err)
	}
	return "deploy_" + base64.RawURLEncoding.EncodeToString(random), nil
}

func compareFeedbackVersions(serviceA string, a FeedbackVersion, serviceB string, b FeedbackVersion) int {
	return slices.Compare([]string{serviceA, a.Repository, a.Version, a.Commit}, []string{serviceB, b.Repository, b.Version, b.Commit})
}

// SortFeedbackRunningVersions orders running versions by service and then by
// version.
func SortFeedbackRunningVersions(versions []FeedbackRunningVersion) {
	slices.SortFunc(versions, func(a, b FeedbackRunningVersion) int {
		return compareFeedbackVersions(a.Service, a.FeedbackVersion, b.Service, b.FeedbackVersion)
	})
}

// SortFeedbackDeploys orders deploys by time, then by service and version,
// and then by ID.
func SortFeedbackDeploys(deploys []FeedbackDeploy) {
	slices.SortFunc(deploys, func(a, b FeedbackDeploy) int {
		if order := a.DeployedAt.Compare(b.DeployedAt); order != 0 {
			return order
		}
		if order := compareFeedbackVersions(a.Service, a.FeedbackVersion, b.Service, b.FeedbackVersion); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}
