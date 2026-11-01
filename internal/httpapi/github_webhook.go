package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// The GitHub webhook receiver (component-http-api, GitHub webhook receiver;
// feature-quiet-idle-runtime; req-quiet-idle-runtime AC-3.1 to AC-3.3;
// DEC-99(5)). GitHub authenticates each delivery with an HMAC-SHA256
// signature over the raw body under the workspace's webhook secret, so the
// route reads no credential. The secret never reaches a log line, event, or
// response.

// GitHubWebhookApplier applies an accepted delivery asynchronously.
type GitHubWebhookApplier interface {
	Submit(github.WebhookDelivery)
}

// maxGitHubWebhookBody is GitHub's delivery size cap.
const maxGitHubWebhookBody = 25 << 20

// GitHubWebhookActor is the system actor deliveries are recorded and
// applied under.
const GitHubWebhookActor = "github-webhook"

func (s *Server) receiveGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	workspace := strings.TrimSpace(chi.URLParam(r, "workspace"))
	if s.GitHubWebhooks == nil || s.GitHubWebhookApplier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "github_webhook_unavailable"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGitHubWebhookBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if len(body) > maxGitHubWebhookBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body_too_large"})
		return
	}
	ctx := store.WithWorkspace(store.WithActor(r.Context(), store.SystemActor(GitHubWebhookActor)), workspace)
	secret := ""
	if workspace != "" {
		secret, err = s.GitHubWebhooks.GetWorkspaceGitHubWebhookSecretForUse(ctx, workspace)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("GitHub webhook: workspace=%s read webhook secret: %v", workspace, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
			return
		}
	}
	// A missing workspace or secret answers like a wrong signature, so the
	// route is no workspace-existence oracle.
	if secret == "" || !validGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_signature"})
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || event == "" || len(deliveryID) > 255 || len(event) > 255 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "delivery_headers_required"})
		return
	}
	received := time.Now().UTC()
	accepted, err := s.GitHubWebhooks.RecordGitHubWebhookDelivery(ctx, workspace, deliveryID, event, received)
	if err != nil {
		log.Printf("GitHub webhook: workspace=%s delivery=%s event=%s record: %v", workspace, deliveryID, event, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if !accepted {
		// A redelivery of a recorded delivery applies nothing (AC-3.3).
		log.Printf("GitHub webhook: workspace=%s delivery=%s event=%s duplicate", workspace, deliveryID, event)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	s.GitHubWebhookApplier.Submit(github.WebhookDelivery{Workspace: workspace, ID: deliveryID, Event: event, Body: body, ReceivedAt: received})
	log.Printf("GitHub webhook: workspace=%s delivery=%s event=%s accepted", workspace, deliveryID, event)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// validGitHubSignature checks X-Hub-Signature-256, "sha256=" and the hex
// HMAC-SHA256 of body, in constant time.
func validGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
