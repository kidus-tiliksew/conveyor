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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

func appTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
func TestAppJWT(t *testing.T) {
	key, private := appTestKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	token, err := (AppCredentials{41, private}).JWT(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("JWT parts")
	}
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]int64
	if err = json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iat"] != now.Add(-time.Minute).Unix() || claims["exp"] != now.Add(9*time.Minute).Unix() || claims["iss"] != 41 {
		t.Fatalf("claims %v", claims)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	if _, err = (AppCredentials{41, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))}).JWT(now); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []AppCredentials{{41, "invalid"}, {0, private}, {41, private + "extra"}} {
		if _, err := invalid.JWT(now); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
func TestAppManifestInstallationTokensAndCoverage(t *testing.T) {
	_, private := appTestKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/code/conversions":
			if r.Method != "POST" || r.Header.Get("Authorization") != "" {
				t.Error("conversion credential or method")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 41, "slug": "conveyor-fixture", "client_id": "client", "pem": private})
		case "/app/installations/12":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ") {
				t.Error("installation lacks JWT")
			}
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"},"suspended_at":null}`)
		case "/app/installations/12/access_tokens":
			if r.Method != "POST" {
				t.Error("exchange method")
			}
			n := exchanges.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("installation-runtime-secret-%d", n), "expires_at": now.Add(time.Hour)})
		case "/installation/repositories":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer installation-runtime-secret-") {
				t.Error("listing lacks installation token")
			}
			fmt.Fprint(w, `{"repositories":[{"full_name":"Org/Repo"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := &AppClient{HTTP: server.Client(), BaseURL: server.URL, Now: func() time.Time { return now }, Runner: func(token, identity string) AppRunner {
		return newRESTRunner(server.Client(), server.URL, token, identity)
	}}
	app, err := client.ConvertManifest(t.Context(), "code")
	if err != nil {
		t.Fatal(err)
	}
	app.WorkspaceID = "demo"
	app.Connected = true
	app.InstallationID = 12
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Token(t.Context(), "demo", app); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatal("concurrent requests minted duplicate tokens")
	}
	first, err := client.Token(t.Context(), "demo", app)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.RequireRepository(t.Context(), "demo", first, "org/repo"); err != nil {
		t.Fatal(err)
	}
	if err = client.RequireRepository(t.Context(), "demo", first, "org/uncovered"); ErrorCategory(err) != ForgePermission {
		t.Fatal("uncovered repository category")
	}
	now = now.Add(55 * time.Minute)
	second, err := client.Token(t.Context(), "demo", app)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || exchanges.Load() != 2 {
		t.Fatal("refresh margin did not mint")
	}
	app.ConnectedAt = now
	if _, err = client.Token(t.Context(), "demo", app); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 3 {
		t.Fatal("replacement did not invalidate cache")
	}
	client.Invalidate("demo")
	if _, err = client.Token(t.Context(), "demo", app); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 4 {
		t.Fatal("disconnect invalidation ignored")
	}
	clean, _, err := redact.Text(t.Context(), nil, private+first+second)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{private, first, second} {
		if strings.Contains(clean, secret) {
			t.Fatal("live secret escaped redaction")
		}
	}
}
func TestAppPermissionAndMalformedResponses(t *testing.T) {
	_, private := appTestKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: "demo", AppID: 41, InstallationID: 12}, PrivateKey: private}
	for _, tc := range []struct {
		name                       string
		readStatus, exchangeStatus int
		readBody                   string
		expiry                     time.Time
		want                       ForgeErrorCategory
	}{
		{"unauthorized", 401, 200, "", now.Add(time.Hour), ForgePermission},
		{"not found", 404, 200, "", now.Add(time.Hour), ForgePermission},
		{"suspended", 200, 200, `{"id":12,"app_id":41,"account":{"login":"org"},"suspended_at":"2026-01-01T00:00:00Z"}`, now.Add(time.Hour), ForgePermission},
		{"foreign app", 200, 200, `{"id":12,"app_id":99,"account":{"login":"org"}}`, now.Add(time.Hour), ForgePermission},
		{"exchange unauthorized", 200, 401, "", now.Add(time.Hour), ForgePermission},
		{"exchange missing", 200, 404, "", now.Add(time.Hour), ForgePermission},
		{"expired", 200, 200, "", now, ForgeResponse},
		{"too long", 200, 200, "", now.Add(2 * time.Hour), ForgeResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/access_tokens") {
					w.WriteHeader(tc.exchangeStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "invalid-candidate-private-token", "expires_at": tc.expiry})
					return
				}
				w.WriteHeader(tc.readStatus)
				body := tc.readBody
				if body == "" {
					body = `{"id":12,"app_id":41,"account":{"login":"org"}}`
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			client := &AppClient{Now: func() time.Time { return now }, Runner: func(token, identity string) AppRunner {
				return newRESTRunner(server.Client(), server.URL, token, identity)
			}}
			_, err := client.Token(t.Context(), "demo", app)
			if ErrorCategory(err) != tc.want {
				t.Fatalf("category %s: %v", ErrorCategory(err), err)
			}
			if tc.want == ForgePermission && (!strings.Contains(err.Error(), "workspace demo") || !strings.Contains(err.Error(), "connect the GitHub App in workspace settings")) {
				t.Fatal("missing remedy")
			}
			if strings.Contains(err.Error(), "invalid-candidate-private-token") || strings.Contains(err.Error(), private) {
				t.Fatal("error contains secret")
			}
		})
	}
	for _, bad := range []core.WorkspaceGitHubAppCredential{{}, {WorkspaceGitHubAppStatus: app.WorkspaceGitHubAppStatus, PrivateKey: "bad"}} {
		if _, err := (&AppClient{}).Token(context.Background(), "demo", bad); ErrorCategory(err) != ForgePermission {
			t.Fatal("missing app/key category")
		}
	}
}
