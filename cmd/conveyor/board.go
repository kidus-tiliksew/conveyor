package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/spf13/cobra"
)

type boardOptions struct {
	limit, offset int
	asJSON        bool
}

type boardPage struct {
	Items  []store.BoardItem `json:"items"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func boardCmd() *cobra.Command {
	var options boardOptions
	cmd := &cobra.Command{Use: "board", Short: "List open tasks and their next actions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runBoard(newClient(), cmd.OutOrStdout(), options) },
	}
	cmd.Flags().IntVar(&options.limit, "limit", 0, "page size (server default when omitted)")
	cmd.Flags().IntVar(&options.offset, "offset", 0, "page offset (requires --limit)")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "print the server body verbatim")
	return cmd
}

func runBoard(c *client, w io.Writer, options boardOptions) error {
	query := url.Values{}
	if options.limit != 0 {
		query.Set("limit", strconv.Itoa(options.limit))
	}
	if options.offset != 0 {
		query.Set("offset", strconv.Itoa(options.offset))
	}
	path := "/v1/board"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	body, _, err := c.getRawWithHeader(path)
	if err != nil {
		return err
	}
	if options.asJSON {
		_, err = fmt.Fprintf(w, "%s\n", bytes.TrimSpace(body))
		return err
	}
	var page boardPage
	if err := json.Unmarshal(body, &page); err != nil {
		return fmt.Errorf("decode board: %w", err)
	}
	for _, item := range page.Items {
		if _, err := fmt.Fprintf(w, "%s  %s  %s  %s\n", item.ID, item.State, item.Title, item.NextAction.Label); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "%d shown, %d total (offset %d)\n", len(page.Items), page.Total, page.Offset)
	return err
}
