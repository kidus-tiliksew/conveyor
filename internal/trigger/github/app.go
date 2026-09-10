package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// AppCredentials signs only in memory (DEC-41; req-delivery-and-forge AC-1.6).
type AppCredentials struct {
	AppID      int64
	PrivateKey string
}

func (a AppCredentials) JWT(now time.Time) (string, error) {
	block, rest := pem.Decode([]byte(a.PrivateKey))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("invalid GitHub App private key")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil || a.AppID <= 0 || key.N.BitLen() < 2048 {
		return "", errors.New("invalid GitHub App RSA identity")
	}
	if err := key.Validate(); err != nil {
		return "", errors.New("invalid GitHub App RSA identity")
	}
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.AppID})
	input := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("sign GitHub App JWT failed")
	}
	token := input + "." + base64.RawURLEncoding.EncodeToString(signature)
	redact.RegisterSecret(a.PrivateKey, time.Time{})
	redact.RegisterSecret(token, now.Add(9*time.Minute))
	return token, nil
}

type AppRunner = func(context.Context, ...string) ([]byte, error)
type cachedAppToken struct {
	fingerprint [32]byte
	token       string
	expires     time.Time
}

// AppClient shares the existing explicit-token REST seam. The injectable
// transport is for GitHub fixtures; production never reads host credentials.
type AppClient struct {
	Runner  func(string, string) AppRunner
	HTTP    *http.Client
	BaseURL string
	Now     func() time.Time
	mu      sync.Mutex
	cache   map[string]cachedAppToken
}

var DefaultAppClient = &AppClient{}

// NewAppClient supplies an explicit GitHub HTTP transport, including fixtures.
func NewAppClient(client *http.Client, baseURL string) *AppClient {
	return &AppClient{HTTP: client, BaseURL: baseURL, Runner: func(token, identity string) AppRunner { return newRESTRunner(client, baseURL, token, identity) }}
}

func (c *AppClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}
func AppIdentity(workspace string) string { return "workspace " + workspace + " GitHub App" }
func AppPermission(workspace string) error {
	return PermissionError(fmt.Errorf("workspace %s GitHub App is missing, revoked, suspended, or lacks repository access; connect the GitHub App in workspace settings", workspace))
}
func (c *AppClient) runner(token, workspace string) AppRunner {
	if c.Runner != nil {
		return c.Runner(token, AppIdentity(workspace))
	}
	return RESTRunner(token, AppIdentity(workspace))
}
func appOperationError(workspace string, err error) error {
	if err == nil {
		return nil
	}
	// RESTRunner has no status field on errors; its stable HTTP 404 text is the
	// existing seam for an installation GitHub deliberately hides from the app.
	if ErrorCategory(err) == ForgePermission || strings.Contains(err.Error(), "status 404") {
		return AppPermission(workspace)
	}
	// Do not propagate a GitHub response body or a transport URL carrying a code.
	return &Error{Category: ErrorCategory(err), Err: errors.New("GitHub App request failed")}
}

type AppInstallation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

func (c *AppClient) Installation(ctx context.Context, workspace string, app AppCredentials, id int64) (AppInstallation, error) {
	var installation AppInstallation
	jwt, err := app.JWT(c.now())
	if err != nil || id <= 0 {
		return installation, AppPermission(workspace)
	}
	raw, err := c.runner(jwt, workspace)(ctx, "api", fmt.Sprintf("/app/installations/%d", id))
	if err != nil {
		return installation, appOperationError(workspace, err)
	}
	if json.Unmarshal(raw, &installation) != nil {
		return installation, forgeResponseError("invalid GitHub App installation response")
	}
	if installation.ID != id || installation.AppID != app.AppID || installation.SuspendedAt != nil || installation.Account.Login == "" {
		return installation, AppPermission(workspace)
	}
	return installation, nil
}
func (c *AppClient) Token(ctx context.Context, workspace string, app core.WorkspaceGitHubAppCredential) (string, error) {
	if workspace == "" || !app.Connected || app.WorkspaceID != workspace || app.InstallationID <= 0 {
		return "", AppPermission(workspace)
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s:%s", app.AppID, app.InstallationID, app.ConnectedAt.Format(time.RFC3339Nano), app.PrivateKey)))
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if entry, ok := c.cache[workspace]; ok && entry.fingerprint == fingerprint && now.Before(entry.expires.Add(-5*time.Minute)) {
		return entry.token, nil
	}
	delete(c.cache, workspace)
	credentials := AppCredentials{app.AppID, app.PrivateKey}
	if _, err := c.Installation(ctx, workspace, credentials, app.InstallationID); err != nil {
		return "", err
	}
	jwt, err := credentials.JWT(now)
	if err != nil {
		return "", AppPermission(workspace)
	}
	raw, err := c.runner(jwt, workspace)(ctx, "api", fmt.Sprintf("/app/installations/%d/access_tokens", app.InstallationID), "--method", "POST")
	if err != nil {
		return "", appOperationError(workspace, err)
	}
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return "", forgeResponseError("invalid GitHub App token response")
	}
	// Bound expiry from receipt: network time before GitHub mints a token
	// must not make a legitimate one-hour token appear too long-lived.
	now = c.now()
	// Register even malformed candidate tokens before returning any diagnostic.
	redact.RegisterSecret(result.Token, result.ExpiresAt)
	if result.Token == "" || !result.ExpiresAt.After(now.Add(5*time.Minute)) || result.ExpiresAt.After(now.Add(time.Hour)) {
		return "", forgeResponseError("invalid GitHub App token expiry")
	}
	if c.cache == nil {
		c.cache = map[string]cachedAppToken{}
	}
	c.cache[workspace] = cachedAppToken{fingerprint, result.Token, result.ExpiresAt}
	return result.Token, nil
}
func (c *AppClient) Invalidate(workspace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, workspace)
}
func (c *AppClient) Repositories(ctx context.Context, workspace, token string) (map[string]bool, error) {
	raw, err := c.runner(token, workspace)(ctx, "api", "/installation/repositories?per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, appOperationError(workspace, err)
	}
	var pages []struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if json.Unmarshal(raw, &pages) != nil {
		return nil, forgeResponseError("invalid GitHub App repository response")
	}
	repos := map[string]bool{}
	for _, page := range pages {
		for _, r := range page.Repositories {
			repos[strings.ToLower(r.FullName)] = true
		}
	}
	return repos, nil
}

type AppStore interface {
	GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error)
}

func (c *AppClient) WorkspaceToken(ctx context.Context, st AppStore, workspace string) (string, error) {
	if st == nil {
		return "", AppPermission(workspace)
	}
	app, err := st.GetWorkspaceGitHubAppForUse(ctx, workspace)
	if err != nil {
		return "", AppPermission(workspace)
	}
	return c.Token(ctx, workspace, app)
}

var appSlugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
var manifestCodePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ConvertManifest is the one unauthenticated GitHub exchange. A code is neither
// logged nor reflected in an error; response key material stays in memory.
func (c *AppClient) ConvertManifest(ctx context.Context, code string) (core.WorkspaceGitHubAppCredential, error) {
	var app core.WorkspaceGitHubAppCredential
	failure := errors.New("GitHub App manifest conversion failed")
	if len(code) > 1024 || !manifestCodePattern.MatchString(code) {
		return app, failure
	}
	client := c.HTTP
	if client == nil {
		client = defaultRESTHTTPClient
	}
	base := c.BaseURL
	if base == "" {
		base = defaultRESTBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return app, failure
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Never follow a manifest response redirect carrying the single-use code.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(req)
	if err != nil {
		return app, failure
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return app, failure
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return app, failure
	}
	var result struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		ClientID string `json:"client_id"`
		PEM      string `json:"pem"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return app, failure
	}
	redact.RegisterSecret(result.PEM, time.Time{})
	if result.ID <= 0 || !appSlugPattern.MatchString(result.Slug) || result.ClientID == "" {
		return app, failure
	}
	if _, err = (AppCredentials{result.ID, result.PEM}).JWT(c.now()); err != nil {
		return app, failure
	}
	app.AppID = result.ID
	app.AppSlug = result.Slug
	app.ClientID = result.ClientID
	app.PrivateKey = result.PEM
	return app, nil
}

// RequireRepository makes uncovered installation access a permission failure
// before a monitor operation reaches a repository endpoint.
func (c *AppClient) RequireRepository(ctx context.Context, workspace, token, slug string) error {
	repos, err := c.Repositories(ctx, workspace, token)
	if err != nil {
		return err
	}
	if !repos[strings.ToLower(slug)] {
		return AppPermission(workspace)
	}
	return nil
}
