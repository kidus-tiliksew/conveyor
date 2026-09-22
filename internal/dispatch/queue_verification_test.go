package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type publicationWorkerFixture struct {
	store.Backend
	mu         sync.Mutex
	delivery   core.VerificationDelivery
	translated []string
}

func (b *publicationWorkerFixture) RunVerificationDelivery(ctx context.Context, a queue.VerificationPublicationArgs, fn func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error) error {
	ws, _ := store.WorkspaceFromContext(ctx)
	if !a.ValidWorkspace(ws) {
		return store.ErrVerificationAccess
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.delivery
	view := current
	err := fn(&view, func(command string, next core.VerificationDelivery) error {
		if command == "check" {
			if current.Generation != b.delivery.Generation {
				return store.ErrVerificationConflict
			}
			return nil
		}
		d, err := store.AdvanceVerificationDelivery(current, command, next, time.Now().UTC())
		if err != nil {
			return err
		}
		current = d
		view = d
		return nil
	})
	if err == nil {
		b.delivery = current
	}
	return err
}
func (b *publicationWorkerFixture) TranslateVerificationPublication(ctx context.Context, p store.VerificationPublication) error {
	ws, _ := store.WorkspaceFromContext(ctx)
	b.translated = append(b.translated, ws+":"+p.ID)
	return nil
}

func TestVerificationPublicationWorkerReconciliation(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(t.Context(), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", GitHub: "org/repo"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{42}, 32))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	credential := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "fixture", ClientID: "client"}, PrivateKey: private}
	if _, err = st.StoreWorkspaceGitHubApp(ctx, "demo", credential); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, "demo", 41, 12, "org"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"}}`)
		case "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)})
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[{"full_name":"org/repo"}]}`)
		default:
			t.Errorf("unexpected App request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	for _, scenario := range []string{"success", "lost_response", "already_written", "readback_mismatch", "head_change", "auth_failure", "exhaustion"} {
		t.Run(scenario, func(t *testing.T) {
			summary := "### Durable verification\nallowlisted metadata"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(summary)))
			b := &publicationWorkerFixture{Backend: st, delivery: core.VerificationDelivery{WorkspaceID: "demo", Repository: "org/repo", TaskID: "task", ContextID: "context", SourcePublicationID: "source", ID: "delivery", Generation: 1, State: "pending", PullRequestNumber: 42, TargetHead: strings.Repeat("a", 40), TargetDigest: digest, Summary: summary}}
			d := New(b, cfg, nil)
			d.GitHubApps = github.NewAppClient(server.Client(), server.URL)
			if scenario == "auth_failure" {
				d.WorkspaceGitHubApps = nil
			}
			worker := &verificationPublicationWorker{dispatcher: d}
			body := "agent Markdown\n<!-- conveyor:verification-evidence -->\nlegacy"
			if scenario == "already_written" {
				body = github.ComposeVerificationBody(body, "demo", "task", summary)
			}
			writes, reads := 0, 0
			d.ReadVerificationPR = func(context.Context, string, int) (github.VerificationPullRequest, error) {
				reads++
				pr := github.VerificationPullRequest{Number: 42, Body: body}
				pr.Head.SHA = strings.Repeat("a", 40)
				if scenario == "head_change" && reads > 1 {
					pr.Head.SHA = strings.Repeat("b", 40)
				}
				return pr, nil
			}
			d.WriteVerificationPR = func(_ context.Context, _ string, _ int, value string) error {
				writes++
				if !strings.Contains(value, "agent Markdown") || !strings.Contains(value, "legacy") {
					t.Fatal("body content erased")
				}
				if scenario != "readback_mismatch" && scenario != "exhaustion" {
					body = value
				}
				if scenario == "lost_response" {
					return errors.New("private response credentials")
				}
				return nil
			}
			args := queue.VerificationPublicationArgs{WorkspaceID: "demo", Repository: "org/repo", PullRequestNumber: 42}
			raw, _ := json.Marshal(args)
			job := queue.Job{WorkspaceID: "demo", ID: "opaque-stream", Args: raw, Attempt: 1, MaxAttempts: 5}
			calls := 1
			if scenario == "exhaustion" {
				calls = 5
			}
			for i := 0; i < calls; i++ {
				job.Attempt = i + 1
				err = worker.Work(ctx, job)
			}
			expected := "published"
			switch scenario {
			case "readback_mismatch", "head_change", "auth_failure":
				expected = "retrying"
			case "exhaustion":
				expected = "failed"
			}
			if b.delivery.State != expected {
				t.Fatalf("state=%s err=%v", b.delivery.State, err)
			}
			if expected == "published" && err != nil {
				t.Fatal(err)
			}
			if scenario == "already_written" && writes != 0 {
				t.Fatal("lost response caused another write")
			}
			if scenario == "lost_response" && (writes != 1 || reads != 2) {
				t.Fatal("lost response not reconciled")
			}
			if strings.Contains(b.delivery.ErrorMessage, "private") {
				t.Fatal("provider error retained")
			}
			if expected == "published" {
				if err = worker.Work(ctx, job); err != nil {
					t.Fatal(err)
				}
				if writes > 1 {
					t.Fatal("published generation written twice")
				}
			}
		})
	}
}

func TestVerificationPublicationWorkspaceEnvelope(t *testing.T) {
	b := &publicationWorkerFixture{}
	w := &verificationPublicationWorker{dispatcher: &Dispatcher{Store: b}}
	p := store.VerificationPublication{ID: "same-source", TaskID: "same-task"}
	raw, _ := json.Marshal(p)
	for _, ws := range []string{"alpha", "beta"} {
		if err := w.Legacy(t.Context(), queue.Job{WorkspaceID: ws, ID: "same-stream", Args: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(b.translated, ",") != "alpha:same-source,beta:same-source" {
		t.Fatal("legacy identities crossed partitions")
	}
	for _, payload := range []string{string(raw), `{"ID":"same-source","TaskID":"same-task","workspace_id":"beta"}`, `{"ID":"same-source","TaskID":"same-task","WorkspaceID":"beta"}`} {
		ws := "alpha"
		if payload == string(raw) {
			ws = ""
		}
		if err := w.Legacy(t.Context(), queue.Job{WorkspaceID: ws, Args: []byte(payload)}); err == nil {
			t.Fatal("invalid legacy envelope accepted")
		}
	}
	for _, args := range []queue.VerificationPublicationArgs{{Repository: "org/repo", PullRequestNumber: 42}, {WorkspaceID: "beta", Repository: "org/repo", PullRequestNumber: 42}} {
		raw, _ := json.Marshal(args)
		if err := w.Work(t.Context(), queue.Job{WorkspaceID: "alpha", Args: raw}); err == nil {
			t.Fatal("typed workspace accepted")
		}
	}
	if len(b.translated) != 2 {
		t.Fatal("refused adapter accessed store")
	}
}
