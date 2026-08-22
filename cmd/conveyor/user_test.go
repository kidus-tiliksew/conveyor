package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type fakeSignInLinkStore struct {
	issued      core.IssuedSignInLink
	issueErr    error
	deliveryErr error
	email       string
	outcome     string
	actor       store.Actor
	closed      bool
}

func (f *fakeSignInLinkStore) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	f.email = email
	f.actor = store.ActorFromContext(ctx)
	return f.issued, f.issueErr
}

func (f *fakeSignInLinkStore) RecordInvitationDelivery(ctx context.Context, _ string, outcome string) error {
	f.actor = store.ActorFromContext(ctx)
	f.outcome = outcome
	return f.deliveryErr
}

func (f *fakeSignInLinkStore) Close() { f.closed = true }

func TestUserIssueLinkPrintsConfiguredURLAndAuditsManualDelivery(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://deployment")
	t.Setenv(config.PublicURLEnv, "https://conveyor.example/")
	fake := &fakeSignInLinkStore{issued: core.IssuedSignInLink{Email: "owner@example.test", Value: "cv_signin_secret", ExpiresAt: time.Now()}}
	command := newUserCmd(func(context.Context, string) (signInLinkStore, error) { return fake, nil })
	var output strings.Builder
	command.SetOut(&output)
	command.SetArgs([]string{"issue-link", " OWNER@example.test "})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if fake.email != " OWNER@example.test " || fake.outcome != "fallback" || fake.actor.ID != hostLocalSignInLinkActorID || fake.actor.Role != core.ActorSystem || !fake.closed {
		t.Fatalf("store call email=%q outcome=%q actor=%+v closed=%v", fake.email, fake.outcome, fake.actor, fake.closed)
	}
	if got := output.String(); !strings.Contains(got, "https://conveyor.example/sign-in#token=cv_signin_secret") {
		t.Fatalf("output=%q", got)
	}
}

func TestUserIssueLinkPrintsManualTokenFallback(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://deployment")
	t.Setenv(config.PublicURLEnv, "")
	fake := &fakeSignInLinkStore{issued: core.IssuedSignInLink{Email: "invitee@example.test", Value: "cv_signin_manual"}}
	command := newUserCmd(func(context.Context, string) (signInLinkStore, error) { return fake, nil })
	var output strings.Builder
	command.SetOut(&output)
	command.SetArgs([]string{"issue-link", "invitee@example.test"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "CONVEYOR_PUBLIC_URL is not configured") || !strings.Contains(got, "cv_signin_manual") || strings.Contains(got, "/sign-in#token=") {
		t.Fatalf("output=%q", got)
	}
}

func TestUserIssueLinkRefusesUnknownEmailAndMissingDatabase(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://deployment")
	fake := &fakeSignInLinkStore{issueErr: store.ErrNotFound}
	command := newUserCmd(func(context.Context, string) (signInLinkStore, error) { return fake, nil })
	command.SetArgs([]string{"issue-link", "unknown@example.test"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "no active account or pending invitation") {
		t.Fatalf("unknown email error=%v", err)
	}

	t.Setenv("CONVEYOR_DATABASE_URL", "")
	command = newUserCmd(func(context.Context, string) (signInLinkStore, error) { return nil, errors.New("must not open") })
	command.SetArgs([]string{"issue-link", "owner@example.test"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL is required") {
		t.Fatalf("missing database error=%v", err)
	}
}

func TestIssueAndPrintSignInLinkDoesNotPrintWhenAuditFails(t *testing.T) {
	fake := &fakeSignInLinkStore{issued: core.IssuedSignInLink{Email: "owner@example.test", Value: "cv_signin_never_print"}, deliveryErr: errors.New("audit unavailable")}
	var output strings.Builder
	err := issueAndPrintSignInLink(t.Context(), &output, fake, "owner@example.test", "https://conveyor.example")
	if err == nil || !strings.Contains(err.Error(), "record manual sign-in-link delivery") || output.Len() != 0 || strings.Contains(err.Error(), fake.issued.Value) {
		t.Fatalf("error=%v output=%q", err, output.String())
	}
}
