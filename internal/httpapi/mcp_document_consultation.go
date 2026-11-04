package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// mcpConsultationTimeout bounds the ledger write, so a stalled ledger delays a
// read by at most this long and never fails it.
const mcpConsultationTimeout = 5 * time.Second

// recordDocumentConsultation records that a session read System Design text. A
// caller runs it only after a page has passed the output budget, so a refused,
// failed, or oversized read records nothing. The page itself decides what it
// delivered: an outline or an authority-absent read carries no body text.
// Listings and searches never reach it (req-design-delivery AC-5.1, AC-5.2;
// DEC-103).
//
// Attribution comes from the caller's own live claim, checked on every call and
// never trusted from the arguments. Arguments naming anything else leave a user
// credential's read unattributed and unrecorded. Consultation is observational:
// a ledger failure is logged, leaves the response unchanged, and a later read
// of the same version records it, because nothing is remembered in process.
func (s *Server) recordDocumentConsultation(ctx context.Context, credential core.AuthenticatedCredential, workspace string, args map[string]any, page mcpReadPage) {
	workOrderID, sessionID := readString(args, "work_order_id"), readString(args, "session_id")
	if readString(args, "kind") != "system_design" || workOrderID == "" || sessionID == "" || len(page.Items) != 1 {
		return
	}
	var served struct {
		Version int    `json:"version"`
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(page.Items[0], &served); err != nil || served.Version == 0 || (served.Content == "" && served.Text == "") {
		return
	}
	documentID := readString(args, "document_id")
	ctx, cancel := context.WithTimeout(ctx, mcpConsultationTimeout)
	defer cancel()
	err := s.authorizeDocumentReadClaim(ctx, "get_document", credential, workspace, args)
	if err == nil {
		err = s.Store.RecordSystemDesignConsulted(ctx, documentID, served.Version, sessionID, workOrderID)
	} else if !errors.Is(err, store.ErrBackendOperation) {
		return
	}
	if err != nil {
		logged, _ := redact.New(nil).Redact(err.Error())
		log.Printf("mcp: record system design consultation failed: workspace=%q work_order=%q document=%q version=%d error=%q", workspace, workOrderID, documentID, served.Version, logged)
	}
}
