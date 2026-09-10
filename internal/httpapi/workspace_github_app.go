package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/go-chi/chi/v5/middleware"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type manifestState struct {
	session   [32]byte
	workspace string
	expires   time.Time
}
type manifestStates struct {
	mu     sync.Mutex
	values map[[32]byte]manifestState
	now    func() time.Time
}

func (s *manifestStates) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
func (s *manifestStates) issue(workspace, cookie string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[[32]byte]manifestState{}
	}
	now := s.clock()
	for key, value := range s.values {
		if !now.Before(value.expires) {
			delete(s.values, key)
		}
	}
	// Bound concurrent sessions and discard prior unused states for this session.
	session := sha256.Sum256([]byte(cookie))
	for key, value := range s.values {
		if value.workspace == workspace && value.session == session {
			delete(s.values, key)
		}
	}
	if len(s.values) >= 4096 {
		return "", errors.New("too many pending GitHub App connections")
	}
	s.values[sha256.Sum256([]byte(state))] = manifestState{session, workspace, now.Add(10 * time.Minute)}
	return state, nil
}
func (s *manifestStates) consume(workspace, cookie, state string) bool {
	if len(state) != 43 {
		return false
	}
	key := sha256.Sum256([]byte(state))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.values[key]
	if !ok || entry.workspace != workspace || entry.session != sha256.Sum256([]byte(cookie)) {
		return false
	}
	delete(s.values, key)
	return s.clock().Before(entry.expires)
}
func appHTTPError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func (s *Server) appClient() *github.AppClient {
	if s.GitHubApps != nil {
		return s.GitHubApps
	}
	return github.DefaultAppClient
}
func appSession(r *http.Request) (string, bool) {
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser || credential.Method != core.CredentialMethodSession {
		return "", false
	}
	cookie, err := r.Cookie(dashboardSessionCookie)
	returnCookie := ""
	if err == nil {
		returnCookie = cookie.Value
	}
	return returnCookie, returnCookie != ""
}
func (s *Server) createWorkspaceGitHubAppManifest(w http.ResponseWriter, r *http.Request) {
	cookie, ok := appSession(r)
	if !ok {
		appHTTPError(w, http.StatusUnauthorized, "dashboard_session_required")
		return
	}
	workspace, _ := store.WorkspaceFromContext(r.Context())
	if s.WorkspaceGitHubApps == nil || s.Workspaces == nil {
		appHTTPError(w, http.StatusNotFound, "github_app_unavailable")
		return
	}
	record, err := s.Workspaces.GetWorkspace(r.Context(), workspace)
	if err != nil {
		writeWorkspaceNotFound(w)
		return
	}
	base, err := url.Parse(strings.TrimRight(s.InvitationDelivery.PublicURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.RawQuery != "" || base.Fragment != "" {
		appHTTPError(w, http.StatusServiceUnavailable, "public_url_required")
		return
	}
	state, err := s.appStates.issue(workspace, cookie)
	if err != nil {
		appHTTPError(w, http.StatusInternalServerError, "manifest_state_failed")
		return
	}
	path := base.String() + "/v1/workspaces/" + url.PathEscape(workspace) + "/github-app"
	manifest := map[string]any{"name": "Conveyor " + record.Name, "url": base.String(), "public": true, "redirect_url": path + "/callback", "setup_url": path + "/setup", "setup_on_update": true, "hook_attributes": map[string]bool{"active": false}, "default_permissions": map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write", "issues": "write", "statuses": "write"}}
	s.setAppReturnCookie(w, r, workspace, cookie)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest, "state": state})
}
func (s *Server) workspaceGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	cookie, ok := appSession(r)
	if !ok {
		appHTTPError(w, http.StatusUnauthorized, "dashboard_session_required")
		return
	}
	workspace, _ := store.WorkspaceFromContext(r.Context())
	if !s.appStates.consume(workspace, cookie, r.URL.Query().Get("state")) {
		appHTTPError(w, http.StatusBadRequest, "invalid_manifest_state")
		return
	}
	if s.WorkspaceGitHubApps == nil {
		appHTTPError(w, http.StatusNotFound, "github_app_unavailable")
		return
	}
	app, err := s.appClient().ConvertManifest(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		appHTTPError(w, http.StatusUnprocessableEntity, "manifest_conversion_failed")
		return
	}
	setupState, err := s.appSetupStates.issue(workspace, cookie)
	if err != nil {
		appHTTPError(w, http.StatusInternalServerError, "installation_state_failed")
		return
	}
	if _, err = s.WorkspaceGitHubApps.StoreWorkspaceGitHubApp(r.Context(), workspace, app); err != nil {
		appHTTPError(w, http.StatusUnprocessableEntity, "github_app_store_failed")
		return
	}
	s.appClient().Invalidate(workspace)
	s.setAppReturnCookie(w, r, workspace, cookie)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "https://github.com/apps/"+app.AppSlug+"/installations/new?state="+url.QueryEscape(setupState), http.StatusFound)
}
func (s *Server) workspaceGitHubAppSetup(w http.ResponseWriter, r *http.Request) {
	cookie, ok := appSession(r)
	if !ok {
		appHTTPError(w, http.StatusUnauthorized, "dashboard_session_required")
		return
	}
	workspace, _ := store.WorkspaceFromContext(r.Context())
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	action := r.URL.Query().Get("setup_action")
	if err != nil || id <= 0 || (action != "install" && action != "update") {
		appHTTPError(w, http.StatusBadRequest, "invalid_installation")
		return
	}
	if s.WorkspaceGitHubApps == nil {
		appHTTPError(w, http.StatusNotFound, "github_app_unavailable")
		return
	}
	app, err := s.WorkspaceGitHubApps.GetWorkspaceGitHubAppForUse(r.Context(), workspace)
	if err != nil {
		appHTTPError(w, http.StatusNotFound, "github_app_not_connected")
		return
	}
	if !s.appSetupStates.consume(workspace, cookie, r.URL.Query().Get("state")) {
		appHTTPError(w, http.StatusBadRequest, "invalid_installation_state")
		return
	}
	installation, err := s.appClient().Installation(r.Context(), workspace, github.AppCredentials{AppID: app.AppID, PrivateKey: app.PrivateKey}, id)
	if err != nil {
		appHTTPError(w, http.StatusUnprocessableEntity, "installation_verification_failed")
		return
	}
	if _, err = s.WorkspaceGitHubApps.RecordWorkspaceGitHubAppInstallation(r.Context(), workspace, app.AppID, id, installation.Account.Login); err != nil {
		appHTTPError(w, http.StatusConflict, "github_app_changed")
		return
	}
	s.appClient().Invalidate(workspace)
	http.Redirect(w, r, strings.TrimRight(s.InvitationDelivery.PublicURL, "/")+"/settings?workspace="+url.QueryEscape(workspace), http.StatusFound)
}
func (s *Server) getWorkspaceGitHubApp(w http.ResponseWriter, r *http.Request) {
	workspace, _ := store.WorkspaceFromContext(r.Context())
	if s.WorkspaceGitHubApps == nil {
		appHTTPError(w, http.StatusNotFound, "github_app_unavailable")
		return
	}
	status, err := s.WorkspaceGitHubApps.GetWorkspaceGitHubAppStatus(r.Context(), workspace)
	if err != nil {
		appHTTPError(w, http.StatusInternalServerError, "github_app_status_failed")
		return
	}
	type repositoryCoverage struct {
		Name    string `json:"name"`
		Covered bool   `json:"covered"`
	}
	repositories := []repositoryCoverage{}
	covered := map[string]bool{}
	if status.Connected && status.InstallationID > 0 {
		token, err := s.appClient().WorkspaceToken(r.Context(), s.WorkspaceGitHubApps, workspace)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "github_app_permission", "forge_error_category": string(github.ForgePermission), "message": github.AppPermission(workspace).Error()})
			return
		}
		covered, err = s.appClient().Repositories(r.Context(), workspace, token)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "github_app_permission", "forge_error_category": string(github.ForgePermission), "message": github.AppPermission(workspace).Error()})
			return
		}
	}
	if s.ConfigProvider != nil {
		cfg, err := s.ConfigProvider(r.Context())
		if err != nil {
			appHTTPError(w, http.StatusInternalServerError, "workspace_config_failed")
			return
		}
		for _, repo := range cfg.Repos {
			repositories = append(repositories, repositoryCoverage{repo.Name, covered[strings.ToLower(repo.GitHub)]})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	result := map[string]any{"connected": status.Connected, "app_slug": status.AppSlug, "installation_account": status.InstallationAccount, "repositories": repositories}
	// An operator can resume installation from settings without registering a
	// replacement app. The state also protects setup_action=update returns.
	if cookie, ok := appSession(r); ok && status.Connected && s.Memberships != nil {
		credential, _ := store.CredentialFromContext(r.Context())
		allowed, err := s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, workspace, core.CapabilityManageWorkspace)
		if err == nil && allowed {
			state, err := s.appSetupStates.issue(workspace, cookie)
			if err == nil {
				result["installation_url"] = "https://github.com/apps/" + status.AppSlug + "/installations/new?state=" + url.QueryEscape(state)
				s.setAppReturnCookie(w, r, workspace, cookie)
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) deleteWorkspaceGitHubApp(w http.ResponseWriter, r *http.Request) {
	workspace, _ := store.WorkspaceFromContext(r.Context())
	if s.WorkspaceGitHubApps == nil {
		appHTTPError(w, http.StatusNotFound, "github_app_unavailable")
		return
	}
	if err := s.WorkspaceGitHubApps.DeleteWorkspaceGitHubApp(r.Context(), workspace); err != nil {
		appHTTPError(w, http.StatusInternalServerError, "github_app_disconnect_failed")
		return
	}
	s.appClient().Invalidate(workspace)
	w.WriteHeader(http.StatusNoContent)
}

// Manifest codes and session-bound state must not enter access logs. Route the
// original request while giving chi's logger a query-free copy for this flow.
func githubAppRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logged := r
		if strings.Contains(r.URL.Path, "/github-app") {
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Cache-Control", "no-store")
			logged = r.Clone(r.Context())
			u := *r.URL
			u.RawQuery = ""
			u.ForceQuery = false
			logged.URL = &u
			logged.RequestURI = u.RequestURI()
		}
		middleware.Logger(http.HandlerFunc(func(w http.ResponseWriter, lr *http.Request) { next.ServeHTTP(w, r.WithContext(lr.Context())) })).ServeHTTP(w, logged)
	})
}

// Keep the ordinary /v1 session cookie Strict. A ten-minute, HttpOnly copy
// scoped to this workspace's app endpoints permits GitHub's top-level GET
// returns. Both mutating GETs additionally consume session-bound state.
func (s *Server) setAppReturnCookie(w http.ResponseWriter, r *http.Request, workspace, value string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.HasPrefix(strings.ToLower(s.InvitationDelivery.PublicURL), "https://")
	http.SetCookie(w, &http.Cookie{Name: dashboardSessionCookie, Value: value, Path: "/v1/workspaces/" + url.PathEscape(workspace) + "/github-app", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 600, Expires: time.Now().Add(10 * time.Minute)})
}
