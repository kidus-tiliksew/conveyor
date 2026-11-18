package main

// `conveyor crate` commands (component-crates invariant 15; req-crates
// AC-6.3). Each command calls the one route that holds its rule and adds no
// rule of its own. The server validates every field, state, and membership
// act. --json prints the route's response body as the server sent it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// crateRow is the crate row of component-crates Public surfaces as the
// commands print it.
type crateRow struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	TargetDate      *string  `json:"target_date"`
	State           string   `json:"state"`
	Shipped         bool     `json:"shipped"`
	OpenTaskCount   int      `json:"open_task_count"`
	MergedTaskCount int      `json:"merged_task_count"`
	TotalTaskCount  int      `json:"total_task_count"`
	RequirementIDs  []string `json:"requirement_ids"`
}

// crateDetail is the crate read of GET /v1/crates/{id}.
type crateDetail struct {
	crateRow
	Requirements []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		TaskCount int    `json:"task_count"`
	} `json:"requirements"`
	UndeclaredRequirements []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		TaskCount int    `json:"task_count"`
	} `json:"undeclared_requirements"`
	Tasks []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		State      string `json:"state"`
		NextAction struct {
			Label string `json:"label"`
		} `json:"next_action"`
	} `json:"tasks"`
}

// crateTaskRow is the task a membership act returns, with its crate.
type crateTaskRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Crate *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"crate"`
}

func crateCmd() *cobra.Command { return crateCmdWith(newClient) }

// crateCmdWith builds the command group over the client that connect
// returns, so tests can point it at a test server.
func crateCmdWith(connect func() *client) *cobra.Command {
	command := &cobra.Command{Use: "crate", Short: "List, read, create, and edit crates, and add or remove tasks", SilenceUsage: true, SilenceErrors: true}
	var asJSON bool
	command.PersistentFlags().BoolVar(&asJSON, "json", false, "print the server's response body")

	var state string
	list := &cobra.Command{Use: "list", Short: "List the crates of the workspace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/v1/crates"
			if cmd.Flags().Changed("state") {
				path += "?" + url.Values{"state": {state}}.Encode()
			}
			body, err := crateRequest(connect(), http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			return printCrateList(cmd.OutOrStdout(), body, asJSON)
		},
	}
	list.Flags().StringVar(&state, "state", "open", "crates to list: open, closed, or all")

	show := &cobra.Command{Use: "show <crate-id>", Short: "Show one crate with its requirements and tasks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := crateRequest(connect(), http.MethodGet, "/v1/crates/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			return printCrateDetail(cmd.OutOrStdout(), body, asJSON)
		},
	}

	var name, targetDate string
	var requirements []string
	create := &cobra.Command{Use: "create", Short: "Create an open crate", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fields := map[string]any{"name": name}
			crateEditFields(cmd, fields, targetDate, requirements)
			body, err := crateRequest(connect(), http.MethodPost, "/v1/crates", fields)
			if err != nil {
				return err
			}
			return printCrateRow(cmd.OutOrStdout(), body, asJSON, "Created crate")
		},
	}
	create.Flags().StringVar(&name, "name", "", "crate name, 1 to 120 characters")
	create.Flags().StringVar(&targetDate, "target-date", "", "target date, written YYYY-MM-DD")
	create.Flags().StringArrayVar(&requirements, "requirement", nil, "confirmed requirement ID that the crate declares (repeat for each ID)")

	var editName, editTargetDate string
	var editRequirements []string
	edit := &cobra.Command{Use: "edit <crate-id>", Short: "Change the name, target date, or declared requirements of a crate", Args: cobra.ExactArgs(1),
		Long: "Change only the fields you name. An empty --target-date removes the target date. An empty --requirement removes every declared requirement. A list of --requirement values replaces the declared list.",
		RunE: func(cmd *cobra.Command, args []string) error {
			fields := map[string]any{}
			if cmd.Flags().Changed("name") {
				fields["name"] = editName
			}
			crateEditFields(cmd, fields, editTargetDate, editRequirements)
			body, err := crateRequest(connect(), http.MethodPatch, "/v1/crates/"+url.PathEscape(args[0]), fields)
			if err != nil {
				return err
			}
			return printCrateRow(cmd.OutOrStdout(), body, asJSON, "Changed crate")
		},
	}
	edit.Flags().StringVar(&editName, "name", "", "new crate name")
	edit.Flags().StringVar(&editTargetDate, "target-date", "", "new target date, written YYYY-MM-DD, or empty to remove it")
	edit.Flags().StringArrayVar(&editRequirements, "requirement", nil, "confirmed requirement ID of the new declared list (repeat for each ID), or empty to remove every ID")

	stateCommand := func(use, short, value, verb string) *cobra.Command {
		return &cobra.Command{Use: use + " <crate-id>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				body, err := crateRequest(connect(), http.MethodPatch, "/v1/crates/"+url.PathEscape(args[0]), map[string]any{"state": value})
				if err != nil {
					return err
				}
				return printCrateRow(cmd.OutOrStdout(), body, asJSON, verb)
			},
		}
	}

	add := &cobra.Command{Use: "add <task-id> <crate-id>", Short: "Add a task to an open crate", Args: cobra.ExactArgs(2),
		Long: "Add the task to the crate. A task is in one crate at most, so the task leaves the crate it was in.",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := crateRequest(connect(), http.MethodPut, "/v1/tasks/"+url.PathEscape(args[0])+"/crate", map[string]any{"crate_id": args[1]})
			if err != nil {
				return err
			}
			return printCrateTask(cmd.OutOrStdout(), body, asJSON)
		},
	}
	remove := &cobra.Command{Use: "remove <task-id>", Short: "Remove a task from its crate", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := crateRequest(connect(), http.MethodDelete, "/v1/tasks/"+url.PathEscape(args[0])+"/crate", nil)
			if err != nil {
				return err
			}
			return printCrateTask(cmd.OutOrStdout(), body, asJSON)
		},
	}

	command.AddCommand(list, show, create, edit,
		stateCommand("close", "Close a crate", "closed", "Closed crate"),
		stateCommand("reopen", "Reopen a closed crate", "open", "Reopened crate"),
		add, remove)
	return command
}

// crateEditFields adds the target date and the declared requirements when
// their flags are present. An empty target date sends null, and empty
// requirement values send an empty list, so the route clears the field.
func crateEditFields(cmd *cobra.Command, fields map[string]any, targetDate string, requirements []string) {
	if cmd.Flags().Changed("target-date") {
		if strings.TrimSpace(targetDate) == "" {
			fields["target_date"] = nil
		} else {
			fields["target_date"] = targetDate
		}
	}
	if cmd.Flags().Changed("requirement") {
		ids := []string{}
		for _, id := range requirements {
			if strings.TrimSpace(id) != "" {
				ids = append(ids, id)
			}
		}
		fields["requirement_ids"] = ids
	}
}

// crateRequest sends one request and returns the response body. A refusal
// returns the route's status and message as the error.
func crateRequest(c *client, method, path string, fields map[string]any) ([]byte, error) {
	var body []byte
	if fields != nil {
		var err error
		if body, err = json.Marshal(fields); err != nil {
			return nil, err
		}
	}
	response, err := c.send(method, path, body, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func printCrateJSON(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "%s\n", bytes.TrimSpace(body))
	return err
}

func crateDate(date *string) string {
	if date == nil {
		return "no target date"
	}
	return *date
}

func crateLine(row crateRow) string {
	return fmt.Sprintf("%s  %s  %s  %d open, %d merged, %d total  %s", row.ID, row.State, crateDate(row.TargetDate), row.OpenTaskCount, row.MergedTaskCount, row.TotalTaskCount, row.Name)
}

func printCrateList(w io.Writer, body []byte, asJSON bool) error {
	if asJSON {
		return printCrateJSON(w, body)
	}
	var rows []crateRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("decode crates: %w", err)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(w, crateLine(row)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%d crates\n", len(rows))
	return err
}

func printCrateRow(w io.Writer, body []byte, asJSON bool, verb string) error {
	if asJSON {
		return printCrateJSON(w, body)
	}
	var row crateRow
	if err := json.Unmarshal(body, &row); err != nil {
		return fmt.Errorf("decode crate: %w", err)
	}
	_, err := fmt.Fprintf(w, "%s %s.\n%s\n", verb, row.ID, crateLine(row))
	return err
}

func printCrateDetail(w io.Writer, body []byte, asJSON bool) error {
	if asJSON {
		return printCrateJSON(w, body)
	}
	var detail crateDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return fmt.Errorf("decode crate: %w", err)
	}
	var out strings.Builder
	fmt.Fprintln(&out, crateLine(detail.crateRow))
	if detail.Shipped {
		fmt.Fprintln(&out, "Shipped: every task is merged or closed.")
	}
	fmt.Fprintln(&out, "Requirements:")
	if len(detail.Requirements) == 0 {
		fmt.Fprintln(&out, "  none declared")
	}
	for _, requirement := range detail.Requirements {
		fmt.Fprintf(&out, "  %s  %d tasks  %s\n", requirement.ID, requirement.TaskCount, requirement.Title)
	}
	if len(detail.UndeclaredRequirements) > 0 {
		fmt.Fprintln(&out, "Requirements that tasks have but the crate does not declare:")
		for _, requirement := range detail.UndeclaredRequirements {
			fmt.Fprintf(&out, "  %s  %d tasks  %s\n", requirement.ID, requirement.TaskCount, requirement.Title)
		}
	}
	fmt.Fprintln(&out, "Tasks:")
	if len(detail.Tasks) == 0 {
		fmt.Fprintln(&out, "  none")
	}
	for _, task := range detail.Tasks {
		fmt.Fprintf(&out, "  %s  %s  %s  %s\n", task.ID, task.State, task.Title, task.NextAction.Label)
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func printCrateTask(w io.Writer, body []byte, asJSON bool) error {
	if asJSON {
		return printCrateJSON(w, body)
	}
	var task crateTaskRow
	if err := json.Unmarshal(body, &task); err != nil {
		return fmt.Errorf("decode task: %w", err)
	}
	var err error
	if task.Crate == nil {
		_, err = fmt.Fprintf(w, "Task %s is in no crate.\n", task.ID)
	} else {
		_, err = fmt.Fprintf(w, "Task %s is in crate %s (%s).\n", task.ID, task.Crate.ID, task.Crate.Name)
	}
	return err
}
