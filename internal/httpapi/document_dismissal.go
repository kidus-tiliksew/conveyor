package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// decodeDocumentDismissalNote is shared by both tiers' confirm and dismiss
// routes. Authorization remains on their existing confirm_documents middleware.
func decodeDocumentDismissalNote(r *http.Request) (*http.Request, error) {
	var body struct {
		Note string `json:"note"`
	}
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("invalid dismissal note: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("dismissal note body must contain one JSON object")
		}
	}
	ctx, err := store.WithDocumentDismissalNote(r.Context(), body.Note)
	if err != nil {
		return nil, err
	}
	return r.WithContext(ctx), nil
}
