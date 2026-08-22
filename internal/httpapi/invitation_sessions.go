package httpapi

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const dashboardSessionCookie = "conveyor_session"

func (s *Server) redeemSignInLink(w http.ResponseWriter, r *http.Request) {
	if s.InvitationSessions == nil {
		http.Error(w, "sign-in unavailable", http.StatusNotFound)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Token) == "" {
		writeValidationError(w, "token", errors.New("token is required"))
		return
	}
	session, user, err := s.InvitationSessions.RedeemSignInLink(r.Context(), request.Token)
	if err != nil {
		http.Error(w, "invalid or expired sign-in link", http.StatusUnauthorized)
		return
	}
	setDashboardSessionCookie(w, r, s.InvitationDelivery.PublicURL, session.Value, session.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "expires_at": session.ExpiresAt})
}

func setDashboardSessionCookie(w http.ResponseWriter, r *http.Request, publicURL, value string, expires time.Time) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.HasPrefix(strings.ToLower(publicURL), "https://")
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{Name: dashboardSessionCookie, Value: value, Path: "/v1", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge})
}

func (s *Server) signOutDashboardSession(w http.ResponseWriter, r *http.Request) {
	credential, _ := store.CredentialFromContext(r.Context())
	if credential.Method != core.CredentialMethodSession {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}
	if err := s.InvitationSessions.RevokeDashboardSession(r.Context(), credential.OwnerUserID, credential.ID); err != nil {
		http.Error(w, "session unavailable", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: dashboardSessionCookie, Value: "", Path: "/v1", HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.HasPrefix(strings.ToLower(s.InvitationDelivery.PublicURL), "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issueAndDeliverSignInLink(r *http.Request, email string) (core.MembershipGrant, error) {
	issued, err := s.InvitationSessions.IssueSignInLink(r.Context(), email)
	if err != nil {
		return core.MembershipGrant{}, err
	}
	link := s.signInURL(r, issued.Value)
	result := core.MembershipGrant{Email: issued.Email, SignInURL: link, Delivery: "fallback"}
	if !s.InvitationDelivery.SMTPConfigured() {
		s.recordInvitationDelivery(r, issued.Email, result.Delivery)
		return result, nil
	}
	if err := sendSignInMail(s.InvitationDelivery, issued.Email, link); err != nil {
		result.SignInURL = ""
		result.Delivery = "failed"
		s.recordInvitationDelivery(r, issued.Email, result.Delivery)
		return result, nil
	}
	result.SignInURL = ""
	result.Delivery = "sent"
	s.recordInvitationDelivery(r, issued.Email, result.Delivery)
	return result, nil
}

func (s *Server) recordInvitationDelivery(r *http.Request, email, outcome string) {
	if s.InvitationSessions != nil {
		if err := s.InvitationSessions.RecordInvitationDelivery(r.Context(), email, outcome); err != nil {
			log.Printf("audit invitation delivery outcome: %v", err)
		}
	}
}

func (s *Server) signInURL(r *http.Request, token string) string {
	base := s.InvitationDelivery.PublicURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	base += "/sign-in"
	return base + "#token=" + url.QueryEscape(token)
}

func (s *Server) resendWorkspaceInvitation(w http.ResponseWriter, r *http.Request) {
	if s.InvitationSessions == nil {
		http.Error(w, "sign-in unavailable", http.StatusNotFound)
		return
	}
	result, err := s.issueAndDeliverSignInLink(r, chiURLParamEmail(r))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeWorkspaceNotFound(w)
			return
		}
		http.Error(w, "could not issue sign-in link", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func chiURLParamEmail(r *http.Request) string {
	// chi routes on RawPath when it is populated, so route parameters can still
	// be percent-encoded. Keep invalid escapes unchanged for downstream
	// validation instead of turning malformed input into a transport error.
	email := chi.URLParam(r, "email")
	decoded, err := url.PathUnescape(email)
	if err != nil {
		return email
	}
	return decoded
}

func sendSignInMail(cfg config.InvitationDelivery, to, link string) error {
	address := net.JoinHostPort(cfg.Host, cfg.Port)
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return err
	}
	if err = conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	ok, _ := client.Extension("STARTTLS")
	if !ok {
		return errors.New("SMTP server does not advertise STARTTLS")
	}
	tlsConfig := cfg.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = cfg.Host
	}
	if tlsConfig.MinVersion == 0 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	if err = client.StartTLS(tlsConfig); err != nil {
		return err
	}
	if cfg.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return err
	}
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	message := "From: " + cfg.From + "\r\nTo: " + to + "\r\nSubject: Sign in to Conveyor\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nUse this one-time sign-in link:\r\n" + link + "\r\n"
	if _, err = wc.Write([]byte(message)); err != nil {
		return err
	}
	if err = wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}
