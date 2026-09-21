package workorder

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type VerificationScopeRevision struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}
type VerificationPrepareRequest struct {
	RequestKey          string                      `json:"request_key"`
	AdditionalRevisions []VerificationScopeRevision `json:"additional_revisions,omitempty"`
}
type VerificationContextRequest struct {
	ContextID string `json:"context_id"`
}
type VerificationSource struct {
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
	SectionID  string `json:"section_id"`
}
type VerificationLink struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type VerificationObligationRequest struct {
	ObservationProcedure string                `json:"observation_procedure,omitempty"`
	ContextID            string                `json:"context_id"`
	ObligationID         string                `json:"obligation_id"`
	Description          string                `json:"description"`
	Sources              []VerificationSource  `json:"sources"`
	Contract             verification.Exercise `json:"contract"`
}
type VerificationStartRequest struct {
	ContextID             string                       `json:"context_id"`
	StartKey              string                       `json:"start_key"`
	Subject               core.VerificationSubject     `json:"subject"`
	Environment           core.VerificationEnvironment `json:"environment"`
	EffectivePermissions  []verification.Permission    `json:"effective_permissions"`
	SafeInputs            map[string]json.RawMessage   `json:"safe_inputs"`
	ReplayAuthorizationID string                       `json:"replay_authorization_id,omitempty"`
}
type VerificationOutcomeRequest struct {
	ContextID   string `json:"context_id"`
	RunID       string `json:"run_id"`
	State       string `json:"state"`
	Explanation string `json:"explanation"`
	ExitCode    *int   `json:"exit_code,omitempty"`
}
type VerificationEvidenceRequest struct {
	ContextID     string             `json:"context_id"`
	RunID         string             `json:"run_id"`
	SubmissionKey string             `json:"submission_key"`
	Evidence      []json.RawMessage  `json:"evidence"`
	Links         []VerificationLink `json:"links,omitempty"`
}
type VerificationArtifactFinalize struct {
	Name               string `json:"name"`
	ContentType        string `json:"content_type"`
	SizeBytes          int64  `json:"size_bytes"`
	SHA256             string `json:"sha256"`
	SanitationRecord   string `json:"sanitation_record,omitempty"`
	MaskingAttestation string `json:"masking_attestation,omitempty"`
}
type VerificationArtifactRequest struct {
	ContextID string                        `json:"context_id"`
	RunID     string                        `json:"run_id"`
	UploadID  string                        `json:"upload_id"`
	Index     *int                          `json:"index,omitempty"`
	Content   []byte                        `json:"content,omitempty"`
	Finalize  *VerificationArtifactFinalize `json:"finalize,omitempty"`
}
type VerificationReadRequest struct {
	ContextID  string `json:"context_id"`
	EvidenceID string `json:"evidence_id"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Offset     int    `json:"offset,omitempty"`
}

// VerificationRequestType is the shared wire contract for REST, MCP and worker
// compatibility handlers. Authority and store commands are never wire payloads.
func VerificationRequestType(operation string) any {
	switch operation {
	case "prepare_verification":
		return &VerificationPrepareRequest{}
	case "get_verification_context", "get_verification_publication":
		return &VerificationContextRequest{}
	case "register_verification_obligation":
		return &VerificationObligationRequest{}
	case "start_verification_attempt":
		return &VerificationStartRequest{}
	case "report_verification_outcome":
		return &VerificationOutcomeRequest{}
	case "submit_verification_evidence":
		return &VerificationEvidenceRequest{}
	case "upload_verification_artifact":
		return &VerificationArtifactRequest{}
	case "read_verification_evidence":
		return &VerificationReadRequest{}
	case "get_evidence_schemas":
		return &struct{}{}
	}
	return nil
}

func (s *Service) verificationAccess(ctx context.Context, id, session, token string, write bool) (store.VerificationStore, store.VerificationAccess, core.WorkOrder, error) {
	backend, ok := s.Store.(store.VerificationStore)
	if !ok {
		return nil, store.VerificationAccess{}, core.WorkOrder{}, fmt.Errorf("verification store unavailable")
	}
	refuse := func() (store.VerificationStore, store.VerificationAccess, core.WorkOrder, error) {
		return nil, store.VerificationAccess{}, core.WorkOrder{}, store.ErrVerificationAccess
	}
	ws, ok := store.WorkspaceFromContext(ctx)
	if !ok {
		return refuse()
	}
	o, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return refuse()
	}
	task, err := s.Store.GetTask(ctx, o.TaskID)
	if err != nil || task.Workspace != ws {
		return refuse()
	}
	a := store.VerificationAccess{TaskID: task.ID, WorkOrderID: o.ID, WorkOrderAttemptID: o.AttemptID, ClientToken: token, Claim: core.WorkOrderClaimIdentity{WorkerID: o.WorkerID, ClaimantID: o.ClaimantID, SessionID: session}}
	actor := store.ActorFromContext(ctx)
	if !write && session == "" && actor.Role == core.ActorUser {
		a.UserID = strings.TrimPrefix(actor.ID, "user:")
		membership, ok := s.Store.(store.MembershipStore)
		if !ok {
			return refuse()
		}
		if store.AuthorizeVerificationUserRead(ctx, membership, a) != nil {
			return refuse()
		}
	} else if store.VerifyVerificationClaim(ctx, a, task, o, write, time.Now().UTC()) != nil {
		return refuse()
	}
	return backend, a, o, nil
}

// Verification runs no repository code and makes no acceptance decision (VK-8).
func (s *Service) Verification(ctx context.Context, id, session, token, operation string, raw []byte) (any, error) {
	request := VerificationRequestType(operation)
	if request == nil {
		return nil, store.ErrVerificationInvalid
	}
	if len(raw) > 8<<20 {
		return nil, store.ErrVerificationInvalid
	}
	write := operation != "get_verification_context" && operation != "get_verification_publication" && operation != "read_verification_evidence"
	var backend store.VerificationStore
	var a store.VerificationAccess
	var o core.WorkOrder
	var err error
	if operation != "get_evidence_schemas" {
		backend, a, o, err = s.verificationAccess(ctx, id, session, token, write)
		if err != nil {
			return nil, err
		}

	}
	if err := core.ValidateVerificationWire(raw, reflect.TypeOf(request)); err != nil {
		return nil, store.ErrVerificationInvalid
	}
	if err := core.DecodeVerificationRequest(raw, request); err != nil {
		return nil, store.ErrVerificationInvalid
	}
	if operation == "get_evidence_schemas" {
		return core.VerificationEvidenceSchemas(), nil
	}
	command := store.VerificationCommand{Access: a}
	switch r := request.(type) {
	case *VerificationPrepareRequest:
		return s.prepareVerification(ctx, backend, a, o, *r)
	case *VerificationContextRequest:
		snapshot, err := backend.ReadVerification(ctx, a, r.ContextID)
		if err != nil {
			return nil, err
		}
		if operation == "get_verification_publication" {
			return snapshot.Publications, nil
		}
		return snapshot, nil
	case *VerificationObligationRequest:
		command.Kind, command.ContextID = store.VerificationRegisterObligation, r.ContextID
		command.Obligation = &store.VerificationObligation{ID: r.ObligationID, Description: r.Description, Contract: r.Contract, ObservationProcedure: r.ObservationProcedure}
		for _, source := range r.Sources {
			command.Obligation.Sources = append(command.Obligation.Sources, store.VerificationCitation{DocumentID: source.DocumentID, Version: source.Version, SectionID: source.SectionID})
		}
	case *VerificationStartRequest:
		command.Kind, command.ContextID, command.Key = store.VerificationStartAttempt, r.ContextID, r.StartKey
		if !validVerificationSubject(r.Subject) {
			return nil, store.ErrVerificationInvalid
		}
		command.Attempt = &store.VerificationAttempt{Subject: r.Subject, Environment: r.Environment, SafeInputs: r.SafeInputs, EffectivePermissions: r.EffectivePermissions, ReplayAuthorizationID: r.ReplayAuthorizationID}
	case *VerificationOutcomeRequest:
		command.Kind, command.ContextID, command.RunID = store.VerificationTerminateAttempt, r.ContextID, r.RunID
		command.Attempt = &store.VerificationAttempt{State: r.State, Explanation: r.Explanation, ExitCode: r.ExitCode}
		if r.State == "succeeded" {
			command.ValidateSuccess = func(snapshot store.VerificationSnapshot) error {
				return ValidateVerificationSuccess(snapshot, r.RunID, r.ExitCode)
			}
		}
	case *VerificationEvidenceRequest:
		command.Kind, command.ContextID, command.RunID, command.Key = store.VerificationWriteEvidence, r.ContextID, r.RunID, r.SubmissionKey
		for _, raw := range r.Evidence {
			var fields map[string]json.RawMessage
			if err := core.DecodeVerificationRequest(raw, &fields); err != nil {
				return nil, store.ErrVerificationInvalid
			}
			actor := store.ActorFromContext(ctx).ID
			if supplied, ok := fields["submitted_by"]; ok {
				var name string
				if json.Unmarshal(supplied, &name) != nil || name != "" && name != actor {
					return nil, store.ErrVerificationAccess
				}
			}
			fields["submitted_by"] = core.JSONPayload(actor)
			fields["received_at"] = core.JSONPayload(time.Now().UTC().Format(time.RFC3339Nano))
			encoded, err := json.Marshal(fields)
			if err != nil {
				return nil, store.ErrVerificationInvalid
			}
			command.Evidence = append(command.Evidence, encoded)
		}
		for _, link := range r.Links {
			command.Links = append(command.Links, store.VerificationEvidenceLink{From: link.From, To: link.To})
		}
	case *VerificationArtifactRequest:
		command.ContextID, command.RunID = r.ContextID, r.RunID
		if r.Finalize != nil {
			if r.Index != nil || len(r.Content) != 0 {
				return nil, store.ErrVerificationInvalid
			}
			command.Kind, command.Key, command.Artifacts = store.VerificationFinalizeArtifact, r.UploadID, []store.VerificationArtifactInput{{UploadID: r.UploadID, Name: r.Finalize.Name, ContentType: r.Finalize.ContentType, SizeBytes: r.Finalize.SizeBytes, SHA256: r.Finalize.SHA256, SanitationRecord: r.Finalize.SanitationRecord, MaskingAttestation: r.Finalize.MaskingAttestation}}
		} else {
			if r.Index == nil {
				return nil, store.ErrVerificationInvalid
			}
			command.Kind = store.VerificationStageChunk
			command.Chunk = &store.VerificationUploadChunk{UploadID: r.UploadID, Index: *r.Index, Content: r.Content}
		}
	case *VerificationReadRequest:
		snapshot, err := backend.ReadVerification(ctx, a, r.ContextID)
		if err != nil {
			return nil, err
		}
		for _, e := range snapshot.Evidence {
			if e.Envelope.ID == r.EvidenceID {
				if r.ArtifactID == "" {
					return e, nil
				}
				artifact, data, err := backend.ReadVerificationArtifact(ctx, a, r.EvidenceID, r.ArtifactID)
				if err != nil {
					return nil, err
				}
				if r.Offset < 0 || r.Offset > len(data) {
					return nil, store.ErrVerificationInvalid
				}
				end := min(len(data), r.Offset+(512<<10))
				return struct {
					Artifact   core.Artifact `json:"artifact"`
					Content    []byte        `json:"content"`
					NextOffset int           `json:"next_offset"`
					Complete   bool          `json:"complete"`
				}{artifact, data[r.Offset:end], end, end == len(data)}, nil
			}
		}
		return nil, store.ErrVerificationAccess
	}
	return backend.ApplyVerification(ctx, command)
}

func (s *Service) prepareVerification(ctx context.Context, b store.VerificationStore, a store.VerificationAccess, o core.WorkOrder, r VerificationPrepareRequest) (any, error) {
	if r.RequestKey == "" {
		return nil, store.ErrVerificationInvalid
	}
	scopeBytes, _ := json.Marshal(r)
	scopeHash := fmt.Sprintf("%x", sha256.Sum256(scopeBytes))
	if snapshot, err := b.ReadVerification(ctx, a, "request:"+r.RequestKey); err == nil {
		if snapshot.Contexts[0].WorkOrderAttemptID != a.WorkOrderAttemptID || snapshot.Contexts[0].RequestDigest != scopeHash {
			return nil, store.ErrVerificationConflict
		}
		return snapshot, nil
	} else if !errors.Is(err, store.ErrVerificationAccess) {
		return nil, err
	}
	task, err := s.Store.GetTask(ctx, o.TaskID)
	if err != nil {
		return nil, store.ErrVerificationAccess
	}
	cfg, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	pins := []core.VerificationPin{}
	for _, p := range o.ServedRequirementSnapshot {
		pins = append(pins, core.VerificationPin{Kind: "requirement", DocumentID: p.ID, Version: p.Version})
	}
	if o.GovernanceSnapshot != nil {
		for _, p := range o.GovernanceSnapshot.Designs {
			pins = append(pins, core.VerificationPin{Kind: "system_design", DocumentID: p.ID, Version: p.Version})
		}
	}
	apps := s.GitHubApps
	if apps == nil {
		apps = github.DefaultAppClient
	}
	scope := append([]VerificationScopeRevision{{Repository: task.Repo, SHA: o.HeadSHA}}, r.AdditionalRevisions...)
	if len(scope) > 100 {
		return nil, store.ErrVerificationInvalid
	}
	sort.Slice(scope, func(i, j int) bool { return scope[i].Repository < scope[j].Repository })
	selectionPins := make([]verification.Pin, len(pins))
	for i, p := range pins {
		selectionPins[i] = verification.Pin{Kind: p.Kind, DocumentID: p.DocumentID, Version: p.Version}
	}
	selection := store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, ContextPins: selectionPins, Stage: "verify", ManifestRevision: o.HeadSHA, SourceRevision: o.HeadSHA, Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{}}, Subjects: []store.VerificationSubjectContract{}}
	vc := store.VerificationContext{RequestDigest: scopeHash, GoverningPins: pins, Discovery: []json.RawMessage{}}
	seenRepos, seenKits := map[string]bool{}, map[string]bool{}
	for _, source := range scope {
		repo, ok := cfg.Repo(source.Repository)
		if !ok || seenRepos[source.Repository] {
			return nil, store.ErrVerificationInvalid
		}
		seenRepos[source.Repository] = true
		d := apps.DiscoverVerification(ctx, s.WorkspaceGitHubApps, task.Workspace, repo.GitHub, source.SHA)
		manifest := d.Manifest
		if d.State == "no_manifest" {
			manifest = &verification.Manifest{SchemaVersion: 1, Kits: []verification.Kit{}}
		}
		receipt := verification.Evaluate(manifest, verification.SelectionContext{Pins: selectionPins, Stage: "verify", ManifestRevision: source.SHA, SourceRevision: source.SHA}, d.Trees)
		if d.State != "present" && d.State != "no_manifest" {
			receipt.Diagnostics = append(receipt.Diagnostics, verification.Diagnostic{Path: "discovery." + source.Repository, Message: d.State})
		}
		for _, selected := range receipt.Kits {
			if seenKits[selected.KitID] {
				return nil, fmt.Errorf("%w: duplicate scoped kit identity", store.ErrVerificationInvalid)
			}
			seenKits[selected.KitID] = true
			if selected.Eligibility == "eligible" && d.State == "present" {
				for _, kit := range d.Manifest.Kits {
					if kit.ID == selected.KitID {
						for _, exercise := range kit.Exercises {
							stageMatch := false
							for _, stage := range exercise.Stages {
								if stage == "verify" {
									stageMatch = true
								}
							}
							if !stageMatch {
								continue
							}
							selection.Subjects = append(selection.Subjects, store.VerificationSubjectContract{Subject: core.VerificationSubject{Kind: "kit", KitID: kit.ID, KitVersion: kit.Version, ContentDigest: selected.Digest, ExerciseID: exercise.ID}, Contract: exercise})
						}
					}
				}
			}
		}
		selection.Receipt.Kits = append(selection.Receipt.Kits, receipt.Kits...)
		selection.Receipt.Diagnostics = append(selection.Receipt.Diagnostics, receipt.Diagnostics...)
		discovery, _ := json.Marshal(struct {
			Repository string `json:"repository"`
			github.VerificationDiscovery
		}{source.Repository, d})
		vc.Discovery = append(vc.Discovery, discovery)
		vc.Revisions = append(vc.Revisions, core.VerificationRevision{Repository: source.Repository, RemoteIdentity: repo.URL, SHA: source.SHA})
	}
	result, err := b.ApplyVerification(ctx, store.VerificationCommand{Access: a, Kind: store.VerificationCreateContext, Key: r.RequestKey, Context: &vc, Selection: &selection})
	if errors.Is(err, store.ErrVerificationConflict) {
		snapshot, readErr := b.ReadVerification(ctx, a, "request:"+r.RequestKey)
		if readErr != nil {
			return nil, readErr
		}
		if snapshot.Contexts[0].RequestDigest != scopeHash {
			return nil, store.ErrVerificationConflict
		}
		return snapshot, nil
	}
	if err != nil {
		return nil, err
	}
	return b.ReadVerification(ctx, a, result.ID)
}

func validVerificationSubject(s core.VerificationSubject) bool {
	if s.Kind == "ordinary" {
		return s.ObligationID != "" && len(s.ContractDigest) == 64 && s.KitID == "" && s.KitVersion == "" && s.ContentDigest == "" && s.ExerciseID == ""
	}
	return s.Kind == "kit" && s.KitID != "" && s.KitVersion != "" && len(s.ContentDigest) == 64 && s.ExerciseID != "" && s.ObligationID == "" && s.ContractDigest == ""
}

// ValidateVerificationSuccess evaluates only evidence from the frozen subject
// and attempt. The sealing task also calls this evaluator inside its transaction
// (feature-verification-kit-execution VK-7.1; req-verification-kits REQ-4).
func ValidateVerificationSuccess(snapshot store.VerificationSnapshot, runID string, exit *int) error {
	var attempt *store.VerificationAttempt
	for i := range snapshot.Attempts {
		if snapshot.Attempts[i].ID == runID {
			attempt = &snapshot.Attempts[i]
		}
	}
	if attempt == nil {
		return store.ErrVerificationAccess
	}
	var contract *verification.Exercise
	for _, s := range snapshot.Selections {
		for _, subject := range s.Subjects {
			if subject.Subject == attempt.Subject {
				v := subject.Contract
				contract = &v
			}
		}
	}
	for _, o := range snapshot.Obligations {
		if attempt.Subject.Kind == "ordinary" && o.ID == attempt.Subject.ObligationID && o.Digest == attempt.Subject.ContractDigest {
			v := o.Contract
			contract = &v
		}
	}
	if contract == nil {
		return store.ErrVerificationState
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && (exit == nil || *exit != 0) {
		return store.ErrVerificationState
	}
	counts := map[string]int{}
	assertions := map[string]core.AssertionResultPayload{}
	evidence := map[string]core.VerificationEvidence{}
	execution, interaction := false, false
	for _, record := range snapshot.Evidence {
		e := record.Envelope
		if e.RunID != runID || e.Subject != attempt.Subject {
			continue
		}
		evidence[e.ID] = e
		counts[e.Type]++
		switch e.Type {
		case "assertion_result":
			var p core.AssertionResultPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				return store.ErrVerificationInvalid
			}
			if _, ok := assertions[p.AssertionID]; ok {
				return store.ErrVerificationInvalid
			}
			assertions[p.AssertionID] = p
		case "execution_report":
			var p core.ExecutionReportPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.ExitCode != nil && *p.ExitCode == 0 && p.TimedOut != nil && !*p.TimedOut && p.Cancelled != nil && !*p.Cancelled {
				execution = true
			}
		case "operator_observation":
			interaction = true
		}
	}
	if (contract.Kind == "script" || contract.Kind == "hybrid") && !execution {
		return store.ErrVerificationState
	}
	if (contract.Kind == "interactive" || contract.Kind == "hybrid") && !interaction {
		return store.ErrVerificationState
	}
	if contract.Kind == "observation" && len(contract.EvidenceOutputs) == 0 {
		return store.ErrVerificationState
	}
	for _, output := range contract.EvidenceOutputs {
		if counts[output.Type] < output.MinimumItems {
			return store.ErrVerificationState
		}
	}
	for _, id := range contract.RequiredAssertions {
		p, ok := assertions[id]
		if !ok || p.Outcome != "pass" || len(p.Supporting) == 0 {
			return store.ErrVerificationState
		}
		for _, ref := range p.Supporting {
			if ref.EvidenceID != "" {
				if _, ok := evidence[ref.EvidenceID]; !ok {
					return store.ErrVerificationInvalid
				}
			} else {
				found := false
				for _, e := range evidence {
					for _, a := range e.Artifacts {
						if a.ArtifactID == ref.ArtifactID && a.SHA256 == ref.SHA256 {
							found = true
						}
					}
				}
				if !found {
					return store.ErrVerificationInvalid
				}
			}
		}
	}
	for _, op := range snapshot.Operations {
		if op.RunID == runID && len(op.History) > 0 {
			state := op.History[len(op.History)-1].State
			if state != "applied" && state != "not_applied" && state != "completed" {
				return store.ErrVerificationState
			}
		}
	}
	return nil
}

// ReconcileVerificationClaims is called by the daemon workspace reconciliation
// loop. It never impersonates a worker or renews an abandoned execution.
func (s *Service) ReconcileVerificationClaims(ctx context.Context) (int, error) {
	b, ok := s.Store.(store.VerificationStore)
	if !ok {
		return 0, nil
	}
	actor := store.ActorFromContext(ctx)
	if actor.Role != core.ActorSystem {
		return 0, store.ErrVerificationAccess
	}
	return b.ReconcileVerificationClaims(store.WithActor(ctx, store.Actor{ID: "verification-reconciler", Role: core.ActorSystem}))
}
