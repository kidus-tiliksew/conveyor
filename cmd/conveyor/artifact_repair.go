package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/spf13/cobra"
)

func artifactCmd() *cobra.Command {
	command := &cobra.Command{Use: "artifact", Short: "Manage artifacts"}
	var request store.ArtifactRepairRequest
	repair := &cobra.Command{Use: "repair <artifact-id>", Short: "Validate and audit an artifact media-type repair", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c := newClient()
		if strings.TrimSpace(c.workspace) == "" {
			return fmt.Errorf("--workspace is required")
		}
		if strings.TrimSpace(request.ExpectedOldContentType) == "" || strings.TrimSpace(request.NewContentType) == "" || strings.TrimSpace(request.RequestID) == "" {
			return fmt.Errorf("--expected-old-content-type, --content-type and --request-id are required")
		}
		var result store.ArtifactRepairResult
		payload, _ := json.Marshal(request)
		if err := c.do("POST", "/v1/artifacts/"+url.PathEscape(args[0])+"/metadata-repair", payload, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	repair.Flags().StringVar(&request.ExpectedOldContentType, "expected-old-content-type", "", "expected stored media type")
	repair.Flags().StringVar(&request.NewContentType, "content-type", "", "canonical detected image media type")
	repair.Flags().StringVar(&request.RequestID, "request-id", "", "workspace-scoped idempotency key")
	repair.Flags().BoolVar(&request.DryRun, "dry-run", false, "validate without changing metadata or reserving the request ID")
	command.AddCommand(repair)
	return command
}
