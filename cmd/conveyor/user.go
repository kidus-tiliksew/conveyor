package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	"github.com/spf13/cobra"
)

type signInLinkStore interface {
	signInLinkIssuer
	Close()
}

type openSignInLinkStore func(context.Context, string) (signInLinkStore, error)

func userCmd() *cobra.Command {
	return newUserCmd(func(ctx context.Context, databaseURL string) (signInLinkStore, error) {
		return backend.Open(ctx, config.Database{Backend: "postgres", URL: databaseURL})
	})
}

func newUserCmd(openStore openSignInLinkStore) *cobra.Command {
	command := &cobra.Command{Use: "user", Short: "Manage deployment users from the Conveyor host"}
	issueLink := &cobra.Command{
		Use:   "issue-link <email>",
		Short: "Issue a fresh sign-in link for an existing account or pending invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_DATABASE_URL"))
			if databaseURL == "" {
				return errors.New("CONVEYOR_DATABASE_URL is required; set it to the deployment Postgres database and retry")
			}
			st, err := openStore(cmd.Context(), databaseURL)
			if err != nil {
				return fmt.Errorf("open deployment database: %w", err)
			}
			defer st.Close()
			if err = issueAndPrintSignInLink(cmd.Context(), cmd.OutOrStdout(), st, args[0], config.InvitationDeliveryFromEnvironment().PublicURL); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return errors.New("cannot issue sign-in link: no active account or pending invitation has that email")
				}
				return err
			}
			return nil
		},
	}
	command.AddCommand(issueLink)
	return command
}
