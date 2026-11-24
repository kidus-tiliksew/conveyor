package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The built-in OAuth 2.1 authorization server for `/mcp` (req-mcp-oauth
// REQ-1 to REQ-4; DEC-146; component-identity-membership invariants 50 to 54;
// component-http-api invariants 4 to 6). Every atomic transition, digest, and
// audit event lives in store.OAuthStore. This file owns only the HTTP
// policy: the public URL gate, discovery, client verification, request
// validation, and the mapping of store sentinels to OAuth errors.

// TrustedProxiesEnv lists the proxies whose X-Forwarded-For header the
// registration limiter believes, as comma-separated IP addresses or CIDR
// prefixes (component-identity-membership invariants 32 and 51).
const TrustedProxiesEnv = "CONVEYOR_TRUSTED_PROXIES"

const (
	// oauthFormBodyLimit bounds a token or revocation request body.
	oauthFormBodyLimit = 16 << 10
	// oauthRegistrationBodyLimit bounds a registration request body.
	oauthRegistrationBodyLimit = 16 << 10
	// oauthConsentBodyLimit bounds a consent decision body.
	oauthConsentBodyLimit = 16 << 10
	// oauthClientNameMaxBytes bounds a client name.
	oauthClientNameMaxBytes = 256
	// oauthRedirectURIMaxBytes bounds one redirect URI.
	oauthRedirectURIMaxBytes = 2048
	// oauthRegistrationMaxRedirectURIs bounds a registration's redirect list.
	oauthRegistrationMaxRedirectURIs = 8
	// oauthRegistrationMaxListEntries bounds grant_types and response_types.
	oauthRegistrationMaxListEntries = 4
	// oauthStateMaxBytes bounds the state value a client asks to be echoed.
	oauthStateMaxBytes = 2048
	// oauthTokenMaxBytes bounds a code, token, or verifier value.
	oauthTokenMaxBytes = 512
	// oauthRegistrationBudget and oauthRegistrationWindow bound registrations
	// per source address. oauthRegistrationSources bounds the limiter's
	// memory.
	oauthRegistrationBudget  = 10
	oauthRegistrationWindow  = time.Minute
	oauthRegistrationSources = 4096
	// oauthClientCleanupInterval bounds how often a registration also removes
	// expired unused clients.
	oauthClientCleanupInterval = 10 * time.Minute
)

const (
	oauthClientKindMetadataDocument    = "metadata_document"
	oauthClientKindDynamicRegistration = "dynamic_registration"
)

// oauthActor is the actor of an open OAuth route. A token or revocation
// request carries no signed-in person, so the authorization server itself
// records the act (component-persistence invariant 17).
func oauthActor() store.Actor { return store.SystemActor("oauth") }

// oauthIssuerFromPublicURL returns the issuer that publicURL names, or false
// when publicURL is not exactly its own canonical origin (req-mcp-oauth
// AC-1.1, AC-1.4; component-identity-membership invariant 50; DEC-146(1)).
// One rule decides: canonicalOAuthOrigin builds the canonical spelling from
// the parsed value, and OAuth is enabled only when the configured value
// equals it byte for byte. So the issuer, every advertised endpoint, and the
// session Origin comparison, which reads the same configured value, all use
// one string, the one a browser sends as Origin. Nothing derives an issuer
// from a request's Host header.
func oauthIssuerFromPublicURL(publicURL string) (string, bool) {
	canonical, ok := canonicalOAuthOrigin(publicURL)
	if !ok || canonical != publicURL {
		return "", false
	}
	return canonical, true
}

// canonicalOAuthOrigin parses raw and builds its canonical origin:
//   - a lowercase scheme, https, or http only for a loopback host;
//   - a lowercase DNS name with no trailing dot, no empty label, labels of
//     at most 63 bytes, a name of at most 253 bytes, and a last label that is
//     not all digits, so a malformed IPv4 address is never read as a name;
//   - or an IP address written as netip.Addr.String() renders it, an IPv6
//     address in brackets, with no zone and not IPv4-mapped;
//   - a decimal port from 1 to 65535, left out when it is the scheme's
//     default.
//
// raw must hold no user information, query, or fragment, and no path other
// than "/". It reports false when no canonical origin exists. The caller
// compares raw with the result, so every other spelling is refused.
func canonicalOAuthOrigin(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "\\") || hasOAuthControlOrSpace(raw) {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	defaultPort := map[string]string{"https": "443", "http": "80"}[scheme]
	if defaultPort == "" {
		return "", false
	}
	host := parsed.Hostname()
	if host == "" {
		return "", false
	}
	bracketed := strings.HasPrefix(parsed.Host, "[")
	var canonicalHost string
	if address, addrErr := netip.ParseAddr(host); addrErr == nil {
		if address.Zone() != "" || address.Is4In6() {
			return "", false
		}
		if address.Is6() {
			if !bracketed {
				return "", false
			}
			canonicalHost = "[" + address.String() + "]"
		} else {
			if bracketed {
				return "", false
			}
			canonicalHost = address.String()
		}
	} else {
		if bracketed {
			return "", false
		}
		name, nameOK := canonicalOAuthDNSName(host)
		if !nameOK {
			return "", false
		}
		canonicalHost = name
	}
	if scheme == "http" && !oauthLoopbackHost(strings.Trim(canonicalHost, "[]")) {
		return "", false
	}
	canonical := scheme + "://" + canonicalHost
	if port := parsed.Port(); port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 {
			return "", false
		}
		if decimal := strconv.Itoa(number); decimal != defaultPort {
			canonical += ":" + decimal
		}
	}
	return canonical, true
}

// canonicalOAuthDNSName lowercases a DNS host name and reports whether it is
// a valid name: letters, digits, and hyphens in labels of 1 to 63 bytes that
// neither start nor end with a hyphen, at most 253 bytes in all, with no
// trailing dot, and a last label that is not a number. A browser parses a
// host whose last label is a number as IPv4 (WHATWG URL "ends in a number"):
// all decimal digits, which also covers octal, single-integer, and
// fewer-than-four-part forms such as 0177.0.0.1, 2130706433, and 127.1, or
// 0x with hexadecimal digits, such as 0x7f000001. Such a host is never the
// canonical dotted-decimal form that netip renders, so it is refused.
func canonicalOAuthDNSName(host string) (string, bool) {
	name := strings.ToLower(host)
	if name == "" || len(name) > 253 {
		return "", false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", false
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return "", false
	}
	if hex, isHex := strings.CutPrefix(last, "0x"); isHex && strings.Trim(hex, "0123456789abcdef") == "" {
		return "", false
	}
	return name, true
}

// oauthLoopbackHost reports whether host is one of the three loopback names
// that loopback HTTP admits.
func oauthLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func hasOAuthControlOrSpace(value string) bool {
	for _, r := range value {
		if r <= ' ' || r == 0x7f {
			return true
		}
	}
	return false
}

// oauthServer is the enabled authorization server: the issuer and the store.
type oauthServer struct {
	issuer string
	store  store.OAuthStore
}

func (o oauthServer) audience() string { return o.issuer + "/mcp" }

func (o oauthServer) resourceMetadataURL() string {
	return o.issuer + "/.well-known/oauth-protected-resource/mcp"
}

// oauth reports the enabled authorization server. OAuth is enabled only when
// the configured public URL is a canonical origin and the backend has the
// OAuth store.
func (s *Server) oauth() (oauthServer, bool) {
	if s.OAuth == nil {
		return oauthServer{}, false
	}
	issuer, ok := oauthIssuerFromPublicURL(s.InvitationDelivery.PublicURL)
	if !ok {
		return oauthServer{}, false
	}
	return oauthServer{issuer: issuer, store: s.OAuth}, true
}

// writeOAuthUnavailable is the non-HTML refusal of every OAuth route while
// OAuth is disabled (req-mcp-oauth AC-1.4).
func writeOAuthUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "oauth_unavailable", "error_description": "OAuth is not enabled on this server"})
}

// requireOAuthEnabled refuses an OAuth route while OAuth is disabled.
func (s *Server) requireOAuthEnabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.oauth(); !ok {
			writeOAuthUnavailable(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireOAuthStore refuses a grant route on a backend without OAuth.
func (s *Server) requireOAuthStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.OAuth == nil {
			writeOAuthUnavailable(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// oauthNoStore marks every consent, token, revocation, and grant response,
// errors included, as uncacheable and referrer-free
// (component-identity-membership invariant 52).
func oauthNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setOAuthNoStoreHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func setOAuthNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// bearerValue parses one Authorization header value as a Bearer credential.
// The scheme is matched without regard to case and may be followed by more
// than one space (RFC 9110 section 11.1; RFC 6750 section 2.1).
func bearerValue(header string) (string, bool) {
	scheme, provided, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	provided = strings.TrimLeft(provided, " ")
	return provided, provided != ""
}

// oauthBearerPresented reports whether the request presents any OAuth wire
// value as its bearer. Only the prefix is read; no lookup happens.
func oauthBearerPresented(r *http.Request) bool {
	for _, header := range r.Header.Values("Authorization") {
		provided, ok := bearerValue(header)
		if !ok {
			continue
		}
		if _, isOAuth := core.OAuthCredentialKindOf(provided); isOAuth {
			return true
		}
	}
	return false
}

// refuseOAuthBearerOutsideMCP refuses an OAuth bearer on every route except
// `/mcp`, before routing, so no open route, REST route, claim-bound delivery
// route, or dashboard path can admit it, even beside a session cookie. It
// reads only the prefix and looks nothing up (req-security-boundaries AC-1.4;
// req-mcp-oauth AC-4.3; component-http-api invariant 6).
func refuseOAuthBearerOutsideMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The exception compares the escaped path, which is what the router
		// matches, so a percent-encoded "/mcp" that the router would send
		// elsewhere is refused too.
		if r.URL.EscapedPath() != "/mcp" && oauthBearerPresented(r) {
			if oauthLoggedPath(r.URL.Path) {
				// An OAuth route keeps its privacy headers on this early
				// refusal (component-identity-membership invariant 52).
				setOAuthNoStoreHeaders(w)
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireMCPEndpointAuth admits `/mcp` with a personal access token or an
// OAuth access token, never a session cookie (component-mcp-protocol
// invariant 4). An access token goes only to the OAuth verifier, and every
// other OAuth value is refused without a lookup. The claim-bound delivery
// routes keep requireMCPAuth, which admits only a personal access token.
// Live workspace capability stays in the shared MCP tool path, which reads
// the owner's binding on every call (req-mcp-oauth AC-4.3).
func (s *Server) requireMCPEndpointAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider, enabled := s.oauth()
		challenge := func() {
			// The resource metadata pointer is present only while OAuth is
			// enabled (req-mcp-oauth AC-1.3, AC-1.4).
			if enabled {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+provider.resourceMetadataURL()+`"`)
			} else {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		provided, ok := bearerValue(r.Header.Get("Authorization"))
		if !ok {
			challenge()
			return
		}
		var credential core.AuthenticatedCredential
		var verifyErr error
		if kind, isOAuth := core.OAuthCredentialKindOf(provided); isOAuth {
			switch {
			case kind != core.OAuthCredentialAccess || !enabled:
				verifyErr = core.ErrInvalidCredential
			default:
				credential, verifyErr = provider.store.VerifyOAuthAccessToken(r.Context(), provided)
				if verifyErr == nil && (credential.Method != core.CredentialMethodOAuthMCP || credential.Kind != core.CredentialUser) {
					verifyErr = core.ErrInvalidCredential
				}
				if verifyErr == nil {
					verifyErr = provider.requireGrantAudience(r.Context(), credential)
				}
			}
		} else {
			credential, verifyErr = s.verifyCredential(r.Context(), provided)
			// Every credential is a person's, so anything else is refused like
			// an unknown one (component-http-api invariant 4; DEC-147(1)).
			if verifyErr == nil && credential.Kind != core.CredentialUser {
				verifyErr = core.ErrInvalidCredential
			}
		}
		if verifyErr != nil {
			if !errors.Is(verifyErr, core.ErrInvalidCredential) {
				// A store failure stays distinct from a refused credential.
				log.Printf("httpapi: mcp credential verification failed: request_id=%q error=%q", middleware.GetReqID(r.Context()), redactedErrorLogText(verifyErr))
				http.Error(w, "credential verification failed", http.StatusInternalServerError)
				return
			}
			challenge()
			return
		}
		// Every act is the owning person's, as for a personal access token
		// (req-mcp-oauth AC-4.3; component-mcp-protocol invariant 4).
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireGrantAudience refuses an OAuth access token whose grant was not
// issued for the currently configured audience, <public-url>/mcp. After an
// operator changes the public URL, a token issued for the earlier origin is
// refused at its next use (DEC-146(4); req-mcp-oauth AC-4.3). The verified
// credential's ID is its grant's ID (store.OAuthCredential), and the store's
// owner-scoped grant listing returns each unrevoked grant's resource, so the
// check needs no change to the store contract. A missing grant, such as one
// revoked after verification, is refused too.
func (o oauthServer) requireGrantAudience(ctx context.Context, credential core.AuthenticatedCredential) error {
	grants, err := o.store.ListOwnOAuthGrants(ctx, credential.OwnerUserID)
	if err != nil {
		return fmt.Errorf("read OAuth grant audience: %w", err)
	}
	for _, grant := range grants {
		if grant.ID == credential.ID {
			if grant.Resource != o.audience() {
				return core.ErrInvalidCredential
			}
			return nil
		}
	}
	return core.ErrInvalidCredential
}

// refuseAuthorizationHeader refuses, with 401, any request that carries an
// Authorization header at all, whatever its scheme or spelling, before the
// session cookie is read. It guards only the dashboard-session consent and
// grant mounts, so a bearer of any scheme spelling never falls back to the
// cookie there (approved plan v1; component-identity-membership invariants
// 52 and 54). Other routes keep authenticateHumanCredential's own rule.
func refuseAuthorizationHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["Authorization"]; present {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireDashboardSession admits only a dashboard session, after
// requireSelfServiceCredential authenticated it, so consent and grant
// management never accept a personal access token or an OAuth token
// (component-identity-membership invariants 52 and 54).
func requireDashboardSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, ok := store.CredentialFromContext(r.Context())
		if !ok || credential.Method != core.CredentialMethodSession {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Discovery (req-mcp-oauth AC-1.1, AC-1.2) ---

func (s *Server) getOAuthProtectedResource(w http.ResponseWriter, _ *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 provider.audience(),
		"authorization_servers":    []string{provider.issuer},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) getOAuthAuthorizationServer(w http.ResponseWriter, _ *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	// The authorization endpoint is the dashboard page, which calls the
	// session-only consent API (DEC-146(3)).
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                provider.issuer,
		"authorization_endpoint":                provider.issuer + "/oauth/authorize",
		"token_endpoint":                        provider.issuer + "/oauth/token",
		"revocation_endpoint":                   provider.issuer + "/oauth/revoke",
		"registration_endpoint":                 provider.issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"client_id_metadata_document_supported": true,
	})
}

// oauthPostOnly serves an OAuth endpoint that accepts only POST. The route is
// mounted for every method, as /mcp is, so any other method answers 405 with
// Allow: POST and a JSON body, and never falls through to the dashboard
// (component-http-api invariant 2).
func oauthPostOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts only POST")
			return
		}
		next(w, r)
	}
}

// notFoundJSON answers a well-known path this server does not serve with a
// JSON 404, never the dashboard HTML.
func notFoundJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}

// --- Shared client checks (req-mcp-oauth AC-2.1, AC-2.2) ---

// validateOAuthRedirectURI refuses a redirect URI that is not an absolute
// `https://` URI or a loopback `http://` URI, or that has a fragment, user
// information, or an oversized value. It only classifies: oauthRedirectURIMatches
// decides membership on the original strings.
func validateOAuthRedirectURI(redirectURI string) error {
	if redirectURI == "" || len(redirectURI) > oauthRedirectURIMaxBytes || strings.ContainsAny(redirectURI, "#\\") || hasOAuthControlOrSpace(redirectURI) {
		return errors.New("redirect URI must be an https or loopback http URI without a fragment")
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.Hostname() == "" {
		return errors.New("redirect URI must be an https or loopback http URI without a fragment")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if oauthLoopbackHost(parsed.Hostname()) {
			return nil
		}
	}
	return errors.New("redirect URI must be an https or loopback http URI without a fragment")
}

// oauthRedirectURIListed reports whether one of the client's redirect URIs
// matches requested. It is the one membership matcher for metadata-document
// and dynamically registered clients (component-identity-membership invariant
// 51). An authorization code stays bound to the exact requested string
// (req-mcp-oauth AC-3.3).
func oauthRedirectURIListed(listed []string, requested string) bool {
	for _, candidate := range listed {
		if oauthRedirectURIMatches(candidate, requested) {
			return true
		}
	}
	return false
}

// oauthRedirectURIMatches accepts identical strings, or two loopback HTTP URIs
// that differ only in port (req-mcp-oauth AC-2.4; RFC 8252 section 7.3).
func oauthRedirectURIMatches(listed, requested string) bool {
	if listed == requested {
		return true
	}
	listedPortless, ok := oauthLoopbackURIWithoutPort(listed)
	if !ok {
		return false
	}
	requestedPortless, ok := oauthLoopbackURIWithoutPort(requested)
	return ok && listedPortless == requestedPortless
}

// oauthLoopbackURIWithoutPort removes the port from a loopback HTTP URI and
// keeps every other byte as written. It reports false for any other URI, and
// for a port that is empty or outside 1 to 65535. It reads the original text,
// because a parsed URI decodes the host and path and does not tell an empty
// port from an absent one.
func oauthLoopbackURIWithoutPort(uri string) (string, bool) {
	const scheme = "http://"
	rest, ok := strings.CutPrefix(uri, scheme)
	if !ok {
		return "", false
	}
	authority, tail := rest, ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority, tail = rest[:end], rest[end:]
	}
	for _, host := range []string{"127.0.0.1", "[::1]", "localhost"} {
		suffix, ok := strings.CutPrefix(authority, host)
		if !ok {
			continue
		}
		if suffix != "" {
			port, ok := strings.CutPrefix(suffix, ":")
			if !ok {
				return "", false
			}
			if number, err := strconv.ParseUint(port, 10, 16); err != nil || number == 0 {
				return "", false
			}
		}
		return scheme + host + tail, true
	}
	return "", false
}

// validateOAuthGrantTypes admits authorization_code with optional
// refresh_token. An omitted list means authorization_code (RFC 7591).
func validateOAuthGrantTypes(grantTypes []string) error {
	if len(grantTypes) == 0 {
		return nil
	}
	if len(grantTypes) > oauthRegistrationMaxListEntries {
		return errors.New("too many grant types")
	}
	code := false
	for _, grantType := range grantTypes {
		switch grantType {
		case "authorization_code":
			code = true
		case "refresh_token":
		default:
			return errors.New("only authorization_code and refresh_token are supported")
		}
	}
	if !code {
		return errors.New("grant_types must include authorization_code")
	}
	return nil
}

// validateOAuthResponseTypes admits only the code response type.
func validateOAuthResponseTypes(responseTypes []string) error {
	if len(responseTypes) > oauthRegistrationMaxListEntries {
		return errors.New("too many response types")
	}
	for _, responseType := range responseTypes {
		if responseType != "code" {
			return errors.New("only the code response type is supported")
		}
	}
	return nil
}

// oauthMetadata returns the server's metadata-document resolver.
func (s *Server) oauthMetadata() *oauthMetadataResolver {
	s.oauthResolverOnce.Do(func() {
		if s.oauthResolver == nil {
			s.oauthResolver = &oauthMetadataResolver{}
		}
	})
	return s.oauthResolver
}

// oauthVerifiedClient is a client whose identity the server verified.
type oauthVerifiedClient struct {
	ID           string
	Name         string
	Kind         string
	RedirectURIs []string
}

// allowsRedirect reports whether redirectURI is one of the client's redirect
// URIs (req-mcp-oauth AC-2.4).
func (client oauthVerifiedClient) allowsRedirect(redirectURI string) bool {
	return oauthRedirectURIListed(client.RedirectURIs, redirectURI)
}

// errOAuthClientLookup marks a store failure during client verification, so
// it answers 500 rather than a client error.
type errOAuthClientLookup struct{ err error }

func (e errOAuthClientLookup) Error() string { return e.err.Error() }
func (e errOAuthClientLookup) Unwrap() error { return e.err }

// verifyOAuthClient resolves a client_id to a verified client: a metadata
// document for an `https://` ID, or a live dynamic registration otherwise.
// An expired unused registration reads as unknown (req-mcp-oauth AC-2.3).
func (s *Server) verifyOAuthClient(ctx context.Context, provider oauthServer, clientID string) (oauthVerifiedClient, error) {
	if clientID == "" || len(clientID) > oauthClientIDMaxBytes {
		return oauthVerifiedClient{}, errOAuthClientUnverified
	}
	if core.IsOAuthMetadataDocumentClientID(clientID) {
		document, err := s.oauthMetadata().resolve(ctx, clientID)
		if err != nil {
			return oauthVerifiedClient{}, errOAuthClientUnverified
		}
		return oauthVerifiedClient{ID: document.ClientID, Name: document.ClientName, Kind: oauthClientKindMetadataDocument, RedirectURIs: document.RedirectURIs}, nil
	}
	client, err := provider.store.GetOAuthClient(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return oauthVerifiedClient{}, errOAuthClientUnverified
	}
	if err != nil {
		return oauthVerifiedClient{}, errOAuthClientLookup{err: err}
	}
	if validateOAuthGrantTypes(client.GrantTypes) != nil {
		return oauthVerifiedClient{}, errOAuthClientUnverified
	}
	return oauthVerifiedClient{ID: client.ID, Name: client.Name, Kind: oauthClientKindDynamicRegistration, RedirectURIs: client.RedirectURIs}, nil
}

// --- Dynamic client registration (req-mcp-oauth AC-2.2, AC-2.3) ---

type oauthRegistrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod *string  `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	writeJSON(w, status, body)
}

func (s *Server) registerOAuthClient(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	// The limiter runs before the body is read, so a refused source costs
	// no decoding and no store call.
	if !s.oauthRegistrationLimiter().allow(s.oauthSourceAddress(r), time.Now()) {
		w.Header().Set("Retry-After", strconv.Itoa(int(oauthRegistrationWindow.Seconds())))
		writeOAuthError(w, http.StatusTooManyRequests, "too_many_requests", "too many client registrations from this address; try again later")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration body must be application/json")
		return
	}
	var request oauthRegistrationRequest
	// Unknown RFC 7591 fields, such as client_uri or scope, are ignored: the
	// server keeps only what it verifies.
	if err := decodeBoundedJSON(w, r, oauthRegistrationBodyLimit, &request); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "the registration body is larger than 16 KiB")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration body is not valid JSON")
		return
	}
	if request.TokenEndpointAuthMethod == nil || *request.TokenEndpointAuthMethod != "none" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be none")
		return
	}
	if err := validateOAuthGrantTypes(request.GrantTypes); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	if err := validateOAuthResponseTypes(request.ResponseTypes); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	if len(request.ClientName) > oauthClientNameMaxBytes || hasOAuthControl(request.ClientName) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "client_name is too long or holds control characters")
		return
	}
	if len(request.RedirectURIs) == 0 || len(request.RedirectURIs) > oauthRegistrationMaxRedirectURIs {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("redirect_uris must hold 1 to %d URIs", oauthRegistrationMaxRedirectURIs))
		return
	}
	for _, redirectURI := range request.RedirectURIs {
		if err := validateOAuthRedirectURI(redirectURI); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	grantTypes := request.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code"}
	}
	if err := s.cleanupExpiredOAuthClients(r.Context(), provider); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	client, err := provider.store.RegisterOAuthClient(r.Context(), store.OAuthClientRegistration{Name: request.ClientName, RedirectURIs: request.RedirectURIs, GrantTypes: grantTypes})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOAuthRedirectURIRefused):
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "a redirect URI holds a value that looks like a credential")
		case errors.Is(err, store.ErrOAuthClientMetadataRefused):
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the client name holds a value that looks like a credential")
		case errors.Is(err, store.ErrRefusal):
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the client metadata was refused")
		default:
			// A store failure, a retryable conflict included, is logged
			// redacted with the request ID and answers a generic 500
			// (DEC-105(1); component-http-api invariant 16).
			writeStoreFailure(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func hasOAuthControl(value string) bool {
	for _, r := range value {
		if r < ' ' || r == 0x7f {
			return true
		}
	}
	return false
}

// cleanupExpiredOAuthClients removes expired unused registrations at most
// once per oauthClientCleanupInterval per process. The store never removes a
// client that backs a grant (req-mcp-oauth AC-2.3). A store failure is
// returned, and the registration answers it as a store failure.
func (s *Server) cleanupExpiredOAuthClients(ctx context.Context, provider oauthServer) error {
	now := time.Now()
	s.oauthCleanupMu.Lock()
	if !s.oauthCleanupAt.IsZero() && now.Sub(s.oauthCleanupAt) < oauthClientCleanupInterval {
		s.oauthCleanupMu.Unlock()
		return nil
	}
	s.oauthCleanupAt = now
	s.oauthCleanupMu.Unlock()
	if _, err := provider.store.DeleteExpiredOAuthClients(ctx); err != nil {
		return fmt.Errorf("delete expired OAuth clients: %w", err)
	}
	return nil
}

// oauthAttemptLimiter is a process-local fixed-window limiter with a bounded
// number of keys. It is a per-process protection, not durable proof.
type oauthAttemptLimiter struct {
	mu      sync.Mutex
	budget  int
	window  time.Duration
	maxKeys int
	windows map[string]oauthAttemptWindow
}

type oauthAttemptWindow struct {
	started time.Time
	count   int
}

func newOAuthAttemptLimiter(budget int, window time.Duration, maxKeys int) *oauthAttemptLimiter {
	return &oauthAttemptLimiter{budget: budget, window: window, maxKeys: maxKeys, windows: make(map[string]oauthAttemptWindow)}
}

// allow counts one attempt for key and reports whether it is within budget.
func (l *oauthAttemptLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, present := l.windows[key]
	if !present && len(l.windows) >= l.maxKeys {
		// Reclaim finished windows. A live window is never evicted, so a
		// source that used its budget cannot get a fresh one by crowding
		// the table; while the table stays full of live windows, a new
		// source is refused until a window ends.
		for candidate, window := range l.windows {
			if now.Sub(window.started) >= l.window {
				delete(l.windows, candidate)
			}
		}
		if len(l.windows) >= l.maxKeys {
			return false
		}
	}
	if !present || now.Sub(state.started) >= l.window {
		state = oauthAttemptWindow{started: now}
	}
	state.count++
	l.windows[key] = state
	return state.count <= l.budget
}

func (s *Server) oauthRegistrationLimiter() *oauthAttemptLimiter {
	s.oauthLimiterOnce.Do(func() {
		s.oauthLimiter = newOAuthAttemptLimiter(oauthRegistrationBudget, oauthRegistrationWindow, oauthRegistrationSources)
	})
	return s.oauthLimiter
}

// ParseTrustedProxies parses CONVEYOR_TRUSTED_PROXIES: comma-separated IP
// addresses or CIDR prefixes. Any invalid entry makes the whole setting
// invalid, and an invalid setting trusts no proxy.
func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if strings.Contains(field, "/") {
			prefix, err := netip.ParsePrefix(field)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid prefix %q", TrustedProxiesEnv, field)
			}
			// Peers are compared unmapped, so an IPv4-mapped prefix is
			// rewritten as the IPv4 prefix it covers.
			if prefix.Addr().Is4In6() {
				if prefix.Bits() < 96 {
					return nil, fmt.Errorf("%s: IPv4-mapped prefix %q must be /96 or longer; write it as an IPv4 prefix", TrustedProxiesEnv, field)
				}
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		address, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid address %q", TrustedProxiesEnv, field)
		}
		address = address.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
	}
	return prefixes, nil
}

// trustedProxiesFromEnvironment reads CONVEYOR_TRUSTED_PROXIES once at
// server construction. An invalid value is logged and trusts no proxy.
func trustedProxiesFromEnvironment() []netip.Prefix {
	prefixes, err := ParseTrustedProxies(os.Getenv(TrustedProxiesEnv))
	if err != nil {
		log.Printf("httpapi: %v; trusting no proxy", err)
		return nil
	}
	return prefixes
}

func (s *Server) trustedProxy(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range s.TrustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// oauthSourceAddress is the registration limiter's source address
// (component-identity-membership invariant 32's rule): RemoteAddr, or, when
// that peer is a trusted proxy, the nearest X-Forwarded-For address that is
// not a trusted proxy, read from right to left. A malformed chain is not
// believed, and the peer stays the source.
func (s *Server) oauthSourceAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return "peer:" + host
	}
	peer = peer.Unmap()
	if !s.trustedProxy(peer) {
		return peer.String()
	}
	var chain []netip.Addr
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, field := range strings.Split(header, ",") {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(field))
			// A zone-qualified address names a local interface, never a
			// client, so the chain is malformed and is not believed.
			if parseErr != nil || address.Zone() != "" {
				return peer.String()
			}
			chain = append(chain, address.Unmap())
		}
	}
	for index := len(chain) - 1; index >= 0; index-- {
		if !s.trustedProxy(chain[index]) {
			return chain[index].String()
		}
	}
	// A chain of trusted proxies names no client, so the direct peer stays
	// the source (component-identity-membership invariant 32).
	return peer.String()
}

// --- Consent (req-mcp-oauth AC-3.2 to AC-3.4) ---

// oauthAuthorizationRequest is one authorization request, read from the
// consent query string or the decision body.
type oauthAuthorizationRequest struct {
	ResponseType        string `json:"response_type"`
	ClientID            string `json:"client_id"`
	RedirectURI         string `json:"redirect_uri"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	State               string `json:"state"`
	// Resource records whether the request names a resource. An explicit
	// value, the empty string and JSON null included, is kept as sent, so
	// only omission takes the default audience (req-mcp-oauth AC-3.3).
	Resource oauthOptionalString `json:"resource"`
	Scope    string              `json:"scope"`
}

// oauthOptionalString is a string field that remembers whether the request
// named it. JSON null counts as named, with an empty value, so it is refused
// like an explicit empty string rather than read as an omission.
type oauthOptionalString struct {
	Set   bool
	Value string
}

func (o *oauthOptionalString) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		o.Value = ""
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}

// oauthConsentError is a refused authorization request. A non-empty
// redirect is set only after the client and its redirect URI were verified,
// so an unverified URI is never offered (req-mcp-oauth AC-3.4).
type oauthConsentError struct {
	status      int
	code        string
	description string
	redirectTo  string
	// storeFailure is set when the request failed because of the store, not
	// because of the request. The handler answers it with writeStoreFailure.
	storeFailure error
}

func writeOAuthConsentError(w http.ResponseWriter, failure *oauthConsentError) {
	body := map[string]string{"error": failure.code, "error_description": failure.description}
	if failure.redirectTo != "" {
		body["redirect_to"] = failure.redirectTo
	}
	writeJSON(w, failure.status, body)
}

// oauthConsentFields are the binding parameters that must appear at most
// once in a consent query.
var oauthConsentFields = []string{"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "state", "resource", "scope"}

func oauthAuthorizationRequestFromQuery(query url.Values) (oauthAuthorizationRequest, *oauthConsentError) {
	for _, field := range oauthConsentFields {
		if len(query[field]) > 1 {
			return oauthAuthorizationRequest{}, &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: field + " appears more than once"}
		}
	}
	return oauthAuthorizationRequest{
		ResponseType: query.Get("response_type"), ClientID: query.Get("client_id"), RedirectURI: query.Get("redirect_uri"),
		CodeChallenge: query.Get("code_challenge"), CodeChallengeMethod: query.Get("code_challenge_method"),
		State: query.Get("state"), Resource: oauthQueryField(query, "resource"), Scope: query.Get("scope"),
	}, nil
}

// oauthQueryField reads a query parameter and whether the query names it.
func oauthQueryField(query url.Values, field string) oauthOptionalString {
	values, present := query[field]
	if !present || len(values) == 0 {
		return oauthOptionalString{}
	}
	return oauthOptionalString{Set: true, Value: values[0]}
}

// verifyOAuthAuthorizationRequest verifies the client, then the redirect
// URI, then the rest of the request. Only errors after both verifications
// carry a redirect.
func (s *Server) verifyOAuthAuthorizationRequest(ctx context.Context, provider oauthServer, userID string, request oauthAuthorizationRequest) (oauthVerifiedClient, store.OAuthCodeRequest, *oauthConsentError) {
	pageError := func(code, description string) *oauthConsentError {
		return &oauthConsentError{status: http.StatusBadRequest, code: code, description: description}
	}
	if len(request.State) > oauthStateMaxBytes {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, pageError("invalid_request", "state is too long")
	}
	client, err := s.verifyOAuthClient(ctx, provider, request.ClientID)
	if err != nil {
		var lookup errOAuthClientLookup
		if errors.As(err, &lookup) {
			return oauthVerifiedClient{}, store.OAuthCodeRequest{}, &oauthConsentError{storeFailure: lookup.err}
		}
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, pageError("invalid_client", "the client could not be verified")
	}
	if validateOAuthRedirectURI(request.RedirectURI) != nil || !client.allowsRedirect(request.RedirectURI) {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, pageError("invalid_redirect_uri", "the redirect URI is not registered for this client")
	}
	redirectError := func(code, description string) *oauthConsentError {
		return &oauthConsentError{status: http.StatusBadRequest, code: code, description: description,
			redirectTo: oauthRedirectWith(request.RedirectURI, [][2]string{{"error", code}, {"error_description", description}}, request.State)}
	}
	if request.ResponseType != "code" {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, redirectError("unsupported_response_type", "response_type must be code")
	}
	if request.CodeChallengeMethod != "S256" {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, redirectError("invalid_request", "code_challenge_method must be S256")
	}
	if !validOAuthCodeChallenge(request.CodeChallenge) {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, redirectError("invalid_request", "code_challenge must be an S256 challenge")
	}
	// An omitted resource names the MCP audience; any other value is refused
	// (RFC 8707; req-mcp-oauth AC-3.3).
	resource := provider.audience()
	if request.Resource.Set {
		resource = request.Resource.Value
	}
	if resource != provider.audience() {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, redirectError("invalid_target", "resource must be "+provider.audience())
	}
	codeRequest := store.OAuthCodeRequest{UserID: userID, ClientID: client.ID, ClientName: client.Name, RedirectURI: request.RedirectURI, Resource: resource, CodeChallenge: request.CodeChallenge}
	// The store's own pre-write check refuses secret-shaped client metadata.
	// The refusal shows on Conveyor's page and never redirects.
	if err := store.ValidateOAuthCodeRequest(codeRequest); err != nil {
		return oauthVerifiedClient{}, store.OAuthCodeRequest{}, oauthStoreConsentRefusal(err)
	}
	return client, codeRequest, nil
}

// oauthStoreConsentRefusal maps a store refusal at consent to a page error.
func oauthStoreConsentRefusal(err error) *oauthConsentError {
	switch {
	case errors.Is(err, store.ErrOAuthRedirectURIRefused):
		return &oauthConsentError{status: http.StatusBadRequest, code: "invalid_redirect_uri", description: "the redirect URI holds a value that looks like a credential"}
	case errors.Is(err, store.ErrOAuthClientMetadataRefused):
		return &oauthConsentError{status: http.StatusBadRequest, code: "invalid_client", description: "the client metadata holds a value that looks like a credential"}
	default:
		return &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: "the authorization request was refused"}
	}
}

// validOAuthCodeChallenge reports whether challenge is 43 base64url
// characters, the length of an S256 challenge (RFC 7636).
func validOAuthCodeChallenge(challenge string) bool {
	if len(challenge) != 43 {
		return false
	}
	for _, r := range challenge {
		if !oauthBase64URLRune(r) {
			return false
		}
	}
	return true
}

func oauthBase64URLRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
}

// validOAuthCodeVerifier reports whether verifier is 43 to 128 unreserved
// characters (RFC 7636 section 4.1).
func validOAuthCodeVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !oauthBase64URLRune(r) && r != '.' && r != '~' {
			return false
		}
	}
	return true
}

// oauthResponseFields are the authorization response parameters a redirect
// URI's own query must not keep, so a stale value never reaches the client.
var oauthResponseFields = map[string]bool{"code": true, "state": true, "error": true, "error_description": true, "error_uri": true, "iss": true}

// oauthRedirectWith adds response parameters to a verified redirect URI. It
// keeps every other query parameter exactly as written, removes stale
// response parameters, and adds state as the client sent it.
func oauthRedirectWith(redirectURI string, fields [][2]string, state string) string {
	base, rawQuery, _ := strings.Cut(redirectURI, "?")
	var kept []string
	if rawQuery != "" {
		for _, pair := range strings.Split(rawQuery, "&") {
			if pair == "" {
				continue
			}
			key, _, _ := strings.Cut(pair, "=")
			if decoded, err := url.QueryUnescape(key); err == nil && oauthResponseFields[decoded] {
				continue
			}
			if oauthResponseFields[key] {
				continue
			}
			kept = append(kept, pair)
		}
	}
	for _, field := range fields {
		kept = append(kept, url.QueryEscape(field[0])+"="+url.QueryEscape(field[1]))
	}
	if state != "" {
		kept = append(kept, "state="+url.QueryEscape(state))
	}
	return base + "?" + strings.Join(kept, "&")
}

// oauthConsentView is what the consent page shows: the verified client, the
// redirect host, the signed-in person, and the audience.
type oauthConsentView struct {
	Client struct {
		ID     string `json:"client_id"`
		Name   string `json:"client_name"`
		Kind   string `json:"kind"`
		Origin string `json:"origin,omitempty"`
	} `json:"client"`
	RedirectHost string `json:"redirect_host"`
	User         struct {
		ID          string `json:"id"`
		Email       string `json:"email,omitempty"`
		DisplayName string `json:"display_name,omitempty"`
	} `json:"user"`
	Resource string `json:"resource"`
}

// getOAuthAuthorization validates an authorization request for the consent
// page. It writes nothing and mints nothing.
func (s *Server) getOAuthAuthorization(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	// A malformed pair is refused, never dropped, so a damaged resource
	// cannot read as omitted.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeOAuthConsentError(w, &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: "the authorization query is not valid URL encoding"})
		return
	}
	request, failure := oauthAuthorizationRequestFromQuery(query)
	if failure != nil {
		writeOAuthConsentError(w, failure)
		return
	}
	client, codeRequest, failure := s.verifyOAuthAuthorizationRequest(r.Context(), provider, credential.OwnerUserID, request)
	if failure != nil && failure.storeFailure != nil {
		writeStoreFailure(w, r, failure.storeFailure)
		return
	}
	if failure != nil {
		writeOAuthConsentError(w, failure)
		return
	}
	var view oauthConsentView
	view.Client.ID, view.Client.Name, view.Client.Kind = client.ID, client.Name, client.Kind
	if client.Kind == oauthClientKindMetadataDocument {
		if parsed, err := url.Parse(client.ID); err == nil {
			view.Client.Origin = parsed.Scheme + "://" + parsed.Host
		}
	}
	if parsed, err := url.Parse(codeRequest.RedirectURI); err == nil {
		view.RedirectHost = parsed.Host
	}
	view.User.ID = credential.OwnerUserID
	if s.CallerIdentities != nil {
		identity, err := s.CallerIdentities.GetCallerIdentity(r.Context(), credential.OwnerUserID, "")
		if err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		view.User.Email, view.User.DisplayName = identity.Email, identity.DisplayName
	}
	view.Resource = codeRequest.Resource
	writeJSON(w, http.StatusOK, view)
}

type oauthConsentDecision struct {
	oauthAuthorizationRequest
	Decision string `json:"decision"`
}

// postOAuthAuthorization records the person's explicit decision. It
// verifies the whole request again, and approval mints one single-use code
// through the store (req-mcp-oauth AC-3.3).
func (s *Server) postOAuthAuthorization(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	if r.URL.RawQuery != "" {
		writeOAuthConsentError(w, &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: "the decision must be sent in the request body"})
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	var decision oauthConsentDecision
	if err := decodeStrictBoundedJSON(w, r, oauthConsentBodyLimit, &decision); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOAuthConsentError(w, &oauthConsentError{status: http.StatusRequestEntityTooLarge, code: "invalid_request", description: "the decision body is larger than 16 KiB"})
			return
		}
		writeOAuthConsentError(w, &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: "the decision body is not valid"})
		return
	}
	if decision.Decision != "approve" && decision.Decision != "deny" {
		writeOAuthConsentError(w, &oauthConsentError{status: http.StatusBadRequest, code: "invalid_request", description: "decision must be approve or deny"})
		return
	}
	_, codeRequest, failure := s.verifyOAuthAuthorizationRequest(r.Context(), provider, credential.OwnerUserID, decision.oauthAuthorizationRequest)
	if failure != nil && failure.storeFailure != nil {
		writeStoreFailure(w, r, failure.storeFailure)
		return
	}
	if failure != nil {
		writeOAuthConsentError(w, failure)
		return
	}
	if decision.Decision == "deny" {
		writeJSON(w, http.StatusOK, map[string]string{"redirect_to": oauthRedirectWith(codeRequest.RedirectURI, [][2]string{{"error", "access_denied"}}, decision.State)})
		return
	}
	_, code, err := provider.store.IssueOAuthAuthorizationCode(r.Context(), codeRequest)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRefusal):
			writeOAuthConsentError(w, oauthStoreConsentRefusal(err))
		case errors.Is(err, store.ErrNotFound):
			// The person was deactivated after the session check.
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			writeStoreFailure(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect_to": oauthRedirectWith(codeRequest.RedirectURI, [][2]string{{"code", code}}, decision.State)})
}

// --- Token and revocation (req-mcp-oauth AC-4.1, AC-4.2, AC-4.4) ---

// readOAuthForm reads a bounded URL-encoded body. Parameters in the query
// string and repeated parameters are refused.
func readOAuthForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if r.URL.RawQuery != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "parameters must be sent in the request body")
		return nil, false
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return nil, false
	}
	// A declared Content-Length over the bound is refused before any read,
	// and a larger body answers 413 (component-http-api invariant 17;
	// DEC-105(3)).
	body, err := readBoundedBody(w, r, oauthFormBodyLimit)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_request", "the body is larger than 16 KiB")
			return nil, false
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is unreadable")
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is not valid form encoding")
		return nil, false
	}
	for _, values := range form {
		if len(values) > 1 {
			// The parameter name is untrusted input, so the refusal never
			// echoes it (req-security-boundaries REQ-2).
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "a parameter appears more than once")
			return nil, false
		}
	}
	return form, true
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func writeOAuthTokenPair(w http.ResponseWriter, pair core.IssuedOAuthTokenPair) {
	expiresIn := int64(time.Until(pair.AccessExpiresAt).Round(time.Second) / time.Second)
	if expiresIn < 1 {
		expiresIn = 1
	}
	writeJSON(w, http.StatusOK, oauthTokenResponse{AccessToken: pair.AccessToken, TokenType: "Bearer", ExpiresIn: expiresIn, RefreshToken: pair.RefreshToken})
}

func (s *Server) exchangeOAuthToken(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	form, ok := readOAuthForm(w, r)
	if !ok {
		return
	}
	ctx := store.WithActor(r.Context(), oauthActor())
	grantType := form.Get("grant_type")
	switch grantType {
	case "authorization_code", "refresh_token":
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	if len(clientID) > oauthClientIDMaxBytes {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "the client could not be verified")
		return
	}
	// RFC 8707: both grants name the MCP audience exactly.
	if resource := form.Get("resource"); resource != provider.audience() {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+provider.audience())
		return
	}
	switch grantType {
	case "authorization_code":
		code, redirectURI, verifier := form.Get("code"), form.Get("redirect_uri"), form.Get("code_verifier")
		if code == "" || redirectURI == "" || verifier == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code, redirect_uri, and code_verifier are required")
			return
		}
		if !validOAuthCodeVerifier(verifier) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code_verifier must be 43 to 128 unreserved characters")
			return
		}
		if len(code) > oauthTokenMaxBytes || len(redirectURI) > oauthRedirectURIMaxBytes {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid")
			return
		}
		// A metadata-document client is verified again at exchange; the
		// store holds no registration for it (req-mcp-oauth AC-2.1).
		if core.IsOAuthMetadataDocumentClientID(clientID) {
			client, err := s.verifyOAuthClient(ctx, provider, clientID)
			var lookup errOAuthClientLookup
			if errors.As(err, &lookup) {
				writeStoreFailure(w, r, lookup.err)
				return
			}
			if err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", "the client could not be verified")
				return
			}
			if !client.allowsRedirect(redirectURI) {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid")
				return
			}
		}
		pair, err := provider.store.ExchangeOAuthAuthorizationCode(ctx, store.OAuthCodeExchange{Code: code, ClientID: clientID, RedirectURI: redirectURI, Resource: provider.audience(), CodeVerifier: verifier})
		if err != nil {
			// A refused code, and secret-shaped metadata that the process
			// secret registry now refuses, answer the same OAuth error with
			// no detail.
			if errors.Is(err, store.ErrOAuthCodeInvalid) || errors.Is(err, store.ErrRefusal) {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid")
				return
			}
			writeStoreFailure(w, r, err)
			return
		}
		writeOAuthTokenPair(w, pair)
	case "refresh_token":
		refreshToken := form.Get("refresh_token")
		if refreshToken == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
			return
		}
		if len(refreshToken) > oauthTokenMaxBytes {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired, or revoked")
			return
		}
		pair, err := provider.store.RefreshOAuthTokens(ctx, store.OAuthRefreshExchange{RefreshToken: refreshToken, ClientID: clientID, Resource: provider.audience()})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrOAuthRefreshTokenReused):
				// A rotated refresh token was used again, also by a retry
				// after a lost response. The store already revoked the grant,
				// so the client must sign in again (req-mcp-oauth AC-4.2).
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used, so the grant is revoked; sign in again")
			case errors.Is(err, store.ErrOAuthTokenInvalid):
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired, or revoked")
			default:
				writeStoreFailure(w, r, err)
			}
			return
		}
		writeOAuthTokenPair(w, pair)
	}
}

// revokeOAuthToken implements RFC 7009 for public clients. An access or
// refresh token of the named client revokes its whole grant; an unknown
// token, another client's token, or a revoked grant answers 200 the same way
// (req-mcp-oauth AC-4.4).
func (s *Server) revokeOAuthToken(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.oauth()
	if !ok {
		writeOAuthUnavailable(w)
		return
	}
	form, ok := readOAuthForm(w, r)
	if !ok {
		return
	}
	token, clientID := form.Get("token"), form.Get("client_id")
	if token == "" || clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token and client_id are required")
		return
	}
	if len(token) > oauthTokenMaxBytes || len(clientID) > oauthClientIDMaxBytes {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := provider.store.RevokeOAuthToken(store.WithActor(r.Context(), oauthActor()), clientID, token); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- Grant management (req-mcp-oauth AC-4.4, AC-4.5) ---

// oauthGrantView is the hash-free listing entry the Settings page shows.
type oauthGrantView struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	Resource   string     `json:"resource"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func oauthGrantProjection(grant core.OAuthGrant) oauthGrantView {
	return oauthGrantView{ID: grant.ID, ClientID: grant.ClientID, ClientName: grant.ClientName, Resource: grant.Resource, CreatedAt: grant.CreatedAt, LastUsedAt: grant.LastUsedAt, RevokedAt: grant.RevokedAt}
}

// listOwnOAuthGrants answers the caller's unrevoked grants, newest first, as
// a JSON array under the shared page contract (component-http-api invariant
// 19; DEC-105(4)). A person's grants grow with that person's own
// authorizations, not with workspace history, so the route has no default
// page: without limit it answers the whole array and no paging header, and
// with limit it answers that page and sets X-Conveyor-Total,
// X-Conveyor-Limit, and X-Conveyor-Offset. A page parameter violation
// answers 400.
func (s *Server) listOwnOAuthGrants(w http.ResponseWriter, r *http.Request) {
	page, ok := readListPage(w, r, 0)
	if !ok {
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	grants, err := s.OAuth.ListOwnOAuthGrants(r.Context(), credential.OwnerUserID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	total := len(grants)
	if page.Paged {
		start := min(page.Offset, total)
		end := min(start+page.Limit, total)
		grants = grants[start:end]
	}
	views := make([]oauthGrantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, oauthGrantProjection(grant))
	}
	page.writeHeaders(w, total)
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) revokeOwnOAuthGrant(w http.ResponseWriter, r *http.Request) {
	credential, _ := store.CredentialFromContext(r.Context())
	grant, err := s.OAuth.RevokeOwnOAuthGrant(r.Context(), credential.OwnerUserID, chi.URLParam(r, "grant_id"))
	if errors.Is(err, store.ErrNotFound) {
		// Another person's grant answers like a missing one.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "grant not found"})
		return
	}
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, oauthGrantProjection(grant))
}

// oauthLoggedPath reports whether a request's query string must stay out of
// the access log: consent, token, revocation, registration, grant, and the
// dashboard's authorization page carry client state and codes.
func oauthLoggedPath(path string) bool {
	return strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/v1/oauth/") || strings.HasPrefix(path, "/.well-known/oauth-")
}
