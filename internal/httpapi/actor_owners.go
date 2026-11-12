package httpapi

import (
	"context"
	"fmt"
	"slices"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// agentActorOwners maps each agent:<id> actor among actorIDs to the user who
// owns that stored credential, so an act recorded under a retired agent
// credential stays readable and resolves to its owner wherever the actor is
// displayed (req-security-boundaries AC-1.3; DEC-116(3);
// component-identity-membership invariant 35). The result has no entry for an
// unknown credential or for any other actor, such as a review round's
// review:round:<n>, so neither fails the read and no owner is invented. A read
// naming no agent:<id> actor issues no query. A store failure is returned, never
// presented as an unresolved actor.
func (s *Server) agentActorOwners(ctx context.Context, actorIDs []string) (map[string]string, error) {
	if s.ActorOwners == nil {
		return nil, nil
	}
	var credentialIDs []string
	for _, actorID := range actorIDs {
		if id, ok := store.AgentActorCredentialID(actorID); ok && !slices.Contains(credentialIDs, id) {
			credentialIDs = append(credentialIDs, id)
		}
	}
	if len(credentialIDs) == 0 {
		return nil, nil
	}
	owners, err := s.ActorOwners.AgentActorOwners(ctx, credentialIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve agent actor owners: %w", err)
	}
	byActor := make(map[string]string, len(owners))
	for _, actorID := range actorIDs {
		if id, ok := store.AgentActorCredentialID(actorID); ok && owners[id] != "" {
			byActor[actorID] = owners[id]
		}
	}
	return byActor, nil
}

// annotateEventActorOwners sets ActorOwnerUserID on each event a read presents.
// The recorded actor and the stored event stay as written.
func (s *Server) annotateEventActorOwners(ctx context.Context, events []core.Event) error {
	actorIDs := make([]string, 0, len(events))
	for _, event := range events {
		actorIDs = append(actorIDs, event.ActorID)
	}
	owners, err := s.agentActorOwners(ctx, actorIDs)
	if err != nil {
		return err
	}
	for index := range events {
		events[index].ActorOwnerUserID = owners[events[index].ActorID]
	}
	return nil
}

// annotateInterventionActorOwners sets ActorOwnerUserID on each intervention a
// read presents.
func (s *Server) annotateInterventionActorOwners(ctx context.Context, interventions []core.Intervention) error {
	actorIDs := make([]string, 0, len(interventions))
	for _, intervention := range interventions {
		actorIDs = append(actorIDs, intervention.ActorID)
	}
	owners, err := s.agentActorOwners(ctx, actorIDs)
	if err != nil {
		return err
	}
	for index := range interventions {
		interventions[index].ActorOwnerUserID = owners[interventions[index].ActorID]
	}
	return nil
}

// annotateSummaryActorOwners sets the owner on the attributions the task
// detail summaries present: the task's creator, each round summary's author,
// and the person of each plan-cycle step that records an actor, in one batch.
func (s *Server) annotateSummaryActorOwners(ctx context.Context, summaries *taskDetailSummaries) error {
	var actorIDs []string
	if summaries.Creator != nil {
		actorIDs = append(actorIDs, summaries.Creator.ActorID)
	}
	for _, entry := range summaries.ReviewRoundSummaries {
		actorIDs = append(actorIDs, entry.Author.ActorID)
	}
	actorIDs = append(actorIDs, planCycleActorIDs(summaries.PlanCycles)...)
	owners, err := s.agentActorOwners(ctx, actorIDs)
	if err != nil {
		return err
	}
	resolvePlanCyclePeople(summaries.PlanCycles, owners)
	if summaries.Creator != nil {
		summaries.Creator.ActorOwnerUserID = owners[summaries.Creator.ActorID]
	}
	for round, entry := range summaries.ReviewRoundSummaries {
		entry.Author.ActorOwnerUserID = owners[entry.Author.ActorID]
		summaries.ReviewRoundSummaries[round] = entry
	}
	return nil
}
