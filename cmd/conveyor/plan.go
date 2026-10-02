package main

import "github.com/spf13/cobra"

// planCmd is the local planning studio: draft, check, ask, review, and push a
// planning package. Every verb takes the draft directory as an argument and no
// verb invents a default location; only push contacts the server.
func planCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "plan",
		Short: "Draft, check, review, and push a local planning package",
	}
	command.AddCommand(planCheckCmd(), planAskCmd(), planReviewCmd(), planPushCmd())
	return command
}
