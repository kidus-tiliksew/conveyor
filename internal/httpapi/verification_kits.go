package httpapi

import (
	"container/list"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// Workspace kit registry (feature-verification-kit-execution v6 VK-11,
// req-verification-kits v2 REQ-11). The read reports what each configured
// repository's base-branch manifest declares and how kit pins compare with the
// currently confirmed corpus. It is display-only: verify-stage selection keeps
// using the frozen work-order snapshot (AC-11.5) and this handler writes nothing.

// kitRegistryRepositoryLimit bounds discovery and pin resolution for one
// repository. Tests shorten it to exercise the timeout path.
var kitRegistryRepositoryLimit = 20 * time.Second

const (
	kitRegistryMemoSize    = 64
	kitRegistryConcurrency = 4
)

type kitRegistryResponse struct {
	Repositories []kitRegistryRepository `json:"repositories"`
}

type kitRegistryRepository struct {
	Repository    string                    `json:"repository"`
	Base          string                    `json:"base"`
	CommitSHA     string                    `json:"commit_sha"`
	State         string                    `json:"state"`
	Reason        string                    `json:"reason,omitempty"`
	SchemaVersion int                       `json:"schema_version,omitempty"`
	Diagnostics   []verification.Diagnostic `json:"diagnostics"`
	Kits          []kitRegistryKit          `json:"kits"`
}

type kitRegistryKit struct {
	ID          string                    `json:"id"`
	Name        string                    `json:"name"`
	Version     string                    `json:"version"`
	Description string                    `json:"description"`
	Path        string                    `json:"path"`
	Digest      string                    `json:"digest"`
	Stages      []string                  `json:"stages"`
	Status      string                    `json:"status"`
	Pins        []kitRegistryPin          `json:"pins"`
	Diagnostics []verification.Diagnostic `json:"diagnostics"`
	Exercises   []kitRegistryExercise     `json:"exercises"`
}

type kitRegistryPin struct {
	Kind           string `json:"kind"`
	DocumentID     string `json:"document_id"`
	Version        int    `json:"version"`
	Status         string `json:"status"`
	CurrentVersion int    `json:"current_version,omitempty"`
}

// kitRegistryExercise is the normalized VK-3 contract with descriptions always
// present (empty under schema 1, AC-10.2) and assertions always in object form.
type kitRegistryExercise struct {
	ID                 string                        `json:"id"`
	Description        string                        `json:"description"`
	Stages             []string                      `json:"stages"`
	Kind               string                        `json:"kind"`
	Argv               []string                      `json:"argv"`
	Cwd                string                        `json:"cwd"`
	TimeoutSeconds     int                           `json:"timeout_seconds"`
	Prerequisites      []verification.Prerequisite   `json:"prerequisites"`
	Permissions        []verification.Permission     `json:"permissions"`
	Inputs             []verification.Input          `json:"inputs"`
	RequiredAssertions []kitRegistryAssertion        `json:"required_assertions"`
	RetryPolicy        string                        `json:"retry_policy"`
	SafetyBasis        string                        `json:"safety_basis"`
	Operations         []verification.Operation      `json:"operations"`
	EvidenceOutputs    []verification.EvidenceOutput `json:"evidence_outputs"`
	Supports           []verification.Support        `json:"supports"`
}

type kitRegistryAssertion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

func (s *Server) getWorkspaceVerificationKits(w http.ResponseWriter, r *http.Request) {
	workspace, _ := store.WorkspaceFromContext(r.Context())
	if s.ConfigProvider == nil {
		appHTTPError(w, http.StatusInternalServerError, "workspace_config_failed")
		return
	}
	cfg, err := s.ConfigProvider(r.Context())
	if err != nil {
		appHTTPError(w, http.StatusInternalServerError, "workspace_config_failed")
		return
	}
	connected := false
	if s.WorkspaceGitHubApps != nil {
		status, err := s.WorkspaceGitHubApps.GetWorkspaceGitHubAppStatus(r.Context(), workspace)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			appHTTPError(w, http.StatusInternalServerError, "github_app_status_failed")
			return
		}
		connected = err == nil && status.Connected && status.InstallationID > 0
	}
	result := kitRegistryResponse{Repositories: make([]kitRegistryRepository, len(cfg.Repos))}
	pins := newKitPinResolver(s.Store)
	var wg sync.WaitGroup
	slots := make(chan struct{}, kitRegistryConcurrency)
	for i, repo := range cfg.Repos {
		if !connected {
			result.Repositories[i] = kitRegistryUnavailable(repo, "", "no_app")
			continue
		}
		wg.Add(1)
		go func(i int, repo config.Repo) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			// Discovery and pin resolution share one repository budget.
			ctx, cancel := context.WithTimeout(r.Context(), kitRegistryRepositoryLimit)
			defer cancel()
			out := s.readRepositoryKits(ctx, workspace, repo)
			// Pin status reads the corpus at request time, so a newly confirmed
			// version shows without any cache invalidation. A failed corpus read
			// or an expired budget makes the repository unavailable rather than
			// reporting statuses derived from missing data (AC-11.4).
			var err error
			for j := range out.Kits {
				if err = pins.apply(ctx, &out.Kits[j]); err != nil {
					break
				}
			}
			if err != nil || ctx.Err() != nil {
				out = kitRegistryUnavailable(repo, out.CommitSHA, "transport")
			}
			result.Repositories[i] = out
		}(i, repo)
	}
	wg.Wait()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func kitRegistryUnavailable(repo config.Repo, sha, reason string) kitRegistryRepository {
	return kitRegistryRepository{Repository: repo.Name, Base: repo.Base, CommitSHA: sha, State: "unavailable", Reason: reason, Diagnostics: []verification.Diagnostic{}, Kits: []kitRegistryKit{}}
}

func (s *Server) readRepositoryKits(ctx context.Context, workspace string, repo config.Repo) kitRegistryRepository {
	client := s.appClient()
	sha, state := client.ResolveBranchHead(ctx, s.WorkspaceGitHubApps, workspace, repo.GitHub, repo.Base)
	if state != "present" {
		return kitRegistryUnavailable(repo, "", kitRegistryReason(ctx, state))
	}
	key := kitRegistryKey{workspace: workspace, repository: strings.ToLower(repo.GitHub), sha: sha}
	d, ok := s.verificationKitMemo.get(key)
	if !ok {
		d = client.DiscoverVerification(ctx, s.WorkspaceGitHubApps, workspace, repo.GitHub, sha)
		switch d.State {
		case "present", "no_manifest", "malformed":
			// Content at a commit is immutable; failures are never cached.
			s.verificationKitMemo.put(key, d)
		}
	}
	return projectRepositoryKits(repo, sha, d, kitRegistryReason(ctx, d.State))
}

// kitRegistryReason maps discovery states onto VK-11 unavailable reasons.
func kitRegistryReason(ctx context.Context, state string) string {
	if ctx.Err() != nil {
		return "transport"
	}
	switch state {
	case "permission", "unknown_revision", "truncated":
		return state
	}
	return "transport"
}

// projectRepositoryKits applies the VK-11 state mapping and kit projection.
// Evaluate supplies content digests and invalid-entry diagnostics, including a
// missing kit root; eligibility is ignored because no work-order pins apply.
func projectRepositoryKits(repo config.Repo, sha string, d github.VerificationDiscovery, reason string) kitRegistryRepository {
	out := kitRegistryRepository{Repository: repo.Name, Base: repo.Base, CommitSHA: sha, Diagnostics: []verification.Diagnostic{}, Kits: []kitRegistryKit{}}
	switch d.State {
	case "no_manifest":
		out.State = "no_manifest"
		return out
	case "present", "malformed":
	default:
		out.State, out.Reason = "unavailable", reason
		return out
	}
	manifest := d.Manifest
	if manifest == nil {
		out.State = "invalid"
		out.Diagnostics = append(out.Diagnostics, verification.Diagnostic{Path: "manifest", Message: "malformed"})
		return out
	}
	out.SchemaVersion = manifest.SchemaVersion
	receipt := verification.Evaluate(manifest, verification.SelectionContext{Stage: "verify", ManifestRevision: sha, SourceRevision: sha}, d.Trees)
	// Discovery reports "malformed" whenever Parse returns an error, which
	// includes a single invalid entry. VK-11 maps that state to an invalid
	// repository. Diagnostics stay where they belong (AC-11.4): the repository
	// carries only manifest-level ones, and each kit is invalid only through its
	// own receipt, which Evaluate also marks for manifest-level diagnostics.
	out.Diagnostics = append(out.Diagnostics, receipt.Diagnostics...)
	out.State = "ok"
	if d.State == "malformed" || len(out.Diagnostics) > 0 {
		out.State = "invalid"
	}
	for i, k := range manifest.Kits {
		kit := projectKit(k)
		if i < len(receipt.Kits) && receipt.Kits[i].KitID == k.ID {
			kit.Digest = receipt.Kits[i].Digest
			if receipt.Kits[i].Eligibility == "invalid" {
				kit.Status = "invalid"
				for _, reason := range receipt.Kits[i].Reasons {
					if reason.Code == "invalid_entry" {
						kit.Diagnostics = append(kit.Diagnostics, verification.Diagnostic{Path: reason.Path, Message: reason.Message})
					}
				}
			}
		}
		if kit.Status == "invalid" {
			kit.Digest = ""
		}
		out.Kits = append(out.Kits, kit)
	}
	return out
}

func projectKit(k verification.Kit) kitRegistryKit {
	kit := kitRegistryKit{ID: k.ID, Name: k.Name, Version: k.Version, Description: k.Description, Path: k.Path, Stages: []string{}, Pins: []kitRegistryPin{}, Diagnostics: []verification.Diagnostic{}, Exercises: []kitRegistryExercise{}}
	for _, p := range k.GoverningPins.Requirements {
		kit.Pins = append(kit.Pins, kitRegistryPin{Kind: verification.PinRequirement, DocumentID: p.DocumentID, Version: p.Version})
	}
	for _, p := range k.GoverningPins.SystemDesigns {
		kit.Pins = append(kit.Pins, kitRegistryPin{Kind: verification.PinSystemDesign, DocumentID: p.DocumentID, Version: p.Version})
	}
	seen := map[string]bool{}
	for _, e := range k.Exercises {
		for _, stage := range e.Stages {
			if !seen[stage] {
				seen[stage] = true
				kit.Stages = append(kit.Stages, stage)
			}
		}
		ex := kitRegistryExercise{ID: e.ID, Description: e.Description, Stages: nonNil(e.Stages), Kind: e.Kind, Argv: nonNil(e.Argv), Cwd: e.Cwd, TimeoutSeconds: e.TimeoutSeconds,
			Prerequisites: nonNil(e.Prerequisites), Permissions: nonNil(e.Permissions), Inputs: nonNil(e.Inputs), RequiredAssertions: []kitRegistryAssertion{},
			RetryPolicy: e.RetryPolicy, SafetyBasis: e.SafetyBasis, Operations: nonNil(e.Operations), EvidenceOutputs: nonNil(e.EvidenceOutputs), Supports: nonNil(e.Supports)}
		for _, a := range e.RequiredAssertions {
			ex.RequiredAssertions = append(ex.RequiredAssertions, kitRegistryAssertion{ID: a.ID, Description: a.Description})
		}
		kit.Exercises = append(kit.Exercises, ex)
	}
	return kit
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// kitPinResolver compares pins with the workspace corpus once per document per
// request (AC-11.3).
type kitPinResolver struct {
	store        store.Store
	mu           sync.Mutex
	requirements map[string]kitDocumentVersions
	designs      map[string]kitDocumentVersions
}

// kitDocumentVersions records a document's served state: its current confirmed
// version and each version's status other than current.
type kitDocumentVersions struct {
	found    bool
	archived bool
	current  int
	versions map[int]string // "confirmed", "pending" or "retired"
}

func newKitPinResolver(st store.Store) *kitPinResolver {
	return &kitPinResolver{store: st, requirements: map[string]kitDocumentVersions{}, designs: map[string]kitDocumentVersions{}}
}

// document is safe for the concurrent repository workers. Concurrent misses may
// read the same document twice; the cached value is identical either way.
// ErrNotFound means the document is absent; any other read failure is returned
// and never cached, so it cannot masquerade as an unresolved pin.
func (p *kitPinResolver) document(ctx context.Context, kind, id string) (kitDocumentVersions, error) {
	cache := p.requirements
	if kind == verification.PinSystemDesign {
		cache = p.designs
	}
	p.mu.Lock()
	d, ok := cache[id]
	p.mu.Unlock()
	if ok {
		return d, nil
	}
	d = kitDocumentVersions{versions: map[int]string{}}
	if p.store != nil {
		var err error
		switch kind {
		case verification.PinRequirement:
			var doc core.Requirement
			if doc, err = p.store.GetRequirement(ctx, id); err == nil {
				d.found, d.archived, d.current = true, doc.Archived, doc.CurrentVersion
				var versions []core.RequirementVersion
				if versions, err = p.store.ListRequirementVersions(ctx, id); err == nil {
					for _, v := range versions {
						d.versions[v.Version] = versionStatus(v.Confirmed, v.Retired)
					}
				}
			}
		case verification.PinSystemDesign:
			var doc core.SystemDesign
			if doc, err = p.store.GetSystemDesign(ctx, id); err == nil {
				d.found, d.archived, d.current = true, doc.Archived, doc.CurrentVersion
				var versions []core.SystemDesignVersion
				if versions, err = p.store.ListSystemDesignVersions(ctx, id); err == nil {
					for _, v := range versions {
						d.versions[v.Version] = versionStatus(v.Confirmed, v.Dismissed)
					}
				}
			}
		}
		if err != nil && !(errors.Is(err, store.ErrNotFound) && !d.found) {
			return kitDocumentVersions{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return kitDocumentVersions{}, err
	}
	p.mu.Lock()
	cache[id] = d
	p.mu.Unlock()
	return d, nil
}

func versionStatus(confirmed, withdrawn bool) string {
	switch {
	case confirmed:
		return "confirmed"
	case withdrawn:
		return "retired"
	}
	return "pending"
}

// kitStatusOrder is the VK-11 precedence among pin statuses.
var kitStatusOrder = []string{"unresolved", "behind", "pending", "current"}

func (p *kitPinResolver) apply(ctx context.Context, kit *kitRegistryKit) error {
	for i := range kit.Pins {
		pin := &kit.Pins[i]
		d, err := p.document(ctx, pin.Kind, pin.DocumentID)
		if err != nil {
			return err
		}
		pin.Status = pinStatus(d, pin.Version)
		if pin.Status == "behind" {
			pin.CurrentVersion = d.current
		}
	}
	if kit.Status == "invalid" {
		return nil
	}
	if len(kit.Pins) == 0 {
		kit.Status = "unpinned"
		return nil
	}
	for _, status := range kitStatusOrder {
		for _, pin := range kit.Pins {
			if pin.Status == status {
				kit.Status = status
				return nil
			}
		}
	}
	return nil
}

func pinStatus(d kitDocumentVersions, version int) string {
	if !d.found || d.archived || version <= 0 {
		return "unresolved"
	}
	if d.current > 0 && version == d.current {
		return "current"
	}
	switch d.versions[version] {
	case "confirmed":
		if version < d.current {
			return "behind"
		}
	case "pending":
		return "pending"
	}
	return "unresolved"
}

// kitRegistryMemo is a bounded LRU of discovery results keyed by exact commit.
type kitRegistryKey struct{ workspace, repository, sha string }

type kitRegistryMemo struct {
	mu      sync.Mutex
	order   *list.List
	entries map[kitRegistryKey]*list.Element
}

type kitRegistryMemoEntry struct {
	key   kitRegistryKey
	value github.VerificationDiscovery
}

func (m *kitRegistryMemo) get(key kitRegistryKey) (github.VerificationDiscovery, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		return github.VerificationDiscovery{}, false
	}
	e, ok := m.entries[key]
	if !ok {
		return github.VerificationDiscovery{}, false
	}
	m.order.MoveToFront(e)
	return e.Value.(kitRegistryMemoEntry).value, true
}

func (m *kitRegistryMemo) put(key kitRegistryKey, value github.VerificationDiscovery) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries, m.order = map[kitRegistryKey]*list.Element{}, list.New()
	}
	if e, ok := m.entries[key]; ok {
		e.Value = kitRegistryMemoEntry{key, value}
		m.order.MoveToFront(e)
		return
	}
	m.entries[key] = m.order.PushFront(kitRegistryMemoEntry{key, value})
	for m.order.Len() > kitRegistryMemoSize {
		oldest := m.order.Back()
		m.order.Remove(oldest)
		delete(m.entries, oldest.Value.(kitRegistryMemoEntry).key)
	}
}

func (m *kitRegistryMemo) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.order == nil {
		return 0
	}
	return m.order.Len()
}
