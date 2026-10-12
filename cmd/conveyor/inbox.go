package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// conveyor inbox prints the operator inbox in server order
// (req-operator-next-action REQ-5, AC-5.2, AC-2.4; DEC-61). It never sorts,
// bands, or filters items itself.

type inboxTaskView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type inboxCompact struct {
	Label     string              `json:"label"`
	Actor     string              `json:"actor"`
	Kind      string              `json:"kind"`
	Judgment  bool                `json:"judgment"`
	Summary   string              `json:"summary"`
	Deadline  *time.Time          `json:"deadline"`
	BlockedBy []nextActionBlocker `json:"blocked_by,omitempty"`
}

// inboxProposalView is the identity of a pending document proposal item.
type inboxProposalView struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Tier    string `json:"tier"`
	Version int    `json:"version,omitempty"`
}

type inboxItemView struct {
	Kind       string              `json:"kind,omitempty"`
	Band       string              `json:"band"`
	Task       *inboxTaskView      `json:"task,omitempty"`
	NextAction *inboxCompact       `json:"next_action,omitempty"`
	Proposal   *inboxProposalView  `json:"proposal,omitempty"`
	Sentence   string              `json:"sentence,omitempty"`
	Blockers   []nextActionBlocker `json:"blockers,omitempty"`
	Deadline   *time.Time          `json:"deadline,omitempty"`
}

type inboxActView struct {
	ID      int64     `json:"id"`
	TaskID  string    `json:"task_id"`
	Kind    string    `json:"kind"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary,omitempty"`
}

type inboxPage struct {
	Items []inboxItemView `json:"items"`
	Acts  []inboxActView  `json:"acts"`
}

type inboxOptions struct {
	band          string
	limit, offset int
	asJSON        bool
}

func inboxCmd() *cobra.Command {
	var options inboxOptions
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "List tasks and proposals waiting on the operator, in server order",
		Long: `List the operator inbox from GET /v1/next-actions in server order under band
headings (decisions, blocked, automatic). Blocked items name each blocker.
The listing ends with the page count and the number of recent factory acts;
--band automatic also lists each act. --json prints the server body verbatim.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInbox(newClient(), cmd.OutOrStdout(), options)
		},
	}
	cmd.Flags().StringVar(&options.band, "band", "", "decisions, blocked, or automatic")
	cmd.Flags().IntVar(&options.limit, "limit", 0, "page size (server default when omitted)")
	cmd.Flags().IntVar(&options.offset, "offset", 0, "page offset")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "print the server body verbatim")
	return cmd
}

func runInbox(c *client, w io.Writer, options inboxOptions) error {
	query := url.Values{}
	if options.band != "" {
		query.Set("band", options.band)
	}
	if options.limit != 0 {
		query.Set("limit", strconv.Itoa(options.limit))
	}
	if options.offset != 0 {
		query.Set("offset", strconv.Itoa(options.offset))
	}
	path := "/v1/next-actions"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	body, header, err := c.getRawWithHeader(path)
	if err != nil {
		return err
	}
	if options.asJSON {
		_, err := fmt.Fprintf(w, "%s\n", bytes.TrimSpace(body))
		return err
	}
	var page inboxPage
	if err := json.Unmarshal(body, &page); err != nil {
		return fmt.Errorf("decode inbox: %w", err)
	}
	return writeInbox(w, page, header.Get("X-Conveyor-Total"), options.band == "automatic")
}

func writeInbox(w io.Writer, page inboxPage, total string, listActs bool) error {
	var b strings.Builder
	band := ""
	for i, item := range page.Items {
		if i == 0 || item.Band != band {
			band = item.Band
			fmt.Fprintf(&b, "%s\n", band)
		}
		switch {
		case item.Task != nil && item.NextAction != nil:
			fmt.Fprintf(&b, "  %s %s: %s: %s\n", item.Task.ID, item.Task.Title, item.NextAction.Label, item.NextAction.Summary)
		case item.Task != nil:
			fmt.Fprintf(&b, "  %s %s\n", item.Task.ID, item.Task.Title)
		case item.Proposal != nil:
			line := fmt.Sprintf("  %s proposal %s", item.Proposal.Tier, item.Proposal.ID)
			if item.Proposal.Version > 0 {
				line += fmt.Sprintf(" v%d", item.Proposal.Version)
			}
			if item.Proposal.Title != "" {
				line += ": " + item.Proposal.Title
			}
			fmt.Fprintln(&b, line)
			if item.Sentence != "" {
				fmt.Fprintf(&b, "    %s\n", item.Sentence)
			}
		default:
			fmt.Fprintf(&b, "  %s\n", item.Sentence)
		}
		if item.Deadline != nil {
			fmt.Fprintf(&b, "    deadline %s\n", item.Deadline.UTC().Format(time.RFC3339))
		}
		for _, blocker := range item.Blockers {
			fmt.Fprintf(&b, "    blocked by %s %s\n", blocker.TaskID, blocker.Title)
		}
	}
	if total == "" {
		total = "?"
	}
	fmt.Fprintf(&b, "showing %d of %s\n", len(page.Items), total)
	fmt.Fprintf(&b, "factory acts: %d\n", len(page.Acts))
	if listActs {
		for _, act := range page.Acts {
			line := fmt.Sprintf("  %s %s %s", act.At.UTC().Format(time.RFC3339), act.TaskID, act.Kind)
			if act.Summary != "" {
				line += ": " + act.Summary
			}
			fmt.Fprintln(&b, line)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
