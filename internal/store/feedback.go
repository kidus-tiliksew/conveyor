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

	"github.com/kidus-tiliksew/conveyor/internal/core"
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

// FeedbackRunningVersion is one version a service runs. ReportedAt and
// ReportedLabel are the arrival and the key's label of the newest signal that
// named the service. A version is stored unresolved.
type FeedbackRunningVersion struct {
	EnvironmentID string
	Service       string
	FeedbackVersion
	DeployID      string
	DeployedAt    time.Time
	ReportedAt    time.Time
	ReportedLabel string
	Resolved      bool
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
	// CheckFeedbackRepositories refuses a signal that states a repository
	// outside the current workspace configuration, with the FeedbackFieldError
	// that RecordFeedbackSignal would answer. It writes nothing.
	// RecordFeedbackSignal checks again, because the configuration can change
	// in between.
	CheckFeedbackRepositories(ctx context.Context, signal FeedbackSignal) error
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

// FeedbackRecentWindow is how far behind the newest signal of an environment
// a signal can arrive and still find every later signal in
// FeedbackHistory.Recent.
const FeedbackRecentWindow = 10 * time.Minute

// FeedbackRecentSignal is one accepted signal of the recent window: when it
// arrived, who sent it, and the digests of the services it named. A signal
// that changes nothing leaves no deploy, so this is what dates the end of an
// earlier arrival's versions.
type FeedbackRecentSignal struct {
	ArrivedAt time.Time
	Label     string
	Services  []string
}

// NewFeedbackRecentSignal is the recent form of a normalized signal.
func NewFeedbackRecentSignal(signal FeedbackSignal, origin FeedbackOrigin) FeedbackRecentSignal {
	recent := FeedbackRecentSignal{ArrivedAt: signal.ArrivedAt, Label: origin.Label, Services: make([]string, len(signal.Services))}
	for i, service := range signal.Services {
		recent.Services[i] = FeedbackServiceDigest(service.Name)
	}
	return recent
}

// FeedbackRecentServices renders the digests for a durable backend's row, and
// ApplyFeedbackRecentServices reads them back.
func FeedbackRecentServices(recent FeedbackRecentSignal) string {
	return string(core.JSONPayload(recent.Services))
}

func ApplyFeedbackRecentServices(recent *FeedbackRecentSignal, services []byte) error {
	if err := json.Unmarshal(services, &recent.Services); err != nil {
		return fmt.Errorf("decode recent signal services: %w", err)
	}
	return nil
}

// FeedbackHistory is the stored state PlanFeedbackChanges reads.
type FeedbackHistory struct {
	// Running are the running versions of the services the signal names.
	Running []FeedbackRunningVersion
	// Recent and Deploys are needed only when FeedbackArrivedLate reports
	// true: the recent signals that arrived after this one, and the deploys
	// that had not stopped at its arrival. Other services can be among them.
	Recent  []FeedbackRecentSignal
	Deploys []FeedbackDeploy
}

// FeedbackArrivedLate reports whether a later arrival already reported a
// service that the signal names.
func FeedbackArrivedLate(signal FeedbackSignal, running []FeedbackRunningVersion) bool {
	return slices.ContainsFunc(running, func(v FeedbackRunningVersion) bool { return v.ReportedAt.After(signal.ArrivedAt) })
}

// FeedbackChanges is what one signal changes.
type FeedbackChanges struct {
	// Deploys are the new deploy records. One of an earlier arrival can be
	// stopped already.
	Deploys []FeedbackDeploy
	// Started are the new running versions, each with its deploy in Deploys.
	Started []FeedbackRunningVersion
	// Stopped are the running versions the signal no longer names.
	Stopped []FeedbackRunningVersion
	// Reported are the services whose newest report is this signal.
	Reported []string
	// Updated are stored deploys with a new deploy time, stop time, or origin,
	// and Moved are stored running versions with a new deploy or deploy time.
	// Only an earlier arrival has either.
	Updated []FeedbackDeploy
	Moved   []FeedbackRunningVersion
}

// PlanFeedbackChanges compares a normalized signal with the stored state of
// the services it names. Every backend records exactly this result. A version
// named twice is one version, and a version that comes back after it stopped
// is a new deploy. The records are those of the arrival order, whatever order
// the signals reach the store in: a signal that a later arrival overtook
// records its deploys and stops at its own arrival, and the next arrival that
// names the service ends what it reported. It changes no version that runs now
// and not the newest signal (component-production-feedback invariants 4 and 5;
// req-production-feedback AC-1.3).
func PlanFeedbackChanges(signal FeedbackSignal, origin FeedbackOrigin, history FeedbackHistory) (FeedbackChanges, error) {
	current := map[string][]FeedbackRunningVersion{}
	for _, version := range history.Running {
		current[version.Service] = append(current[version.Service], version)
	}
	var changes FeedbackChanges
	for _, service := range signal.Services {
		named := []FeedbackVersion{}
		for _, entry := range service.Versions {
			version := FeedbackVersion{Repository: service.Repository, Commit: entry.Commit, Version: entry.Version}
			if !slices.Contains(named, version) {
				named = append(named, version)
			}
		}
		plan := changes.planNewest
		if FeedbackArrivedLate(signal, current[service.Name]) {
			plan = changes.planEarlier
		}
		if err := plan(signal, origin, service.Name, named, current[service.Name], history); err != nil {
			return FeedbackChanges{}, err
		}
	}
	return changes, nil
}

// planNewest records a service whose newest report is this signal.
func (c *FeedbackChanges) planNewest(signal FeedbackSignal, origin FeedbackOrigin, service string, named []FeedbackVersion, running []FeedbackRunningVersion, _ FeedbackHistory) error {
	for _, version := range named {
		if slices.ContainsFunc(running, func(v FeedbackRunningVersion) bool { return v.FeedbackVersion == version }) {
			continue
		}
		deploy, err := c.addDeploy(FeedbackDeploy{EnvironmentID: signal.EnvironmentID, Service: service, FeedbackVersion: version, DeployedAt: signal.ArrivedAt, Origin: origin})
		if err != nil {
			return err
		}
		c.Started = append(c.Started, FeedbackRunningVersion{
			EnvironmentID: signal.EnvironmentID, Service: service, FeedbackVersion: version, DeployID: deploy.ID, DeployedAt: deploy.DeployedAt, ReportedAt: signal.ArrivedAt, ReportedLabel: origin.Label,
		})
	}
	for _, version := range running {
		if !slices.Contains(named, version.FeedbackVersion) {
			c.Stopped = append(c.Stopped, version)
		}
	}
	c.Reported = append(c.Reported, service)
	return nil
}

// planEarlier records a service that a later arrival already reported. From
// the signal's arrival to the next arrival that names the service, the
// service ran what the signal names. Before and after that span the records
// stay as they are.
func (c *FeedbackChanges) planEarlier(signal FeedbackSignal, origin FeedbackOrigin, service string, named []FeedbackVersion, running []FeedbackRunningVersion, history FeedbackHistory) error {
	at := signal.ArrivedAt
	next, nextOrigin := feedbackNextReport(at, service, running, history)
	ran := map[FeedbackVersion]bool{}
	resumed := map[FeedbackVersion]FeedbackDeploy{}
	for _, deploy := range history.Deploys {
		switch {
		case deploy.Service != service:
		case deploy.DeployedAt.Equal(next):
			resumed[deploy.FeedbackVersion] = deploy
		case deploy.DeployedAt.After(at) || (deploy.StoppedAt != nil && !deploy.StoppedAt.After(at)):
		case slices.Contains(named, deploy.FeedbackVersion):
			ran[deploy.FeedbackVersion] = true
		default:
			// The service ran this version and the signal drops it. When it
			// ran on after the next arrival, it came back then.
			if deploy.StoppedAt == nil || deploy.StoppedAt.After(next) {
				again := deploy
				again.DeployedAt, again.Origin = next, nextOrigin
				again, err := c.addDeploy(again)
				if err != nil {
					return err
				}
				c.move(running, deploy.ID, again)
			}
			deploy.StoppedAt, deploy.StopOrigin = &at, &origin
			c.Updated = append(c.Updated, deploy)
		}
	}
	for _, version := range named {
		if ran[version] {
			continue
		}
		if deploy, ok := resumed[version]; ok {
			// The next arrival kept this version, so it was deployed here.
			deploy.DeployedAt, deploy.Origin = at, origin
			c.Updated = append(c.Updated, deploy)
			c.move(running, deploy.ID, deploy)
			continue
		}
		if _, err := c.addDeploy(FeedbackDeploy{EnvironmentID: signal.EnvironmentID, Service: service, FeedbackVersion: version, DeployedAt: at, Origin: origin, StoppedAt: &next, StopOrigin: &nextOrigin}); err != nil {
			return err
		}
	}
	return nil
}

// feedbackNextReport returns the first known arrival after at that named the
// service, and its origin: a recent signal, the service's newest report, or a
// deploy or stop. Past the recent window only an arrival that changed the
// service, or its newest one, is still known.
func feedbackNextReport(at time.Time, service string, running []FeedbackRunningVersion, history FeedbackHistory) (time.Time, FeedbackOrigin) {
	var next time.Time
	var origin FeedbackOrigin
	consider := func(report time.Time, by FeedbackOrigin) {
		if report.After(at) && (next.IsZero() || report.Before(next)) {
			next, origin = report, by
		}
	}
	for _, version := range running {
		consider(version.ReportedAt, FeedbackSignalOrigin(version.ReportedLabel))
	}
	for _, deploy := range history.Deploys {
		if deploy.Service != service {
			continue
		}
		consider(deploy.DeployedAt, deploy.Origin)
		if deploy.StoppedAt != nil {
			consider(*deploy.StoppedAt, *deploy.StopOrigin)
		}
	}
	digest := FeedbackServiceDigest(service)
	for _, recent := range history.Recent {
		if slices.Contains(recent.Services, digest) {
			consider(recent.ArrivedAt, FeedbackSignalOrigin(recent.Label))
		}
	}
	return next, origin
}

// addDeploy gives the deploy a new ID and adds it to the new records.
func (c *FeedbackChanges) addDeploy(deploy FeedbackDeploy) (FeedbackDeploy, error) {
	id, err := newFeedbackDeployID()
	if err != nil {
		return FeedbackDeploy{}, err
	}
	deploy.ID = id
	c.Deploys = append(c.Deploys, deploy)
	return deploy, nil
}

// move points the running version of a stored deploy, if it still runs, at
// the deploy that now holds it.
func (c *FeedbackChanges) move(running []FeedbackRunningVersion, deployID string, deploy FeedbackDeploy) {
	for _, version := range running {
		if version.DeployID == deployID {
			version.DeployID, version.DeployedAt = deploy.ID, deploy.DeployedAt
			c.Moved = append(c.Moved, version)
		}
	}
}

func newFeedbackDeployID() (string, error) { return newFeedbackID("deploy_") }

// NewFeedbackRecentSignalID tells two signals of one arrival apart in a
// durable backend's rows.
func NewFeedbackRecentSignalID() (string, error) { return newFeedbackID("signal_") }

func newFeedbackID(prefix string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate feedback ID: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
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
