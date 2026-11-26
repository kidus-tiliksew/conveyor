package store

import (
	"cmp"
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
// made, and FeedbackSourceGitHub the kind of a change that a GitHub
// deployment report made (component-production-feedback invariant 6).
const (
	FeedbackSourceSignal = "signal"
	FeedbackSourceGitHub = "github"
)

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
// req-environments AC-2.4). Report is the number of the report in the order
// that the store accepted the reports of the environment, from 1. It names the
// report, so two reports with one stored time are never taken for each other.
type FeedbackOrigin struct {
	Source string
	Label  string
	Actor  string
	Report int64
}

// FeedbackBoundary is the place of one report among the reports of its
// environment: its arrival, and for one arrival the order of acceptance.
type FeedbackBoundary struct {
	At     time.Time
	Report int64
}

// Compare orders boundaries. Two are equal only for one report.
func (b FeedbackBoundary) Compare(other FeedbackBoundary) int {
	if order := b.At.Compare(other.At); order != 0 {
		return order
	}
	return cmp.Compare(b.Report, other.Report)
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

// Start is the report that started the deploy.
func (d FeedbackDeploy) Start() FeedbackBoundary {
	return FeedbackBoundary{At: d.DeployedAt, Report: d.Origin.Report}
}

// Stop is the report that stopped the deploy. running is true while none did.
func (d FeedbackDeploy) Stop() (stop FeedbackBoundary, running bool) {
	if d.StoppedAt == nil || d.StopOrigin == nil {
		return FeedbackBoundary{}, true
	}
	return FeedbackBoundary{At: *d.StoppedAt, Report: d.StopOrigin.Report}, false
}

// FeedbackSignalState is the newest accepted signal of one environment. Its
// origin holds no report number.
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
	// first, then in the order of their reports, and then by service and
	// version.
	ListFeedbackDeploys(ctx context.Context, environmentID string) ([]FeedbackDeploy, error)
	// RecordFeedbackGitHubDeploy records one successful GitHub deployment
	// report that matched the environment, in one transaction, and reports
	// whether it changed anything. The report replaces the running versions
	// of the service named after the repository, at the report's time, as
	// PlanFeedbackChanges plans it for a signal that names only that
	// service. It takes the next report number of the environment, which
	// signals share. An environment that is missing or archived by the time
	// of the write, a repository outside the workspace configuration, and a
	// report that changes nothing record nothing and answer false. It leaves
	// the newest signal and every key unchanged (component-production-feedback
	// invariants 6 and 10; req-production-feedback AC-1.5; req-environments
	// AC-1.9).
	RecordFeedbackGitHubDeploy(ctx context.Context, deploy FeedbackGitHubDeploy) (recorded bool, err error)
	// FeedbackStatusStore keeps resolved versions and comparisons and reads
	// merged tasks (feature-production-feedback-live FB-3).
	FeedbackStatusStore
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
// records. report is the number the store gave the signal.
func FeedbackSignalOrigin(label string, report int64) FeedbackOrigin {
	return FeedbackOrigin{Source: FeedbackSourceSignal, Label: label, Actor: SystemActor().ID, Report: report}
}

// FeedbackAcceptedSignal is one accepted signal: when it arrived, its number,
// who sent it, and the digests of the services it named. A signal that changes nothing
// leaves no deploy, so this is what dates the end of an earlier arrival's
// versions. Every accepted signal is kept, because the time that a request
// takes to reach the store has no limit.
type FeedbackAcceptedSignal struct {
	ArrivedAt time.Time
	Report    int64
	Label     string
	Services  []string
}

// NewFeedbackAcceptedSignal is the kept form of a normalized signal.
func NewFeedbackAcceptedSignal(signal FeedbackSignal, origin FeedbackOrigin) FeedbackAcceptedSignal {
	accepted := FeedbackAcceptedSignal{ArrivedAt: signal.ArrivedAt, Report: origin.Report, Label: origin.Label, Services: make([]string, len(signal.Services))}
	for i, service := range signal.Services {
		accepted.Services[i] = FeedbackServiceDigest(service.Name)
	}
	return accepted
}

// FeedbackAcceptedServices renders the digests for a durable backend's row, and
// ApplyFeedbackAcceptedServices reads them back.
func FeedbackAcceptedServices(accepted FeedbackAcceptedSignal) string {
	return string(core.JSONPayload(accepted.Services))
}

func ApplyFeedbackAcceptedServices(accepted *FeedbackAcceptedSignal, services []byte) error {
	if err := json.Unmarshal(services, &accepted.Services); err != nil {
		return fmt.Errorf("decode accepted signal services: %w", err)
	}
	return nil
}

// FeedbackHistory is the stored state PlanFeedbackChanges reads.
type FeedbackHistory struct {
	// Running are the running versions of the services the signal names.
	Running []FeedbackRunningVersion
	// Later and Deploys are needed only when FeedbackLateServices names a
	// service: the first accepted signal after this one that named each such
	// service, and the deploys that had not stopped at its arrival. Others can
	// be among them.
	Later   []FeedbackAcceptedSignal
	Deploys []FeedbackDeploy
}

// FeedbackLateServices returns the digests of the services that a later
// arrival than the signal already reported.
func FeedbackLateServices(signal FeedbackSignal, running []FeedbackRunningVersion) map[string]bool {
	late := map[string]bool{}
	for _, version := range running {
		if version.ReportedAt.After(signal.ArrivedAt) {
			late[FeedbackServiceDigest(version.Service)] = true
		}
	}
	return late
}

// AddLater keeps an accepted signal that is the first to name a late service.
// A backend passes the signals after the late one, in the order of their
// boundaries, and stops when done reports that every late service has its next report.
func (h *FeedbackHistory) AddLater(late map[string]bool, accepted FeedbackAcceptedSignal) (done bool) {
	first := false
	for _, digest := range accepted.Services {
		if late[digest] {
			delete(late, digest)
			first = true
		}
	}
	if first {
		h.Later = append(h.Later, accepted)
	}
	return len(late) == 0
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
// names the service ends what it reported. Signals of one arrival keep the
// order in which the store accepted them. It changes no version that runs now
// and not the newest signal (component-production-feedback invariants 4 to 6;
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
		if len(FeedbackLateServices(signal, current[service.Name])) > 0 {
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
			EnvironmentID: signal.EnvironmentID, Service: service, FeedbackVersion: version, DeployID: deploy.ID, DeployedAt: deploy.DeployedAt, ReportedAt: signal.ArrivedAt,
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
	// The signal is the last accepted report, so it comes after every report
	// of its own arrival.
	at := FeedbackBoundary{At: signal.ArrivedAt, Report: origin.Report}
	next, nextOrigin := feedbackNextReport(at, service, history)
	ran := map[FeedbackVersion]bool{}
	resumed := map[FeedbackVersion]FeedbackDeploy{}
	for _, deploy := range history.Deploys {
		start := deploy.Start()
		stop, runs := deploy.Stop()
		switch {
		case deploy.Service != service:
		case start.Compare(next) == 0:
			// Only the next report itself can keep what this signal names. A
			// report after it that has the same arrival cannot.
			resumed[deploy.FeedbackVersion] = deploy
		case start.Compare(at) > 0 || (!runs && stop.Compare(at) <= 0):
		case slices.Contains(named, deploy.FeedbackVersion):
			ran[deploy.FeedbackVersion] = true
		default:
			// The service ran this version and the signal drops it. When it
			// ran on after the next arrival, it came back then.
			if runs || stop.Compare(next) > 0 {
				again := deploy
				again.DeployedAt, again.Origin = next.At, nextOrigin
				again, err := c.addDeploy(again)
				if err != nil {
					return err
				}
				c.move(running, deploy.ID, again)
			}
			deploy.StoppedAt, deploy.StopOrigin = &at.At, &origin
			c.Updated = append(c.Updated, deploy)
		}
	}
	for _, version := range named {
		if ran[version] {
			continue
		}
		if deploy, ok := resumed[version]; ok {
			// The next arrival kept this version, so it was deployed here.
			deploy.DeployedAt, deploy.Origin = at.At, origin
			c.Updated = append(c.Updated, deploy)
			c.move(running, deploy.ID, deploy)
			continue
		}
		if _, err := c.addDeploy(FeedbackDeploy{EnvironmentID: signal.EnvironmentID, Service: service, FeedbackVersion: version, DeployedAt: at.At, Origin: origin, StoppedAt: &next.At, StopOrigin: &nextOrigin}); err != nil {
			return err
		}
	}
	return nil
}

// feedbackNextReport returns the first report after at that named the service,
// and its origin: an accepted signal, or a deploy or stop of the service.
func feedbackNextReport(at FeedbackBoundary, service string, history FeedbackHistory) (FeedbackBoundary, FeedbackOrigin) {
	var next FeedbackBoundary
	var origin FeedbackOrigin
	consider := func(report time.Time, by FeedbackOrigin) {
		boundary := FeedbackBoundary{At: report, Report: by.Report}
		if boundary.Compare(at) > 0 && (next.At.IsZero() || boundary.Compare(next) < 0) {
			next, origin = boundary, by
		}
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
	for _, accepted := range history.Later {
		if slices.Contains(accepted.Services, digest) {
			consider(accepted.ArrivedAt, FeedbackSignalOrigin(accepted.Label, accepted.Report))
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

// SortFeedbackDeploys orders deploys by the report that started them, then by
// service and version, and then by ID.
func SortFeedbackDeploys(deploys []FeedbackDeploy) {
	slices.SortFunc(deploys, func(a, b FeedbackDeploy) int {
		if order := a.Start().Compare(b.Start()); order != 0 {
			return order
		}
		if order := compareFeedbackVersions(a.Service, a.FeedbackVersion, b.Service, b.FeedbackVersion); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// FeedbackGitHubDeploy is one successful GitHub deployment report that matched
// an unarchived environment and, where the environment tracks a branch for
// the repository, that branch. Repository is the repository's name in the
// workspace configuration and names the service. Commit is the deployment's
// full commit ID, and ReportedAt is the time the status reports
// (component-production-feedback invariant 10; req-production-feedback
// AC-1.5).
type FeedbackGitHubDeploy struct {
	EnvironmentID string
	Repository    string
	Commit        string
	ReportedAt    time.Time
}

// FeedbackGitHubOrigin is the origin of everything a GitHub deployment report
// records: the source GitHub and the system as the actor
// (component-production-feedback invariant 6).
func FeedbackGitHubOrigin(report int64) FeedbackOrigin {
	return FeedbackOrigin{Source: FeedbackSourceGitHub, Actor: SystemActor().ID, Report: report}
}

// FeedbackGitHubDeploySignal checks a GitHub deployment report and returns it
// as the signal that PlanFeedbackChanges reads: one service named after the
// repository that runs exactly the deployment's commit, at the report's time.
// The signal holds no key and no interval, so nothing that reads it changes
// the newest signal or a key. A refusal names the field and never repeats the
// value.
func FeedbackGitHubDeploySignal(deploy FeedbackGitHubDeploy) (FeedbackSignal, error) {
	if deploy.EnvironmentID == "" {
		return FeedbackSignal{}, feedbackFieldError("environment_id", "is required")
	}
	if err := checkFeedbackText(redact.New(nil), "repository", deploy.Repository, feedbackNameMaxCharacters); err != nil {
		return FeedbackSignal{}, err
	}
	commit := strings.ToLower(deploy.Commit)
	if len(commit) != feedbackCommitMaxCharacters || !feedbackCommitValid(commit) {
		return FeedbackSignal{}, feedbackFieldError("commit", "must be a full commit ID of %d hexadecimal characters", feedbackCommitMaxCharacters)
	}
	if deploy.ReportedAt.IsZero() {
		return FeedbackSignal{}, feedbackFieldError("reported_at", "is required")
	}
	return FeedbackSignal{
		EnvironmentID: deploy.EnvironmentID,
		ArrivedAt:     EnvironmentTime(deploy.ReportedAt),
		Services: []FeedbackSignalService{{
			Name: deploy.Repository, Repository: deploy.Repository,
			Versions: []FeedbackSignalVersion{{Commit: commit}},
		}},
	}, nil
}

// Changed reports whether the plan records anything. A report that names
// exactly what the service runs changes nothing.
func (c FeedbackChanges) Changed() bool {
	return len(c.Deploys) > 0 || len(c.Started) > 0 || len(c.Stopped) > 0 || len(c.Updated) > 0 || len(c.Moved) > 0
}

// NextFeedbackReport is the number of the next accepted report of an
// environment: one more than the highest number that its newest signal row,
// its accepted signals, or its deploys and stops hold. Signals and GitHub
// deployment reports share the numbers, so two reports never share one.
func NextFeedbackReport(numbers ...int64) int64 {
	var highest int64
	for _, number := range numbers {
		highest = max(highest, number)
	}
	return highest + 1
}
