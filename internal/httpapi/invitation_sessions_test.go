package httpapi

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type invitationSessionFixture struct {
	credential core.AuthenticatedCredential
	issued     []string
	deliveries []string
	// scoped records every IssueInvitationSignInLink call as
	// "workspace|email|purpose". refuse, when set, decides whether a scoped
	// call answers store.ErrNotFound.
	scoped []string
	refuse func(workspaceID, email string) bool
}

func (f *invitationSessionFixture) IssueSignInLink(_ context.Context, email string) (core.IssuedSignInLink, error) {
	value := fmt.Sprintf("cv_signin_secret-%d", len(f.issued)+1)
	f.issued = append(f.issued, value)
	return core.IssuedSignInLink{Email: email, Value: value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *invitationSessionFixture) IssueInvitationSignInLink(ctx context.Context, workspaceID, email string, purpose store.SignInLinkPurpose) (core.IssuedSignInLink, error) {
	f.scoped = append(f.scoped, workspaceID+"|"+email+"|"+string(purpose))
	if f.refuse != nil && f.refuse(workspaceID, email) {
		return core.IssuedSignInLink{}, store.ErrNotFound
	}
	return f.IssueSignInLink(ctx, email)
}

func (f *invitationSessionFixture) RedeemSignInLink(context.Context, string) (core.DashboardSession, core.IdentityUser, error) {
	return core.DashboardSession{}, core.IdentityUser{}, nil
}

func (f *invitationSessionFixture) SignInWithPassword(context.Context, string, string) (core.DashboardSession, core.IdentityUser, error) {
	return core.DashboardSession{}, core.IdentityUser{}, core.ErrInvalidCredential
}

func (f *invitationSessionFixture) SetOwnPassword(context.Context, string, string, string, string) error {
	return nil
}

func (f *invitationSessionFixture) VerifyDashboardSession(context.Context, string) (core.AuthenticatedCredential, error) {
	return f.credential, nil
}

func (f *invitationSessionFixture) RevokeDashboardSession(context.Context, string, string) error {
	return nil
}

func (f *invitationSessionFixture) RecordInvitationDelivery(_ context.Context, _ string, outcome string) error {
	f.deliveries = append(f.deliveries, outcome)
	return nil
}

func TestInvitationLinkResponsePolicyAndFragmentFormat(t *testing.T) {
	t.Run("no SMTP surfaces one fragment link", func(t *testing.T) {
		fixture := &invitationSessionFixture{}
		server := NewServer(nil)
		server.InvitationSessions = fixture
		request := httptest.NewRequest("POST", "http://conveyor.example/v1/workspaces/demo/members", nil)
		result, err := server.issueAndDeliverSignInLink(request, "demo", "invitee@example.test", store.SignInLinkResend)
		if err != nil {
			t.Fatal(err)
		}
		if result.Delivery != "fallback" || result.SignInURL != "http://conveyor.example/sign-in#token=cv_signin_secret-1" {
			t.Fatalf("result=%+v", result)
		}
		if strings.Contains(result.SignInURL, "?token=") || len(fixture.deliveries) != 1 || fixture.deliveries[0] != "fallback" {
			t.Fatalf("result=%+v deliveries=%v", result, fixture.deliveries)
		}
	})

	t.Run("configured SMTP failure never returns link and resend rotates", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		listener.Close()
		fixture := &invitationSessionFixture{}
		server := NewServer(nil)
		server.InvitationSessions = fixture
		server.InvitationDelivery = config.InvitationDelivery{Host: host, Port: port, From: "conveyor@example.test", PublicURL: "https://conveyor.example"}
		request := httptest.NewRequest("POST", "https://conveyor.example/v1/workspaces/demo/members", nil)
		first, err := server.issueAndDeliverSignInLink(request, "demo", "invitee@example.test", store.SignInLinkResend)
		if err != nil {
			t.Fatal(err)
		}
		second, err := server.issueAndDeliverSignInLink(request, "demo", "invitee@example.test", store.SignInLinkResend)
		if err != nil {
			t.Fatal(err)
		}
		if first.Delivery != "failed" || first.SignInURL != "" || second.Delivery != "failed" || second.SignInURL != "" {
			t.Fatalf("first=%+v second=%+v", first, second)
		}
		if len(fixture.issued) != 2 || fixture.issued[0] == fixture.issued[1] || fmt.Sprint(fixture.deliveries) != "[failed failed]" {
			t.Fatalf("issued=%v deliveries=%v", fixture.issued, fixture.deliveries)
		}
	})
}

func TestSendSignInMailRequiresSTARTTLSAndSendsExactlyOneLink(t *testing.T) {
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	certificateServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	body := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		read := bufio.NewReader(conn)
		write := bufio.NewWriter(conn)
		reply := func(line string) error {
			if _, e := fmt.Fprintf(write, "%s\r\n", line); e != nil {
				return e
			}
			return write.Flush()
		}
		command := func(prefix string) error {
			line, e := read.ReadString('\n')
			if e != nil {
				return e
			}
			if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return fmt.Errorf("command %q, want %s", line, prefix)
			}
			return nil
		}
		if e := reply("220 fake-smtp ESMTP"); e != nil {
			serverErr <- e
			return
		}
		if e := command("EHLO"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250-fake-smtp"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250 STARTTLS"); e != nil {
			serverErr <- e
			return
		}
		if e := command("STARTTLS"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("220 ready"); e != nil {
			serverErr <- e
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if e := tlsConn.Handshake(); e != nil {
			serverErr <- e
			return
		}
		read = bufio.NewReader(tlsConn)
		write = bufio.NewWriter(tlsConn)
		if e := command("EHLO"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250 fake-smtp"); e != nil {
			serverErr <- e
			return
		}
		for _, prefix := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
			if e := command(prefix); e != nil {
				serverErr <- e
				return
			}
			response := "250 ok"
			if prefix == "DATA" {
				response = "354 end with dot"
			}
			if e := reply(response); e != nil {
				serverErr <- e
				return
			}
		}
		var lines []string
		for {
			line, e := read.ReadString('\n')
			if e != nil {
				serverErr <- e
				return
			}
			if strings.TrimSpace(line) == "." {
				break
			}
			lines = append(lines, line)
		}
		body <- strings.Join(lines, "")
		if e := reply("250 queued"); e != nil {
			serverErr <- e
			return
		}
		if e := command("QUIT"); e != nil {
			serverErr <- e
			return
		}
		serverErr <- reply("221 bye")
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	cfg := config.InvitationDelivery{Host: host, Port: port, From: "Conveyor <conveyor@example.test>", TLSConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	link := "https://conveyor.example/sign-in#token=one-secret"
	if err := sendSignInMail(cfg, "invitee@example.test", link); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	message := <-body
	if strings.Count(message, link) != 1 {
		t.Fatalf("message contains link %d times: %q", strings.Count(message, link), message)
	}
}

func TestSendSignInMailRefusesPlainSMTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "220 plain\r\n")
		read := bufio.NewReader(conn)
		read.ReadString('\n')
		fmt.Fprint(conn, "250 plain\r\n")
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	err = sendSignInMail(config.InvitationDelivery{Host: host, Port: port, From: "conveyor@example.test"}, "invitee@example.test", "https://example.test")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error=%v, want STARTTLS refusal", err)
	}
}

// refreshOutputSessions delegates to a real session store and replaces the
// refresh output of each successful verification, so the HTTP boundary's
// cookie contract is driven by exactly the value a store would return.
type refreshOutputSessions struct {
	store.InvitationSessionStore
	expires time.Time
}

func (s refreshOutputSessions) VerifyDashboardSession(ctx context.Context, candidate string) (core.AuthenticatedCredential, error) {
	credential, err := s.InvitationSessionStore.VerifyDashboardSession(ctx, candidate)
	if err == nil {
		credential.SessionExpiresAt = s.expires
	}
	return credential, err
}

// volatileSessionServer signs the deployment owner in on a volatile backend
// and returns the real server over it with the session cookie value.
func volatileSessionServer(t *testing.T) (*Server, store.Backend, core.DashboardSession) {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Cookie Org", Email: "cookie-owner@example.test", DisplayName: "Cookie Owner"}, "cookie-bootstrap"); err != nil {
		t.Fatal(err)
	}
	link, err := st.IssueSignInLink(ctx, "cookie-owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := st.RedeemSignInLink(ctx, link.Value)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.InvitationDelivery.PublicURL = "https://conveyor.example"
	return server, st, session
}

func getMeWithSession(server *Server, value string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: value})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// TestDashboardSessionCookieRefreshContract proves that the authentication
// boundary re-sets the session cookie only for nonzero refresh output, with
// the existing attributes and the exact stored expiry, and never on refusal
// (component-identity-membership).
func TestDashboardSessionCookieRefreshContract(t *testing.T) {
	t.Run("zero refresh output sends no cookie", func(t *testing.T) {
		server, st, session := volatileSessionServer(t)
		server.InvitationSessions = refreshOutputSessions{InvitationSessionStore: st}
		response := getMeWithSession(server, session.Value)
		if response.Code != http.StatusOK || response.Header().Values("Set-Cookie") != nil {
			t.Fatalf("status=%d set-cookie=%v body=%s", response.Code, response.Header().Values("Set-Cookie"), response.Body.String())
		}
	})
	t.Run("winning refresh sends the stored expiry", func(t *testing.T) {
		server, st, session := volatileSessionServer(t)
		expires := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
		server.InvitationSessions = refreshOutputSessions{InvitationSessionStore: st, expires: expires}
		response := getMeWithSession(server, session.Value)
		cookies := response.Result().Cookies()
		if response.Code != http.StatusOK || len(cookies) != 1 {
			t.Fatalf("status=%d cookies=%+v body=%s", response.Code, cookies, response.Body.String())
		}
		cookie := cookies[0]
		if cookie.Name != dashboardSessionCookie || cookie.Value != session.Value || cookie.Path != "/v1" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || !cookie.Expires.Equal(expires) || cookie.MaxAge <= 0 {
			t.Fatalf("refreshed cookie=%+v, want the existing attributes expiring at %s", cookie, expires)
		}
	})
	t.Run("refusal sends no cookie", func(t *testing.T) {
		server, st, session := volatileSessionServer(t)
		server.InvitationSessions = refreshOutputSessions{InvitationSessionStore: st, expires: time.Now().UTC().Add(time.Hour)}
		auth, err := st.VerifyDashboardSession(t.Context(), session.Value)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.RevokeDashboardSession(store.WithActor(t.Context(), store.SystemActor()), auth.OwnerUserID, session.ID); err != nil {
			t.Fatal(err)
		}
		response := getMeWithSession(server, session.Value)
		if response.Code != http.StatusUnauthorized || response.Header().Values("Set-Cookie") != nil {
			t.Fatalf("status=%d set-cookie=%v", response.Code, response.Header().Values("Set-Cookie"))
		}
	})
}

// TestDashboardSessionFreshVerificationSetsNoCookie proves through the real
// volatile store that requests inside the refresh interval send no cookie.
func TestDashboardSessionFreshVerificationSetsNoCookie(t *testing.T) {
	server, _, session := volatileSessionServer(t)
	for call := 1; call <= 2; call++ {
		response := getMeWithSession(server, session.Value)
		if response.Code != http.StatusOK || response.Header().Values("Set-Cookie") != nil {
			t.Fatalf("request %d status=%d set-cookie=%v body=%s", call, response.Code, response.Header().Values("Set-Cookie"), response.Body.String())
		}
	}
}
