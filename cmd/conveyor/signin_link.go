package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const hostLocalSignInLinkActorID = "conveyor-cli"

type signInLinkIssuer interface {
	IssueSignInLink(context.Context, string) (core.IssuedSignInLink, error)
	RecordInvitationDelivery(context.Context, string, string) error
}

// issueAndPrintSignInLink is the host-local recovery and bootstrap delivery
// boundary required by req-260811-0ee057 v20 REQ-9/AC-9.3.
func issueAndPrintSignInLink(ctx context.Context, output io.Writer, issuer signInLinkIssuer, email, publicURL string) error {
	ctx = store.WithActor(ctx, store.Actor{ID: hostLocalSignInLinkActorID, Role: core.ActorSystem})
	issued, err := issuer.IssueSignInLink(ctx, email)
	if err != nil {
		return fmt.Errorf("issue sign-in link: %w", err)
	}
	if err = issuer.RecordInvitationDelivery(ctx, issued.Email, "fallback"); err != nil {
		return fmt.Errorf("record manual sign-in-link delivery: %w", err)
	}

	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base != "" {
		_, err = fmt.Fprintf(output, "Sign-in link for %s:\n%s/sign-in#token=%s\n", issued.Email, base, url.QueryEscape(issued.Value))
		return err
	}
	_, err = fmt.Fprintf(output, "Sign-in token for %s (CONVEYOR_PUBLIC_URL is not configured; deliver this token securely and enter it on the deployment sign-in page):\n%s\n", issued.Email, issued.Value)
	return err
}
