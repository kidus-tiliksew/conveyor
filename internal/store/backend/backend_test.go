package backend_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/connprobe"
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

func TestOpenWithStatsRefusesOutOfRangeMaxConns(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, value := range []int64{0, -1, 2147483648} {
		maxConns := value
		// The URL is unroutable: the refusal must come before any connection.
		st, stats, err := backend.OpenWithStats(ctx, config.Database{Backend: "postgres", URL: "postgres://u:pa55word@192.0.2.1:5432/conveyor?connect_timeout=1", MaxConns: &maxConns})
		if err == nil || st != nil || stats.Pool != nil || stats.Reads != nil {
			t.Fatalf("MaxConns %d: backend %v pool stats %v read stats %v err %v", value, st, stats.Pool != nil, stats.Reads != nil, err)
		}
		if err.Error() != "database.max_conns must be an integer from 1 to 2147483647" {
			t.Fatalf("MaxConns %d: error = %v", value, err)
		}
	}
}

func TestOpenWithStatsReturnsNoStatsForVolatile(t *testing.T) {
	st, stats, err := backend.OpenWithStats(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: "memory"}, backend.AllowVolatile)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if stats.Pool != nil || stats.Reads != nil {
		t.Fatal("the volatile backend reports no pool or read statistics")
	}
}

const paddedFixturePassword = "fixture-padded-pw-c41"

func paddedVariants(raw string) map[string]string {
	return map[string]string{
		"unpadded": raw, "leading spaces": "  " + raw, "trailing space": raw + " ",
		"trailing newline": raw + "\n", "both sides": "\t " + raw + " \r\n",
	}
}

// Every padded form reaches the driver as the unpadded string: the server sees
// the exact user and database, and no refusal or error repeats the password
// (req-deployment-and-releases REQ-2, AC-2.3; req-security-boundaries REQ-2).
func TestOpenWithStatsTrimsSurroundingWhitespace(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, tc := range []struct {
		name, backendName string
		probe             func(*testing.T) *connprobe.Probe
		connection        func(addr string) string
	}{
		{"postgres URL", "postgres", connprobe.Postgres, func(addr string) string {
			return "postgres://fixture_user:" + paddedFixturePassword + "@" + addr + "/fixture_db?sslmode=disable&connect_timeout=2"
		}},
		{"postgres keyword string", "postgres", connprobe.Postgres, func(addr string) string {
			host, port, _ := net.SplitHostPort(addr)
			return "host=" + host + " port=" + port + " user=fixture_user password=" + paddedFixturePassword + " dbname=fixture_db sslmode=disable connect_timeout=2"
		}},
		{"singlestore URL", "singlestore", connprobe.MySQL, func(addr string) string {
			return "singlestore://fixture_user:" + paddedFixturePassword + "@" + addr + "/fixture_db"
		}},
		{"mysql URL", "singlestore", connprobe.MySQL, func(addr string) string {
			return "mysql://fixture_user:" + paddedFixturePassword + "@" + addr + "/fixture_db"
		}},
		{"mysql DSN", "singlestore", connprobe.MySQL, func(addr string) string {
			return "fixture_user:" + paddedFixturePassword + "@tcp(" + addr + ")/fixture_db"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, variant := range []string{"unpadded", "leading spaces", "trailing space", "trailing newline", "both sides"} {
				// A probe per variant, so one variant's handshake can't stand in
				// for another that never connected.
				probe := tc.probe(t)
				value := paddedVariants(tc.connection(probe.Addr))[variant]
				_, _, err := backend.OpenWithStats(ctx, config.Database{Backend: tc.backendName, URL: value})
				if err == nil {
					t.Fatalf("%s: the probe accepted no session, so the open must fail", variant)
				}
				if strings.Contains(err.Error(), paddedFixturePassword) {
					t.Fatalf("%s: error repeats the password", variant)
				}
				probe.Require(t, variant, connprobe.Handshake{User: "fixture_user", Database: "fixture_db"})
			}
		})
	}
}

// Only surrounding whitespace comes off. Whitespace that belongs to a value
// inside the string survives.
func TestOpenWithStatsKeepsWhitespaceInsideTheConnectionString(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, tc := range []struct {
		name, backendName, want string
		probe                   func(*testing.T) *connprobe.Probe
		connection              func(addr string) string
	}{
		{"encoded trailing space in a postgres URL", "postgres", "fixture_db ", connprobe.Postgres, func(addr string) string {
			return "postgres://fixture_user:" + paddedFixturePassword + "@" + addr + "/fixture_db%20?sslmode=disable&connect_timeout=2"
		}},
		{"quoted value in a keyword string", "postgres", "fixture db ", connprobe.Postgres, func(addr string) string {
			host, port, _ := net.SplitHostPort(addr)
			return "host=" + host + " port=" + port + " user=fixture_user sslmode=disable connect_timeout=2 dbname='fixture db '"
		}},
		{"encoded trailing space in a singlestore URL", "singlestore", "fixture_db ", connprobe.MySQL, func(addr string) string {
			return "singlestore://fixture_user:" + paddedFixturePassword + "@" + addr + "/fixture_db%20"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := tc.probe(t)
			if _, _, err := backend.OpenWithStats(ctx, config.Database{Backend: tc.backendName, URL: "  " + tc.connection(probe.Addr) + "\n"}); err == nil {
				t.Fatal("the probe accepted no session, so the open must fail")
			}
			probe.Require(t, tc.name, connprobe.Handshake{User: "fixture_user", Database: tc.want})
		})
	}
}

// The control: without this boundary, the drivers refuse or misread the same
// padded strings. If a driver starts trimming, this test says the boundary and
// its regression no longer prove anything.
func TestDriversMisreadPaddedConnectionStringsWithoutTheBoundary(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	t.Run("postgres refuses trailing padding", func(t *testing.T) {
		raw := "postgres://fixture_user:" + paddedFixturePassword + "@127.0.0.1:1/fixture_db?sslmode=disable&connect_timeout=1"
		_, err := postgres.Open(ctx, raw+" \n")
		if err == nil || !strings.Contains(err.Error(), "the connection string or its pool_max_conns is invalid") {
			t.Fatalf("padded string reached past the parser: %v", err)
		}
	})
	t.Run("postgres misreads a leading-padded URL with a query", func(t *testing.T) {
		// pgx takes the string for keyword settings, so the URL's host never
		// applies and the whole URL, password included, becomes one parameter
		// name it would send to the driver's default server. This parses only.
		parsed, err := pgx.ParseConfig("  postgres://fixture_user:" + paddedFixturePassword + "@fixture-host.invalid:5432/fixture_db?sslmode=disable")
		if err != nil {
			t.Fatal("pgx now refuses this form, so update the control")
		}
		if parsed.Host == "fixture-host.invalid" || parsed.Database == "fixture_db" {
			t.Fatalf("pgx applied the URL: host %q database %q", parsed.Host, parsed.Database)
		}
		carried := false
		for name := range parsed.RuntimeParams {
			carried = carried || strings.Contains(name, paddedFixturePassword)
		}
		if !carried {
			t.Fatal("no startup parameter carries the password, so update the control")
		}
	})
	t.Run("singlestore misreads a DSN", func(t *testing.T) {
		probe := connprobe.MySQL(t)
		raw := "fixture_user:" + paddedFixturePassword + "@tcp(" + probe.Addr + ")/fixture_db"
		if _, err := singlestore.Open(ctx, "  "+raw+" "); err == nil {
			t.Fatal("the probe accepted no session, so the open must fail")
		}
		probe.Require(t, "padded DSN", connprobe.Handshake{User: "  fixture_user", Database: "fixture_db "})
	})
	t.Run("singlestore refuses a padded URL", func(t *testing.T) {
		probe := connprobe.MySQL(t)
		raw := "singlestore://fixture_user:" + paddedFixturePassword + "@" + probe.Addr + "/fixture_db"
		if _, err := singlestore.Open(ctx, "  "+raw); err == nil || !strings.Contains(err.Error(), "invalid SingleStore URL") {
			t.Fatalf("padded string reached past the parser: %v", err)
		}
	})
}

// A blank value must refuse and never reach PostgreSQL's parser, which reads an
// empty string as the driver's defaults and dials a local socket.
func TestOpenWithStatsRefusesBlankDatabaseURL(t *testing.T) {
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, name := range []string{"postgres", "singlestore"} {
		for _, value := range []string{"", "   ", " \n\t"} {
			st, stats, err := backend.OpenWithStats(ctx, config.Database{Backend: name, URL: value})
			if err == nil || st != nil || stats.Pool != nil || stats.Reads != nil {
				t.Fatalf("%s %q: backend %v err %v", name, value, st, err)
			}
			if !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL") || strings.Contains(err.Error(), "dial") {
				t.Fatalf("%s %q: error = %v", name, value, err)
			}
		}
	}
}
