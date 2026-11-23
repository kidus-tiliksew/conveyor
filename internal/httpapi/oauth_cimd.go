package httpapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client ID Metadata Document resolution for the built-in `/mcp`
// authorization server (req-mcp-oauth AC-2.1; component-identity-membership
// invariant 51; DEC-146(2)). An `https://` client_id names a JSON document
// that the client publishes. The server fetches it over its own HTTPS
// transport, which checks every resolved address before it dials and dials
// only the checked address, so a hostname cannot redirect the fetch to a
// private network (SSRF).
const (
	// oauthMetadataDocumentTimeout bounds one complete fetch, redirects
	// included.
	oauthMetadataDocumentTimeout = 5 * time.Second
	// oauthMetadataDocumentMaxRedirects bounds the redirects one fetch follows.
	oauthMetadataDocumentMaxRedirects = 3
	// oauthMetadataDocumentMaxBytes bounds the decoded document.
	oauthMetadataDocumentMaxBytes = 64 << 10
	// oauthMetadataDocumentCacheLifetime is how long a verified document is
	// reused.
	oauthMetadataDocumentCacheLifetime = 5 * time.Minute
	// oauthMetadataDocumentCacheEntries bounds the cache.
	oauthMetadataDocumentCacheEntries = 256
	// oauthClientIDMaxBytes bounds a metadata-document client_id before any
	// fetch, so an oversized ID never reaches the network or the store.
	oauthClientIDMaxBytes = 2048
	// oauthMetadataDocumentMaxRedirectURIs bounds the redirect list of one
	// document.
	oauthMetadataDocumentMaxRedirectURIs = 16
)

// errOAuthClientUnverified is the one refusal of an unresolvable or invalid
// client. Its text names no upstream detail, which can echo untrusted input.
var errOAuthClientUnverified = errors.New("the client could not be verified")

// oauthMetadataDocument is one verified Client ID Metadata Document.
type oauthMetadataDocument struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
}

// allowsRedirect reports whether redirectURI is one of the document's
// redirect URIs, compared as exact strings.
func (document oauthMetadataDocument) allowsRedirect(redirectURI string) bool {
	for _, candidate := range document.RedirectURIs {
		if candidate == redirectURI {
			return true
		}
	}
	return false
}

// oauthMetadataResolver fetches and verifies metadata documents. The zero
// value is ready: it resolves with the system resolver, dials with a
// net.Dialer, verifies TLS against the system roots, and refuses every
// address oauthPublicAddress refuses. Tests set the hooks.
type oauthMetadataResolver struct {
	// lookup resolves a hostname to its addresses.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// dial opens a TCP connection to one checked address.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// allowAddress decides whether a resolved address may be dialed.
	allowAddress func(netip.Addr) bool
	// tlsConfig replaces the system roots in tests.
	tlsConfig *tls.Config
	// timeout replaces oauthMetadataDocumentTimeout in tests.
	timeout time.Duration
	now     func() time.Time

	clientOnce sync.Once
	client     *http.Client

	mu    sync.Mutex
	cache map[string]oauthMetadataCacheEntry
}

type oauthMetadataCacheEntry struct {
	document oauthMetadataDocument
	expires  time.Time
}

func (resolver *oauthMetadataResolver) fetchTimeout() time.Duration {
	if resolver.timeout > 0 {
		return resolver.timeout
	}
	return oauthMetadataDocumentTimeout
}

func (resolver *oauthMetadataResolver) clock() time.Time {
	if resolver.now != nil {
		return resolver.now()
	}
	return time.Now()
}

// resolve returns the verified document that clientID names. The client ID
// must be a valid metadata-document URL. A cached document is reused only
// within oauthMetadataDocumentCacheLifetime; callers still match their
// redirect URI against its list.
func (resolver *oauthMetadataResolver) resolve(ctx context.Context, clientID string) (oauthMetadataDocument, error) {
	if err := validateOAuthMetadataClientID(clientID); err != nil {
		return oauthMetadataDocument{}, err
	}
	now := resolver.clock()
	resolver.mu.Lock()
	if entry, ok := resolver.cache[clientID]; ok && now.Before(entry.expires) {
		resolver.mu.Unlock()
		return entry.document, nil
	}
	resolver.mu.Unlock()
	document, err := resolver.fetch(ctx, clientID)
	if err != nil {
		return oauthMetadataDocument{}, err
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.cache == nil {
		resolver.cache = make(map[string]oauthMetadataCacheEntry)
	}
	if _, present := resolver.cache[clientID]; !present && len(resolver.cache) >= oauthMetadataDocumentCacheEntries {
		// Evict expired entries first, then the entry closest to expiry, so
		// the cache never grows past its bound.
		oldestKey, oldest := "", time.Time{}
		for key, entry := range resolver.cache {
			if !now.Before(entry.expires) {
				delete(resolver.cache, key)
				continue
			}
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		}
		if len(resolver.cache) >= oauthMetadataDocumentCacheEntries && oldestKey != "" {
			delete(resolver.cache, oldestKey)
		}
	}
	resolver.cache[clientID] = oauthMetadataCacheEntry{document: document, expires: now.Add(oauthMetadataDocumentCacheLifetime)}
	return document, nil
}

// validateOAuthMetadataClientID refuses a client_id that cannot name a
// metadata document: not `https://`, too long, with user information, a
// fragment, no path, or a dot segment.
func validateOAuthMetadataClientID(clientID string) error {
	if len(clientID) > oauthClientIDMaxBytes || !strings.HasPrefix(clientID, "https://") || strings.ContainsAny(clientID, "#\\") || hasOAuthControlOrSpace(clientID) {
		return errOAuthClientUnverified
	}
	parsed, err := url.Parse(clientID)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return errOAuthClientUnverified
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return errOAuthClientUnverified
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return errOAuthClientUnverified
		}
	}
	return nil
}

// fetch performs one bounded HTTPS fetch and verifies the document.
func (resolver *oauthMetadataResolver) fetch(ctx context.Context, clientID string) (oauthMetadataDocument, error) {
	ctx, cancel := context.WithTimeout(ctx, resolver.fetchTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	request.Header.Set("Accept", "application/json")
	response, err := resolver.httpClient().Do(request)
	if err != nil {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, oauthMetadataDocumentMaxBytes+1))
	if err != nil || len(body) > oauthMetadataDocumentMaxBytes {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	var raw struct {
		ClientID                string   `json:"client_id"`
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod *string  `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	// The document must name itself and a public client. A document that
	// omits the method declares client_secret_basic (RFC 7591 section 2), so
	// it is refused like any other method.
	if raw.ClientID != clientID || raw.TokenEndpointAuthMethod == nil || *raw.TokenEndpointAuthMethod != "none" {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	if err := validateOAuthGrantTypes(raw.GrantTypes); err != nil {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	if err := validateOAuthResponseTypes(raw.ResponseTypes); err != nil {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	if len(raw.RedirectURIs) == 0 || len(raw.RedirectURIs) > oauthMetadataDocumentMaxRedirectURIs || len(raw.ClientName) > oauthClientNameMaxBytes {
		return oauthMetadataDocument{}, errOAuthClientUnverified
	}
	for _, redirectURI := range raw.RedirectURIs {
		if validateOAuthRedirectURI(redirectURI) != nil {
			return oauthMetadataDocument{}, errOAuthClientUnverified
		}
	}
	return oauthMetadataDocument{ClientID: raw.ClientID, ClientName: raw.ClientName, RedirectURIs: append([]string(nil), raw.RedirectURIs...)}, nil
}

// httpClient builds the dedicated transport once. It uses no environment
// proxy, so the checked address is the address dialed.
func (resolver *oauthMetadataResolver) httpClient() *http.Client {
	resolver.clientOnce.Do(func() {
		transport := &http.Transport{
			Proxy:                  nil,
			DialContext:            resolver.dialChecked,
			TLSClientConfig:        resolver.tlsConfig,
			TLSHandshakeTimeout:    resolver.fetchTimeout(),
			ResponseHeaderTimeout:  resolver.fetchTimeout(),
			MaxResponseHeaderBytes: 16 << 10,
			DisableKeepAlives:      true,
			ForceAttemptHTTP2:      false,
		}
		resolver.client = &http.Client{
			Transport: transport,
			Timeout:   resolver.fetchTimeout(),
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) > oauthMetadataDocumentMaxRedirects {
					return errOAuthClientUnverified
				}
				// Every hop must stay on HTTPS without user information.
				// dialChecked checks the hop's addresses again when it dials.
				if request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Hostname() == "" {
					return errOAuthClientUnverified
				}
				return nil
			},
		}
	})
	return resolver.client
}

// dialChecked resolves the host, refuses the dial when any resolved address
// is not public, and dials the checked addresses directly. The TLS layer
// above it still verifies the certificate for the URL hostname.
func (resolver *oauthMetadataResolver) dialChecked(ctx context.Context, network, address string) (net.Conn, error) {
	// One deadline covers the lookup and every candidate dial.
	ctx, cancel := context.WithTimeout(ctx, resolver.fetchTimeout())
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errOAuthClientUnverified
	}
	var addresses []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		addresses = []netip.Addr{literal}
	} else {
		lookup := resolver.lookup
		if lookup == nil {
			lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		addresses, err = lookup(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errOAuthClientUnverified
		}
	}
	allow := resolver.allowAddress
	if allow == nil {
		allow = oauthPublicAddress
	}
	for _, candidate := range addresses {
		if !allow(candidate.Unmap()) {
			return nil, errOAuthClientUnverified
		}
	}
	dial := resolver.dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: resolver.fetchTimeout()}
		dial = dialer.DialContext
	}
	var lastErr error = errOAuthClientUnverified
	for _, candidate := range addresses {
		conn, dialErr := dial(ctx, network, net.JoinHostPort(candidate.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("dial metadata document host: %w", lastErr)
}

// oauthBlockedPrefixes are special-purpose ranges (the IANA IPv4 and IPv6
// special-purpose address registries) that netip's predicates do not cover
// but that are not public destinations: this network, shared address space,
// IETF protocol assignments, documentation, the 6to4 relay, benchmarking,
// reserved, discard-only, NAT64, Teredo and other IETF assignments, 6to4,
// which can embed a private IPv4 address, and deprecated site-local.
var oauthBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("fec0::/10"),
}

// oauthPublicAddress reports whether address may be fetched: it refuses
// private, loopback, link-local, unspecified, multicast, and the special
// ranges above (req-mcp-oauth AC-2.1).
func oauthPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsInterfaceLocalMulticast() ||
		address.IsMulticast() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range oauthBlockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
