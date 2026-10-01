package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// verificationPermissionRefusal is a server refusal of an operator grant act.
// Reason and Recovery are present only for an authorized operator
// (component-http-api VK-HTTP-8).
type verificationPermissionRefusal struct {
	Status                    int
	Reason, Message, Recovery string
}

func (e *verificationPermissionRefusal) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("refused (%d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("refused: %s: %s\nrecovery: %s", e.Reason, e.Message, e.Recovery)
}

// verificationPermissions calls the user-only grant routes with the explicit
// workspace. It never sends a launcher client token or session identifier.
func (c *client) verificationPermissions(method, orderID string, query url.Values, body []byte, out any) error {
	if c.configErr != nil {
		return c.configErr
	}
	if strings.TrimSpace(c.workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	if strings.TrimSpace(orderID) == "" {
		return fmt.Errorf("work-order ID is required")
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("workspace_id", c.workspace)
	req, err := http.NewRequest(method, c.base+"/v1/work-orders/"+url.PathEscape(orderID)+"/verification/permissions?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("X-Workspace-ID", c.workspace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s (is conveyord running? set CONVEYOR_ADDR if not on :8080)", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		refusal := &verificationPermissionRefusal{Status: resp.StatusCode, Message: string(bytes.TrimSpace(data))}
		var encoded struct{ Error, Reason, Recovery string }
		if json.Unmarshal(data, &encoded) == nil && encoded.Reason != "" {
			refusal.Reason, refusal.Message, refusal.Recovery = encoded.Reason, encoded.Error, encoded.Recovery
		} else if resp.StatusCode == http.StatusUnauthorized {
			refusal.Message += " (" + c.credentialDiagnostic() + ")"
		} else if resp.StatusCode == http.StatusNotFound {
			refusal.Message += "; the work order is not visible to this operator user credential in workspace " + c.workspace
		}
		return refusal
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// verificationPermissionView reads every page of one context projection and
// returns it with next_cursor cleared.
func (c *client) verificationPermissionView(orderID, contextID string) (store.VerificationPermissionView, error) {
	var view store.VerificationPermissionView
	cursor := ""
	for page := 0; page < 400; page++ {
		query := url.Values{"limit": {"50"}}
		if contextID != "" {
			query.Set("context_id", contextID)
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var next store.VerificationPermissionView
		if err := c.verificationPermissions(http.MethodGet, orderID, query, nil, &next); err != nil {
			return view, err
		}
		if page == 0 {
			view = next
			if next.Context != nil {
				contextID = next.Context.ID
			}
		} else {
			view.Subjects = append(view.Subjects, next.Subjects...)
			view.Grants = append(view.Grants, next.Grants...)
		}
		if next.NextCursor == "" {
			view.NextCursor = ""
			return view, nil
		}
		cursor = next.NextCursor
	}
	return view, fmt.Errorf("projection exceeded the page bound")
}

// verificationPermissionReceipt reads one grant receipt back by ID.
func (c *client) verificationPermissionReceipt(orderID, contextID, grantID string) (store.VerificationPermissionGrantView, error) {
	var view store.VerificationPermissionView
	query := url.Values{"grant_id": {grantID}}
	if contextID != "" {
		query.Set("context_id", contextID)
	}
	if err := c.verificationPermissions(http.MethodGet, orderID, query, nil, &view); err != nil {
		return store.VerificationPermissionGrantView{}, err
	}
	if len(view.Grants) != 1 || view.Grants[0].ID != grantID {
		return store.VerificationPermissionGrantView{}, fmt.Errorf("receipt read-back did not return grant %s", grantID)
	}
	return view.Grants[0], nil
}
