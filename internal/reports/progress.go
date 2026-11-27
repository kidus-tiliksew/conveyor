package reports

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// ResolvePlan is the next step of resolving a report's first entry from its
// stored progress (component-production-feedback invariant 7;
// component-feedback-reports invariant 12; approved plan version 3). When
// Final is set, resolving has a definitive result to store: the resolution
// or a definitive unresolved reason. Otherwise Reads lists, in configuration
// order, the candidate repositories whose commit read has no stored answer
// for this input, each read with Ref, and an answer counts as a match only
// when the full commit starts with Prefix. Input is the digest of Ref and
// Prefix, and Keep the canonical slugs of the current candidates: a stored
// answer is saved with both, so it is used only with the input it was read
// with and kept only while its slug is a candidate.
type ResolvePlan struct {
	Final  *store.FeedbackLineStep
	Reads  []feedback.Repository
	Ref    string
	Prefix string
	Input  string
	Keep   []string
}

// Step is the stored form of one candidate's definitive answer under the
// plan's input.
func (p ResolvePlan) Step(candidate feedback.Repository, answer string) store.FeedbackLineStep {
	return store.FeedbackLineStep{Candidate: store.FeedbackRepository(candidate.Slug), Answer: answer, Input: p.Input, Keep: slices.Clone(p.Keep)}
}

// Prune is the step that drops stored answers the plan does not use: all of
// them when they were read with another input, and those of slugs that are
// no longer candidates. ok is false when the stored answers need no change,
// and for a plan with a final result, whose step drops them itself.
func (p ResolvePlan) Prune(progress *store.FeedbackLineProgress) (store.FeedbackLineStep, bool) {
	if p.Final != nil || progress == nil || progress.Ended() || len(progress.Candidates) == 0 {
		return store.FeedbackLineStep{}, false
	}
	stale := progress.Input != p.Input
	for slug := range progress.Candidates {
		if !slices.Contains(p.Keep, slug) {
			stale = true
		}
	}
	if !stale {
		return store.FeedbackLineStep{}, false
	}
	return store.FeedbackLineStep{Input: p.Input, Keep: slices.Clone(p.Keep)}, true
}

// resolveInput is the digest of one resolution input: the ref read and the
// prefix a match needs. Only the digest is stored, so the progress holds no
// report text.
func resolveInput(ref, prefix string) string {
	sum := sha256.Sum256([]byte(ref + "\x00" + prefix))
	return hex.EncodeToString(sum[:])
}

// PlanResolve decides the next step of resolving with the rules of
// feedback.Resolve: a stated commit comes first; without one the
// environment's version rule gives a commit or a tag; a named repository is
// the only candidate, and otherwise every configured repository with a slug
// is; two matches are ambiguous; one match among answered candidates is the
// resolution; no match among answered candidates is no_match. It reads
// stored answers only, by each candidate's current GitHub slug, and never
// asks for one of them again. Stored answers read with another input, after
// the version rule changed, are not used: every candidate is read again with
// the current input, and no answer for one input is combined with an answer
// for another (component-production-feedback invariant 7;
// component-feedback-reports invariant 12). A stored resolution or reason is
// final and is used whatever the current input is.
func PlanResolve(request feedback.ResolveRequest, progress *store.FeedbackLineProgress) ResolvePlan {
	if progress.Ended() {
		if progress.Unresolved != "" {
			return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: progress.Unresolved}}
		}
		return ResolvePlan{Final: &store.FeedbackLineStep{Repository: progress.Repository, Slug: progress.Slug, Commit: progress.Commit}}
	}
	candidates := []feedback.Repository{}
	for _, repository := range request.Repositories {
		if request.Version.Repository != "" && repository.Name != request.Version.Repository {
			continue
		}
		if strings.TrimSpace(repository.Slug) != "" {
			candidates = append(candidates, repository)
		}
	}
	if len(candidates) == 0 {
		return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoRepository}}
	}
	plan := ResolvePlan{}
	switch {
	case request.Version.Commit != "":
		plan.Prefix = strings.ToLower(request.Version.Commit)
		plan.Ref = plan.Prefix
	case request.Rule == nil:
		return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoVersionRule}}
	default:
		target, ok, err := feedback.ApplyVersionRule(*request.Rule, request.Version.Version, request.Service)
		if err != nil || !ok {
			return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: store.FeedbackReasonRuleMismatch}}
		}
		if target.Commit != "" {
			plan.Prefix, plan.Ref = target.Commit, target.Commit
		} else {
			plan.Ref = "tags/" + target.Tag
		}
	}
	plan.Input = resolveInput(plan.Ref, plan.Prefix)
	for _, candidate := range candidates {
		if slug := store.FeedbackRepository(candidate.Slug); !slices.Contains(plan.Keep, slug) {
			plan.Keep = append(plan.Keep, slug)
		}
	}
	slices.Sort(plan.Keep)
	matches := []feedback.Repository{}
	answers := map[string]string{}
	if progress != nil && progress.Input == plan.Input {
		answers = progress.Candidates
	}
	// An answer is keyed by the GitHub slug read, so a name that now points at
	// another repository has no answer and is read again.
	for _, candidate := range candidates {
		answer, answered := answers[store.FeedbackRepository(candidate.Slug)]
		switch {
		case !answered:
			plan.Reads = append(plan.Reads, candidate)
		case answer != store.FeedbackLineAbsent:
			matches = append(matches, candidate)
		}
	}
	switch {
	case len(matches) > 1:
		return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: store.FeedbackReasonAmbiguous}}
	case len(plan.Reads) > 0:
		return plan
	case len(matches) == 1:
		slug := store.FeedbackRepository(matches[0].Slug)
		return ResolvePlan{Final: &store.FeedbackLineStep{Repository: matches[0].Name, Slug: slug, Commit: answers[slug]}}
	}
	return ResolvePlan{Final: &store.FeedbackLineStep{Unresolved: store.FeedbackReasonNoMatch}}
}

// CandidateAnswer is the definitive answer of one candidate's commit read,
// as feedback.Resolve reads it: a full commit that starts with the prefix is
// a match, and any other commit or a commit GitHub does not find is an
// absence. ok is false for every other failure, which proves nothing and is
// never stored.
func CandidateAnswer(prefix, commit string, err error) (answer string, ok bool) {
	switch {
	case err == nil && store.FeedbackCommitID(commit) && strings.HasPrefix(commit, prefix):
		return commit, true
	case err == nil, errors.Is(err, github.ErrCommitNotFound):
		return store.FeedbackLineAbsent, true
	}
	return "", false
}

// TimingResultOf is the stored form of an ended timing outcome: each piece
// of timing evidence by task and deploy ID, and each note by deploy ID.
func TimingResultOf(outcome Outcome) store.FeedbackTimingResult {
	result := store.FeedbackTimingResult{Links: []store.FeedbackTimingLink{}, Notes: []store.FeedbackTimingNote{}}
	for _, link := range outcome.Links {
		for _, piece := range link.Evidence {
			if piece.Deploy == nil {
				continue
			}
			stored := store.FeedbackTimingLink{TaskID: link.TaskID, DeployID: piece.Deploy.ID, PriorSignalAt: piece.PriorSignalAt}
			if piece.ElapsedMinutes != nil {
				stored.ElapsedMinutes = *piece.ElapsedMinutes
			}
			result.Links = append(result.Links, stored)
		}
	}
	for _, note := range outcome.Notes {
		if note.Deploy == nil {
			continue
		}
		result.Notes = append(result.Notes, store.FeedbackTimingNote{Kind: note.Kind, DeployID: note.Deploy.ID, TaskCount: note.TaskCount})
	}
	return result
}

// TimingFromResult rebuilds the ended timing outcome from its stored form
// and the environment's deploy records, which never change the fields
// evidence names. A stored deploy ID that names no deploy is an error.
func TimingFromResult(result store.FeedbackTimingResult, deploys []store.FeedbackDeploy) (Outcome, error) {
	byID := map[string]store.FeedbackDeploy{}
	for _, deploy := range deploys {
		byID[deploy.ID] = deploy
	}
	outcome := Outcome{Ended: true}
	index := map[string]int{}
	for _, link := range result.Links {
		deploy, found := byID[link.DeployID]
		if !found {
			return Outcome{}, fmt.Errorf("the stored timing result names a deploy the environment does not hold")
		}
		elapsed := link.ElapsedMinutes
		piece := store.FeedbackReportEvidence{Kind: store.FeedbackEvidenceTiming, Deploy: evidenceDeploy(deploy), ElapsedMinutes: &elapsed, PriorSignalAt: link.PriorSignalAt}
		i, seen := index[link.TaskID]
		if !seen {
			i = len(outcome.Links)
			index[link.TaskID] = i
			outcome.Links = append(outcome.Links, store.FeedbackReportLinkCandidate{TaskID: link.TaskID})
		}
		outcome.Links[i].Evidence = append(outcome.Links[i].Evidence, piece)
	}
	slices.SortStableFunc(outcome.Links, func(a, b store.FeedbackReportLinkCandidate) int { return compareStrings(a.TaskID, b.TaskID) })
	for _, note := range result.Notes {
		deploy, found := byID[note.DeployID]
		if !found {
			return Outcome{}, fmt.Errorf("the stored timing result names a deploy the environment does not hold")
		}
		outcome.Notes = append(outcome.Notes, store.FeedbackReportNote{Kind: note.Kind, Deploy: evidenceDeploy(deploy), TaskCount: note.TaskCount})
	}
	return outcome, nil
}
