package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// lineageShowOptions and lineageImpactOptions carry the flags of the
// read-only lineage commands; both read through GET /v1/lineage
// (component-lineage; req-260802-72fc68 REQ-5).
type lineageShowOptions struct {
	direction                    string
	kinds, nodeTypes             []string
	maxDepth, maxNodes, maxLinks int
	asJSON                       bool
}

type lineageImpactOptions struct {
	outdated      bool
	limit, offset int
	asJSON        bool
}

func lineageShowCmd() *cobra.Command {
	var options lineageShowOptions
	command := &cobra.Command{
		Use:   "show <type> <id>",
		Short: "Walk lineage from one node with depth, path, and status",
		Long:  "Walk the read-only lineage graph from one node under the operator read budget. Each reached node prints with its depth, type, ID, label, and status; --json prints the server response, which adds each node's edge_path. --direction, --kind, and --node-type narrow the walk; a refused link still counts against its node's fan-out.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if strings.TrimSpace(client.workspace) == "" {
				return fmt.Errorf("--workspace is required")
			}
			return runLineageShow(client, cmd.OutOrStdout(), args[0], args[1], options)
		},
	}
	command.Flags().StringVar(&options.direction, "direction", "", "follow links out of, into, or both ways through each node (out, in, both)")
	command.Flags().StringSliceVar(&options.kinds, "kind", nil, "follow only these edge kinds (repeatable or comma-separated)")
	command.Flags().StringSliceVar(&options.nodeTypes, "node-type", nil, "admit only these node types (repeatable or comma-separated)")
	command.Flags().IntVar(&options.maxDepth, "max-depth", 0, "edge depth cap; defaults to the context budget")
	command.Flags().IntVar(&options.maxNodes, "max-nodes", 0, "node cap including the root; defaults to the context budget")
	command.Flags().IntVar(&options.maxLinks, "max-links", 0, "link cap; defaults to the context budget")
	command.Flags().BoolVar(&options.asJSON, "json", false, "print the server JSON response")
	return command
}

func lineageImpactCmd() *cobra.Command {
	var options lineageImpactOptions
	command := &cobra.Command{
		Use:   "impact <requirement|system_design> <id>",
		Short: "List the tasks each document version served or governed",
		Long:  "List, for a requirement or System Design, every task a version served or governed, with the version's status, the task's state, and its merged commit ranges. --outdated keeps only merged tasks built on a superseded version. The read is bounded to 1000 version-to-task links and pages them.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			if strings.TrimSpace(client.workspace) == "" {
				return fmt.Errorf("--workspace is required")
			}
			return runLineageImpact(client, cmd.OutOrStdout(), args[0], args[1], options)
		},
	}
	command.Flags().BoolVar(&options.outdated, "outdated", false, "only merged tasks built on a superseded version")
	command.Flags().IntVar(&options.limit, "limit", 0, "entries per page (1-100, default 25)")
	command.Flags().IntVar(&options.offset, "offset", 0, "entries to skip (0-1000)")
	command.Flags().BoolVar(&options.asJSON, "json", false, "print the server JSON response")
	return command
}

func lineagePath(nodeType, id, suffix string, query url.Values) string {
	path := "/v1/lineage/" + url.PathEscape(nodeType) + "/" + url.PathEscape(id) + suffix
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

type lineageShowNode struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Depth  int    `json:"depth"`
	Status *struct {
		State          string `json:"state"`
		VersionStatus  string `json:"version_status"`
		CurrentVersion int    `json:"current_version"`
		Archived       bool   `json:"archived"`
		Outcome        string `json:"outcome"`
		ReviewRound    int    `json:"review_round"`
		ReviewSeat     int    `json:"review_seat"`
	} `json:"status"`
}

type lineageShowGraph struct {
	Nodes             []lineageShowNode `json:"nodes"`
	Links             []json.RawMessage `json:"links"`
	Truncated         bool              `json:"truncated"`
	ExhaustionReasons []string          `json:"exhaustion_reasons"`
	OmittedNodes      int               `json:"omitted_nodes"`
	OmittedLinks      int               `json:"omitted_links"`
}

func runLineageShow(c *client, w io.Writer, nodeType, id string, options lineageShowOptions) error {
	query := url.Values{"include": {"depth,path,status"}}
	if options.direction != "" {
		query.Set("direction", options.direction)
	}
	if len(options.kinds) > 0 {
		query.Set("kind", strings.Join(options.kinds, ","))
	}
	if len(options.nodeTypes) > 0 {
		query.Set("node_type", strings.Join(options.nodeTypes, ","))
	}
	for name, value := range map[string]int{"max_depth": options.maxDepth, "max_nodes": options.maxNodes, "max_links": options.maxLinks} {
		if value != 0 {
			query.Set(name, strconv.Itoa(value))
		}
	}
	body, _, err := c.getRawWithHeader(lineagePath(nodeType, id, "", query))
	if err != nil {
		return err
	}
	if options.asJSON {
		_, err = fmt.Fprintf(w, "%s\n", bytes.TrimSpace(body))
		return err
	}
	var graph lineageShowGraph
	if err = json.Unmarshal(body, &graph); err != nil {
		return fmt.Errorf("decode lineage: %w", err)
	}
	var b strings.Builder
	for _, node := range graph.Nodes {
		fmt.Fprintf(&b, "%s%d %s %s", strings.Repeat("  ", node.Depth), node.Depth, node.Type, node.ID)
		if node.Label != "" {
			fmt.Fprintf(&b, " %q", node.Label)
		}
		if status := lineageStatusText(node); status != "" {
			fmt.Fprintf(&b, " [%s]", status)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%d nodes, %d links", len(graph.Nodes), len(graph.Links))
	if graph.Truncated {
		fmt.Fprintf(&b, "; truncated by %s (%d nodes and %d links omitted)", strings.Join(graph.ExhaustionReasons, ", "), graph.OmittedNodes, graph.OmittedLinks)
	}
	b.WriteString("\n")
	_, err = io.WriteString(w, b.String())
	return err
}

func lineageStatusText(node lineageShowNode) string {
	if node.Status == nil {
		return ""
	}
	status := node.Status
	parts := []string{}
	if status.State != "" {
		parts = append(parts, status.State)
	}
	if status.VersionStatus != "" {
		parts = append(parts, fmt.Sprintf("%s, current v%d", status.VersionStatus, status.CurrentVersion))
	} else if status.CurrentVersion > 0 {
		parts = append(parts, fmt.Sprintf("current v%d", status.CurrentVersion))
	}
	if status.Archived {
		parts = append(parts, "archived")
	}
	if status.Outcome != "" {
		parts = append(parts, fmt.Sprintf("%s, round %d seat %d", status.Outcome, status.ReviewRound, status.ReviewSeat))
	}
	return strings.Join(parts, "; ")
}

type lineageImpactPage struct {
	CurrentVersion int  `json:"current_version"`
	Archived       bool `json:"archived"`
	Items          []struct {
		Version       int      `json:"version"`
		VersionStatus string   `json:"version_status"`
		Relation      string   `json:"relation"`
		TaskState     string   `json:"task_state"`
		MergedRanges  []string `json:"merged_ranges"`
		Task          struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"task"`
	} `json:"items"`
	Total             int      `json:"total"`
	Offset            int      `json:"offset"`
	NextOffset        *int     `json:"next_offset"`
	Truncated         bool     `json:"truncated"`
	ExhaustionReasons []string `json:"exhaustion_reasons"`
}

func runLineageImpact(c *client, w io.Writer, nodeType, id string, options lineageImpactOptions) error {
	query := url.Values{}
	if options.outdated {
		query.Set("outdated", "true")
	}
	if options.limit != 0 {
		query.Set("limit", strconv.Itoa(options.limit))
	}
	if options.offset != 0 {
		query.Set("offset", strconv.Itoa(options.offset))
	}
	body, _, err := c.getRawWithHeader(lineagePath(nodeType, id, "/impact", query))
	if err != nil {
		return err
	}
	if options.asJSON {
		_, err = fmt.Fprintf(w, "%s\n", bytes.TrimSpace(body))
		return err
	}
	var page lineageImpactPage
	if err = json.Unmarshal(body, &page); err != nil {
		return fmt.Errorf("decode lineage impact: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s current v%d", nodeType, id, page.CurrentVersion)
	if page.Archived {
		b.WriteString(" (archived)")
	}
	b.WriteString("\n")
	for _, item := range page.Items {
		version := "v?"
		if item.Version > 0 {
			version = fmt.Sprintf("v%d", item.Version)
		}
		fmt.Fprintf(&b, "  %s %s %s %s %q %s", version, item.VersionStatus, item.Relation, item.Task.ID, item.Task.Label, item.TaskState)
		if len(item.MergedRanges) > 0 {
			fmt.Fprintf(&b, " %s", strings.Join(item.MergedRanges, " "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%d of %d entries from offset %d", len(page.Items), page.Total, page.Offset)
	if page.NextOffset != nil {
		fmt.Fprintf(&b, "; next --offset %d", *page.NextOffset)
	}
	if page.Truncated {
		fmt.Fprintf(&b, "; truncated by %s", strings.Join(page.ExhaustionReasons, ", "))
	}
	b.WriteString("\n")
	_, err = io.WriteString(w, b.String())
	return err
}
