package workorder

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// UploadVerificationEvidence attaches evidence only through the exact live
// worker claim. The store derives task and workspace ownership atomically with
// artifact creation; callers cannot supply either value or select a role.
func (s *Service) UploadVerificationEvidence(ctx context.Context, id, workerID, session, clientToken, name, contentType string, content []byte) (core.Artifact, error) {
	return s.Store.CreateClaimedVerificationEvidence(ctx, store.ClaimedVerificationEvidenceRequest{
		WorkOrderID: id, WorkerID: workerID, SessionID: session, ClientToken: clientToken,
		Name: name, ContentType: contentType,
	}, content)
}
