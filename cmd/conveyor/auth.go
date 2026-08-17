package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var readLoginToken = readTerminalLoginToken

func authCmd() *cobra.Command {
	command := &cobra.Command{Use: "auth", Short: "Manage local Conveyor login credentials"}
	login := &cobra.Command{
		Use: "login", Short: "Verify and store a personal access token", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			server, err := loginServer(cmd)
			if err != nil {
				return err
			}
			token, err := readLoginToken(cmd)
			if err != nil {
				return err
			}
			token = strings.TrimSpace(token)
			if token == "" {
				return errors.New("personal access token is required")
			}
			identity, err := (&client{base: server, token: token}).callerIdentity()
			if err != nil {
				return safeCredentialVerificationError(err)
			}
			if err := updateLocalServerConfig(server, func(entry *localServerConfig) { entry.Token = token }); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s <%s>\n", server, identity.DisplayName, identity.Email)
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the effective server and authenticated identity", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClient()
			if c.configErr != nil {
				return c.configErr
			}
			if c.token == "" {
				return fmt.Errorf("no credential is configured for %s; run `conveyor auth login`", c.base)
			}
			identity, err := c.callerIdentity()
			if err != nil {
				return err
			}
			label := ""
			if tokens, listErr := c.personalAccessTokens(); listErr == nil {
				if token, ok := matchingPersonalAccessToken(c.token, tokens); ok {
					label = token.Label
				}
			}
			output := cmd.OutOrStdout()
			rows := [][2]string{{"Server", c.base}, {"Identity", fmt.Sprintf("%s <%s>", identity.DisplayName, identity.Email)}}
			if label != "" {
				rows = append(rows, [2]string{"Token label", label})
			}
			return renderCLIStatusRows(output, outputIsTerminal(output), rows...)
		},
	}
	var revoke bool
	logout := &cobra.Command{
		Use: "logout", Short: "Remove the stored credential", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveClientConfig()
			if err != nil {
				return err
			}
			config, err := loadLocalAuthConfig()
			if err != nil {
				return err
			}
			entry, ok := config.Servers[resolved.Server.Value]
			if !ok || entry.Token == "" {
				return fmt.Errorf("no stored credential for %s", resolved.Server.Value)
			}
			if revoke {
				c := &client{base: resolved.Server.Value, token: entry.Token}
				tokens, listErr := c.personalAccessTokens()
				if listErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke remote token: %v\n", listErr)
				} else if token, matched := matchingPersonalAccessToken(entry.Token, tokens); !matched {
					fmt.Fprintln(cmd.ErrOrStderr(), "warning: stored credential is not a recognized personal access token; removing it locally only")
				} else if revokeErr := c.revokePersonalAccessToken(token.ID); revokeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke remote token: %v\n", revokeErr)
				}
			}
			if err := updateLocalServerConfig(resolved.Server.Value, func(current *localServerConfig) { current.Token = "" }); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out of %s\n", resolved.Server.Value)
			return nil
		},
	}
	logout.Flags().BoolVar(&revoke, "revoke", false, "also revoke the stored personal access token on the server")
	token := &cobra.Command{
		Use: "token", Short: "Print the stored credential for command substitution", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveClientConfig()
			if err != nil {
				return err
			}
			config, err := loadLocalAuthConfig()
			if err != nil {
				return err
			}
			value := config.Servers[resolved.Server.Value].Token
			if value == "" {
				return fmt.Errorf("no stored credential for %s; run `conveyor auth login`", resolved.Server.Value)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), value)
			return err
		},
	}
	command.AddCommand(login, status, logout, token)
	return command
}

func safeCredentialVerificationError(err error) error {
	message := err.Error()
	if status, _, found := strings.Cut(message, ": "); found {
		fields := strings.Fields(status)
		if len(fields) >= 2 && len(fields[0]) == 3 && fields[0][0] >= '4' && fields[0][0] <= '5' {
			return fmt.Errorf("credential verification failed: %s", status)
		}
	}
	return fmt.Errorf("credential verification failed: %w", err)
}

func loginServer(cmd *cobra.Command) (string, error) {
	if serverFlagExplicit || strings.TrimSpace(serverFlag) != "" {
		return normalizeServerURL(serverFlag)
	}
	if value := strings.TrimSpace(os.Getenv("CONVEYOR_ADDR")); value != "" {
		return normalizeServerURL(value)
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Server URL [http://localhost:8080]: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		line = "http://localhost:8080"
	}
	return normalizeServerURL(line)
}

func readTerminalLoginToken(cmd *cobra.Command) (string, error) {
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(input.Fd())) {
		return "", errors.New("auth login requires an interactive terminal for hidden token input; pipe is not accepted")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Personal access token: ")
	value, err := term.ReadPassword(int(input.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read hidden token: %w", err)
	}
	return string(value), nil
}
