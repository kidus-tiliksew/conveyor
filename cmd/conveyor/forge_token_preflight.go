package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// preflightForgeToken is metadata-only: the token value never leaves the
// server, and the claim transaction remains authoritative for deletion races.
func (c *client) fetchForgeTokenPreflight(ctx context.Context, credential string) error {
	var status core.ForgeTokenStatus
	err := c.workerDoContext(ctx, http.MethodGet, "/v1/forge-token", nil, &status, credential)
	if err == nil && status.Configured {
		return nil
	}
	var response *workerHTTPError
	if err == nil || errors.As(err, &response) && response.StatusCode == http.StatusNotFound {
		return store.ErrForgeTokenRequired
	}
	return err
}

func (c *client) preflightForgeToken(ctx context.Context, credential string) error {
	if c.forgeTokenPreflight == nil {
		return nil
	}
	return c.forgeTokenPreflight(ctx, credential)
}
