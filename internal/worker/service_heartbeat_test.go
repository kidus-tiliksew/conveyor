package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type heartbeatSecretSource struct {
	secrets []string
	err     error
}

func (s heartbeatSecretSource) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.secrets, s.err
}

// heartbeatFixture enrolls one worker against a server whose deployment
// configuration names no harness at all. The ConfigProvider fails the test if
// Heartbeat consults it (DEC-56; req-worker REQ-1).
func heartbeatFixture(t *testing.T, now time.Time) (*Service, store.Store, context.Context, core.Worker) {
	t.Helper()
	st := store.NewMemory()
	service := &Service{Store: st, Now: func() time.Time { return now }, ConfigProvider: func(context.Context) (*config.Config, error) {
		t.Error("heartbeat consulted the server configuration")
		return nil, errors.New("server configuration must not be read by heartbeat")
	}}
	operatorCtx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "operator-token", OwnerUserID: "usr-heartbeat", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithWorkspace(store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID("usr-heartbeat"), Role: core.ActorUser}), "demo")
	token, _, err := service.IssuePairing(operatorCtx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := service.Enroll(t.Context(), token, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, worker, err := service.Authenticate(t.Context(), enrollment.Credential, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return service, st, workerCtx, worker
}

func storedProbes(t *testing.T, st store.Store, ctx context.Context, id string) []core.HarnessProbe {
	t.Helper()
	workers, err := st.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if worker.ID == id {
			return worker.Probes
		}
	}
	t.Fatalf("worker %s not listed", id)
	return nil
}

func TestHeartbeatAcceptsClientLocalHarnessProbesWithoutServerRegistry(t *testing.T) {
	now := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	service, st, ctx, worker := heartbeatFixture(t, now)
	checked := now.Add(-time.Minute)
	// The worker sends HarnessFingerprint of its own local definition; the
	// server accepts it without knowing that definition.
	fingerprint := HarnessFingerprint(config.Harness{Name: "local-only-agent", Command: []string{"local-only-agent", "{prompt}", "{mcp_config}"}, ProbeCommand: []string{"local-only-agent", "--version"}, ProbeTimeoutText: "5s"})
	if !validHarnessFingerprint(fingerprint) {
		t.Fatalf("worker fingerprint %q fails the server shape rule", fingerprint)
	}
	probes := []core.HarnessProbe{
		{Harness: "local-only-agent", Fingerprint: fingerprint, Healthy: true, CheckedAt: checked},
		{Harness: "another-local-agent", Healthy: false, Message: "probe exited 1", Transition: "healthy_to_unhealthy"},
	}
	updated, err := service.Heartbeat(ctx, worker, probes)
	if err != nil {
		t.Fatalf("client-local probes refused: %v", err)
	}
	if !updated.LeaseExpiresAt.Equal(now.Add(DefaultLivenessLease)) || !updated.Live(now) {
		t.Fatalf("liveness lease=%s want %s", updated.LeaseExpiresAt, now.Add(DefaultLivenessLease))
	}
	got := storedProbes(t, st, ctx, worker.ID)
	if len(got) != 2 {
		t.Fatalf("stored probes=%+v", got)
	}
	if got[0].Harness != "local-only-agent" || got[0].Fingerprint != fingerprint || !got[0].Healthy || !got[0].CheckedAt.Equal(checked) {
		t.Fatalf("healthy probe=%+v", got[0])
	}
	if got[1].Harness != "another-local-agent" || got[1].Healthy || got[1].Message != "probe exited 1" || got[1].Transition != "healthy_to_unhealthy" || !got[1].CheckedAt.Equal(now) {
		t.Fatalf("unhealthy probe=%+v (zero checked_at must normalize to now)", got[1])
	}
	// A legacy report without optional fields and an empty report both renew liveness.
	if _, err = service.Heartbeat(ctx, worker, []core.HarnessProbe{{Harness: "codex", Healthy: true}}); err != nil {
		t.Fatalf("legacy probe refused: %v", err)
	}
	if _, err = service.Heartbeat(ctx, worker, nil); err != nil {
		t.Fatalf("empty heartbeat refused: %v", err)
	}
}

func TestHeartbeatBoundsProbeReportShapeWithoutEchoingValues(t *testing.T) {
	now := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	secretName := "secret-harness-" + strings.Repeat("z", MaxHarnessProbeNameBytes)
	many := func(n int) []core.HarnessProbe {
		probes := make([]core.HarnessProbe, n)
		for i := range probes {
			probes[i] = core.HarnessProbe{Harness: fmt.Sprintf("agent-%02d", i), Healthy: true}
		}
		return probes
	}
	for _, tc := range []struct {
		name   string
		probes []core.HarnessProbe
		ok     bool
	}{
		{name: "maximum count", probes: many(MaxHarnessProbes), ok: true},
		{name: "count over limit", probes: many(MaxHarnessProbes + 1)},
		{name: "maximum name", probes: []core.HarnessProbe{{Harness: strings.Repeat("n", MaxHarnessProbeNameBytes)}}, ok: true},
		{name: "name over limit", probes: []core.HarnessProbe{{Harness: secretName}}},
		{name: "blank name", probes: []core.HarnessProbe{{Harness: " \t "}}},
		{name: "padded name", probes: []core.HarnessProbe{{Harness: " codex"}}},
		{name: "control character", probes: []core.HarnessProbe{{Harness: "co\ndex"}}},
		{name: "invalid utf8", probes: []core.HarnessProbe{{Harness: "co\xffdex"}}},
		{name: "non-printable format character", probes: []core.HarnessProbe{{Harness: "co\u200bdex"}}},
		{name: "interior space", probes: []core.HarnessProbe{{Harness: "local agent"}}, ok: true},
		{name: "short fingerprint", probes: []core.HarnessProbe{{Harness: "codex", Fingerprint: "abc123"}}},
		{name: "uppercase fingerprint", probes: []core.HarnessProbe{{Harness: "codex", Fingerprint: strings.Repeat("AB", 32)}}},
		{name: "unknown transition", probes: []core.HarnessProbe{{Harness: "codex", Transition: "sideways"}}},
		{name: "duplicate report", probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}, {Harness: "codex", Healthy: false}}},
		{name: "same harness distinct fingerprints", probes: []core.HarnessProbe{{Harness: "codex", Fingerprint: strings.Repeat("a", 64)}, {Harness: "codex", Fingerprint: strings.Repeat("b", 64)}}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, st, ctx, worker := heartbeatFixture(t, now)
			before := storedProbes(t, st, ctx, worker.ID)
			_, err := service.Heartbeat(ctx, worker, tc.probes)
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := storedProbes(t, st, ctx, worker.ID); len(got) != len(tc.probes) {
					t.Fatalf("stored %d of %d probes", len(got), len(tc.probes))
				}
				return
			}
			if !errors.Is(err, ErrInvalidHarnessProbe) {
				t.Fatalf("err=%v, want ErrInvalidHarnessProbe", err)
			}
			if strings.Contains(err.Error(), "secret-harness") || strings.Contains(err.Error(), "sideways") || strings.Contains(err.Error(), "abc123") {
				t.Fatalf("validation error echoed a field value: %v", err)
			}
			if got := storedProbes(t, st, ctx, worker.ID); len(got) != len(before) {
				t.Fatalf("refused heartbeat stored probes=%+v", got)
			}
		})
	}
}

func TestHeartbeatRedactsAndBoundsProbeMessagesBeforePersistence(t *testing.T) {
	now := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	service, st, ctx, worker := heartbeatFixture(t, now)
	exact := "stored-app-key-value-0123456789"
	service.RedactionSecrets = heartbeatSecretSource{secrets: []string{exact}}
	pattern := "ghp_" + strings.Repeat("A1b2", 6)
	long := strings.Repeat("x", MaxHarnessProbeMessageBytes-1) + "é" + strings.Repeat("y", 100)
	_, err := service.Heartbeat(ctx, worker, []core.HarnessProbe{
		{Harness: "pattern", Message: "auth failed with " + pattern},
		{Harness: "exact", Message: "key " + exact + " rejected"},
		{Harness: "long", Message: long},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := storedProbes(t, st, ctx, worker.ID)
	if len(got) != 3 {
		t.Fatalf("stored=%+v", got)
	}
	for _, probe := range got {
		if strings.Contains(probe.Message, pattern) || strings.Contains(probe.Message, exact) {
			t.Fatalf("secret persisted in probe %q: %q", probe.Harness, probe.Message)
		}
		if len(probe.Message) > MaxHarnessProbeMessageBytes {
			t.Fatalf("message bound exceeded: %d bytes", len(probe.Message))
		}
	}
	if !strings.Contains(got[0].Message, "[REDACTED:") || !strings.Contains(got[1].Message, "[REDACTED:") {
		t.Fatalf("redaction placeholders missing: %q / %q", got[0].Message, got[1].Message)
	}
	if got[2].Message != strings.Repeat("x", MaxHarnessProbeMessageBytes-1) {
		t.Fatalf("long message not cut at a rune boundary: %d bytes", len(got[2].Message))
	}

	// Redaction that cannot complete fails closed and persists nothing.
	service.RedactionSecrets = heartbeatSecretSource{err: errors.New("key store unavailable")}
	if _, err = service.Heartbeat(ctx, worker, []core.HarnessProbe{{Harness: "pattern", Message: "new message"}}); !errors.Is(err, ErrHarnessProbeRedaction) {
		t.Fatalf("err=%v, want ErrHarnessProbeRedaction", err)
	}
	if after := storedProbes(t, st, ctx, worker.ID); len(after) != 3 || after[0].Message != got[0].Message {
		t.Fatalf("failed redaction changed stored probes: %+v", after)
	}
	// Reports without messages need no secret source.
	if _, err = service.Heartbeat(ctx, worker, []core.HarnessProbe{{Harness: "pattern", Healthy: true}}); err != nil {
		t.Fatalf("message-free heartbeat refused: %v", err)
	}
}

func TestHeartbeatKeepsRevokedWorkerRefused(t *testing.T) {
	now := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	service, st, ctx, worker := heartbeatFixture(t, now)
	if err := st.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Heartbeat(ctx, worker, []core.HarnessProbe{{Harness: "local-only-agent", Healthy: true}}); !errors.Is(err, store.ErrWorkerUnauthorized) {
		t.Fatalf("revoked worker heartbeat err=%v", err)
	}
}
