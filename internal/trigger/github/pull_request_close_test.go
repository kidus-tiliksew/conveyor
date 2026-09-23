package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClosePullRequestReconcilesCommentAndPreservesBranch(t *testing.T) {
	for _, lost := range []string{"none", "comment", "close"} {
		t.Run(lost, func(t *testing.T) {
			state := "open"
			comment := ""
			posts, closes := 0, 0
			wanted := "Closed by Conveyor: this task was started over as next (conveyor/task-next). Reason: revised scope"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer installation-secret" {
					t.Errorf("wrong authentication")
					w.WriteHeader(401)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/acme/app/pulls/42":
					json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": state, "merged": false})
				case "GET /repos/acme/app/issues/42/comments":
					if comment == "" {
						fmt.Fprint(w, `[]`)
					} else {
						json.NewEncoder(w).Encode([]any{map[string]any{"id": 9, "body": comment}})
					}
				case "POST /repos/acme/app/issues/42/comments":
					posts++
					var body struct {
						Body string `json:"body"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					comment = body.Body
					if lost == "comment" && posts == 1 {
						http.Error(w, "lost acknowledgement installation-secret", 500)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"id": 9, "body": comment})
				case "PATCH /repos/acme/app/pulls/42":
					closes++
					if comment != wanted {
						t.Error("closed before successor comment")
					}
					var body map[string]string
					json.NewDecoder(r.Body).Decode(&body)
					if len(body) != 1 || body["state"] != "closed" {
						t.Errorf("unexpected close fields: %v", body)
					}
					state = "closed"
					if lost == "close" && closes == 1 {
						http.Error(w, "lost close acknowledgement installation-secret", 500)
						return
					}
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected operation, including branch mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			run := newRESTRunner(server.Client(), server.URL, "installation-secret", "workspace demo GitHub App")
			err := closePullRequest(t.Context(), "acme/app", 42, wanted, run)
			if lost != "none" {
				if err == nil || strings.Contains(err.Error(), "installation-secret") {
					t.Fatalf("unsafe or missing error: %v", err)
				}
				if lost == "close" && !errors.Is(err, ErrMutationUncertain) {
					t.Fatalf("lost close category=%q err=%v", ErrorCategory(err), err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err = closePullRequest(t.Context(), "acme/app", 42, wanted, run); err != nil {
				t.Fatal(err)
			}
			if err = closePullRequest(t.Context(), "acme/app", 42, wanted, run); err != nil {
				t.Fatal(err)
			}
			if posts != 1 || closes != 1 || state != "closed" || comment != wanted {
				t.Fatalf("posts=%d closes=%d state=%s comment=%q", posts, closes, state, comment)
			}
		})
	}
}

func TestClosePullRequestSkipsClosedAndMerged(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprint(merged), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/pulls/42") {
					t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "closed", "merged": merged})
			}))
			defer server.Close()
			if err := closePullRequest(t.Context(), "acme/app", 42, "comment", newRESTRunner(server.Client(), server.URL, "token", "workspace App")); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestClosePullRequestErrorsDoNotLeakToken(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		category ForgeErrorCategory
	}{
		{"permission", 401, "secret-installation", ForgePermission},
		{"status", 500, "secret-installation", ForgeStatus},
		{"rate_limit", 429, "secret-installation", ForgeRateLimited},
		{"response", 200, "secret-installation", ForgeResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			err := closePullRequest(t.Context(), "acme/app", 42, "comment", newRESTRunner(server.Client(), server.URL, "secret-installation", "workspace App"))
			if err == nil || ErrorCategory(err) != tc.category || strings.Contains(err.Error(), "secret-installation") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("transport", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		err := closePullRequest(t.Context(), "acme/app", 42, "comment", newRESTRunner(server.Client(), server.URL, "secret-installation", "workspace App"))
		if err == nil || ErrorCategory(err) != ForgeRequest || strings.Contains(err.Error(), "secret-installation") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("missing_no_fallback", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "ambient-secret")
		err := ClosePullRequestWithCredential(t.Context(), "acme/app", 42, "comment", "")
		if ErrorCategory(err) != ForgePermission || !strings.Contains(err.Error(), "settings") || strings.Contains(err.Error(), "ambient-secret") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestClosePullRequestMutationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uncertain bool
	}{
		{"initial_read", false}, {"comment_read", false}, {"comment_write", false}, {"second_read", false},
		{"patch_400", false}, {"patch_403", false}, {"patch_422", false}, {"patch_429", false},
		{"patch_500", true}, {"patch_transport", true}, {"confirmation_403", true}, {"confirmation_malformed", true}, {"confirmed_open", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, patches := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches++
					switch tc.name {
					case "patch_400":
						w.WriteHeader(400)
						return
					case "patch_403":
						w.WriteHeader(403)
						return
					case "patch_422":
						w.WriteHeader(422)
						return
					case "patch_429":
						w.WriteHeader(429)
						return
					case "patch_500":
						w.WriteHeader(500)
						return
					case "patch_transport":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					fmt.Fprint(w, `{}`)
					return
				}
				if strings.Contains(r.URL.Path, "comments") {
					if tc.name == "comment_read" {
						w.WriteHeader(500)
						return
					}
					if tc.name == "comment_write" {
						if r.Method == http.MethodGet {
							fmt.Fprint(w, `[]`)
						} else {
							w.WriteHeader(500)
						}
						return
					}
					fmt.Fprint(w, `[{"id":1,"body":"comment"}]`)
					return
				}
				reads++
				if reads == 1 && tc.name == "initial_read" || reads == 2 && tc.name == "second_read" {
					w.WriteHeader(500)
					return
				}
				if reads == 3 && tc.name == "confirmation_403" {
					w.WriteHeader(403)
					return
				}
				if reads == 3 && tc.name == "confirmation_malformed" {
					fmt.Fprint(w, `{}`)
					return
				}
				fmt.Fprint(w, `{"number":42,"state":"open","merged":false}`)
			}))
			defer server.Close()
			err := closePullRequest(t.Context(), "acme/app", 42, "comment", newRESTRunner(server.Client(), server.URL, "token", "workspace App"))
			if err == nil || errors.Is(err, ErrMutationUncertain) != tc.uncertain {
				t.Fatalf("uncertain=%v want=%v err=%v", errors.Is(err, ErrMutationUncertain), tc.uncertain, err)
			}
			if tc.uncertain && patches != 1 {
				t.Fatalf("uncertainty without PATCH: %d", patches)
			}
			if (tc.name == "initial_read" || tc.name == "comment_read" || tc.name == "comment_write" || tc.name == "second_read") && patches != 0 {
				t.Fatal("pre-mutation failure sent PATCH")
			}
		})
	}
}
