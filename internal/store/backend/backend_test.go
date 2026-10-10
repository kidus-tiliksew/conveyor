package backend_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
)

func TestBackendSelection(t *testing.T) {
	for _, name := range []string{"", "unsupported"} {
		if _, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: name}); !errors.Is(err, backend.ErrUnknownBackend) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: "memory"}); !errors.Is(err, backend.ErrVolatileBackend) {
		t.Fatalf("memory selection: %v", err)
	}
	ctx, cancel := context.WithCancel(store.WithActor(t.Context(), store.SystemActor()))
	cancel()
	if _, err := backend.Open(ctx, config.Database{Backend: "postgres", URL: "postgres://localhost/conveyor_test"}); err == nil || errors.Is(err, backend.ErrUnknownBackend) {
		t.Fatalf("postgres selection: %v", err)
	}
}

func TestVolatileOptInProvidesBackend(t *testing.T) {
	st, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: "memory"}, backend.AllowVolatile)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.IsDurable() || st.Log() == nil {
		t.Fatal("invalid volatile backend")
	}
	identity := config.FirstOperatorIdentity{OrganizationName: "Test", Email: "owner@example.test", DisplayName: "Owner"}
	if seeded, err := st.BootstrapIdentity(store.WithActor(t.Context(), store.SystemActor()), identity, "test-deployment-credential"); err != nil || !seeded {
		t.Fatalf("bootstrap: %v %v", seeded, err)
	}
	owner, err := st.VerifyPersonalAccessToken(store.WithActor(t.Context(), store.SystemActor()), "test-deployment-credential")
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithCredential(store.WithActor(t.Context(), store.SystemActor()), core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	ctx = store.WithWorkspace(ctx, "test")
	cfg := &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", Base: "main"}}}
	if _, err := st.CreateWorkspace(ctx, "test", "Test", cfg); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AuthorizeWorkspace(ctx, owner.ID, "test", core.CapabilityManageMembership); err != nil || !allowed {
		t.Fatalf("owner authorization: %v %v", allowed, err)
	}
	link, err := st.IssueSignInLink(ctx, identity.Email)
	if err != nil {
		t.Fatal(err)
	}
	session, user, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil || user.ID != owner.ID {
		t.Fatalf("redeem: %v", err)
	}
	if _, _, err := st.RedeemSignInLink(ctx, link.Value); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("link reused: %v", err)
	}
	if _, err := st.VerifyDashboardSession(ctx, session.Value); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeDashboardSession(ctx, owner.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.VerifyDashboardSession(ctx, session.Value); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("revoked session accepted: %v", err)
	}
}

func TestSingleStoreAdmission(t *testing.T) {
	database := config.Database{Backend: "singlestore", URL: "invalid"}
	for _, options := range [][]backend.Option{nil, {backend.AllowVolatile}} {
		if _, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), database, options...); err == nil || errors.Is(err, backend.ErrUnknownBackend) {
			t.Fatalf("SingleStore selection did not reach driver validation: %v", err)
		}
	}
}
