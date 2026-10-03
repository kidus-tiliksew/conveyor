package main

// Shared local page infrastructure for `conveyor plan ask` and
// `conveyor plan review`. Both commands serve exactly one page on 127.0.0.1,
// print its URL, and block until the owner submits the form or closes the
// command (Ctrl-C). The pages share one html/template, one stylesheet, and one
// script; the item shapes and the output files differ and live with each
// command. No asset is fetched from a CDN: everything is embedded in the
// binary.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed plan_page_assets
var planPageAssets embed.FS

// planPageTemplate is the single page template both commands render.
var planPageTemplate = template.Must(template.ParseFS(planPageAssets, "plan_page_assets/page.html"))

// planPageAssetFS serves the embedded stylesheet and script under /assets/.
func planPageAssetFS() fs.FS {
	sub, err := fs.Sub(planPageAssets, "plan_page_assets")
	if err != nil {
		panic(fmt.Sprintf("plan page assets: %v", err))
	}
	return sub
}

// planPageMode selects the review or the ask page shape in the template.
type planPageMode string

const (
	planPageReview planPageMode = "review"
	planPageAsk    planPageMode = "ask"
)

// planPageView is everything the shared template renders. Review pages carry
// Tabs and Matrix; ask pages carry Questions.
type planPageView struct {
	Mode    planPageMode
	Title   string
	Command string
	Goal    string
	Intro   string
	Summary string
	Error   string
	// Token is the per-server anti-CSRF token every rendered form carries; the
	// submit handler rejects a form without it.
	Token  string
	Round  int
	Tabs   []planPageTab
	Matrix *planTraceMatrix
	// Questions is the ask shape: one card per grill question.
	Questions []planPageCard
}

// planPageTab is one review tab: a kind's items with its own approved count.
type planPageTab struct {
	ID       string
	Label    string
	Total    int
	Approved int
	Items    []planPageCard
}

// planPageCard is one reviewable item or one grill question. Index is the
// form-field position on the page.
type planPageCard struct {
	Index int
	ID    string
	File  string
	Hash  string

	Headline  string
	Normative string
	Example   string
	Why       string
	Links     []string

	// Review state.
	Verdict       string
	Comment       string
	Before        string
	Changed       bool
	LinkedChanged bool

	// Ask state.
	Prompt      string
	Options     []planPageOption
	Recommended string
	Answer      string
}

// planPageOption is one selectable answer on an ask card.
type planPageOption struct {
	Text        string
	Recommended bool
	Selected    bool
}

// planTraceMatrix is the requirement-by-task traceability table.
type planTraceMatrix struct {
	Columns []string
	Rows    []planTraceRow
}

// planTraceRow is one requirement row; Cells align with Columns.
type planTraceRow struct {
	Label string
	Cells []bool
}

// planPageServer is the one local HTTP server a plan page runs on.
type planPageServer struct {
	listener net.Listener
	http     *http.Server
	// addr is the listener's host:port; the printed URL, the CSRF Origin and
	// Host checks, and the DNS-rebinding guard all compare against it.
	addr string
	// token gates the page and its submit: it is in the printed URL and in a
	// hidden field of every rendered form.
	token string
	url   string
	done  chan struct{}
	// closeOnce makes finish safe to call from the submit handlers and the
	// serveErr branch of runPlanPage at the same time.
	closeOnce sync.Once
	// submitMu serializes submits so one never reads an output file another
	// is writing; submitted records that a submit was written.
	submitMu  sync.Mutex
	submitted bool
}

// newPlanPageServer binds 127.0.0.1:port; port 0 picks a free port. The
// listener is bound before the caller prints the URL, so the printed URL is
// already reachable.
func newPlanPageServer(port int) (*planPageServer, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid --port %d: must be between 0 and 65535", port)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("plan page: listen on 127.0.0.1:%d: %w", port, err)
	}
	token, err := newPlanPageToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	addr := listener.Addr().String()
	server := &planPageServer{
		listener: listener,
		addr:     addr,
		token:    token,
		url:      "http://" + addr + "/?t=" + token,
		done:     make(chan struct{}),
	}
	server.http = &http.Server{Handler: http.NotFoundHandler()}
	return server, nil
}

// newPlanPageToken returns the random per-server token that gates the page.
func newPlanPageToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("plan page token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// URL is the page's address, valid once newPlanPageServer returns.
func (s *planPageServer) URL() string { return s.url }

// finish records that the page's work is done and the command may return. It
// is safe to call concurrently.
func (s *planPageServer) finish() { s.closeOnce.Do(func() { close(s.done) }) }

// validToken reports whether presented equals the server's token, compared in
// constant time.
func (s *planPageServer) validToken(presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// beginSubmit serializes the page's submissions. It returns false once a
// submit has been recorded; otherwise the caller holds the submit lock and
// must call endSubmit, passing whether it wrote its output file. A failed
// write leaves the page open for a retry.
func (s *planPageServer) beginSubmit() bool {
	s.submitMu.Lock()
	if s.submitted {
		s.submitMu.Unlock()
		return false
	}
	return true
}

func (s *planPageServer) endSubmit(recorded bool) {
	if recorded {
		s.submitted = true
	}
	s.submitMu.Unlock()
}

// checkRequestOrigin rejects a request whose Host is not this server's address
// (DNS rebinding) or whose Origin is present and is not this server's origin
// (cross-site form posts).
func (s *planPageServer) checkRequestOrigin(r *http.Request) error {
	if r.Host != s.addr {
		return fmt.Errorf("unexpected host %q", r.Host)
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+s.addr {
		return fmt.Errorf("unexpected origin %q", origin)
	}
	return nil
}

// registerPlanPageRoutes wires the index page, the submit handler, and the
// embedded assets onto the server's mux. Both the index and the submit require
// the server's token; the index carries it in the query and a submit carries
// it in the form.
func registerPlanPageRoutes(mux *http.ServeMux, server *planPageServer, index, submit http.HandlerFunc) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := server.checkRequestOrigin(r); err != nil {
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}
		if !server.validToken(r.URL.Query().Get("t")) {
			http.Error(w, "forbidden: missing or invalid token", http.StatusForbidden)
			return
		}
		index(w, r)
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := server.checkRequestOrigin(r); err != nil {
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !server.validToken(r.PostFormValue("token")) {
			http.Error(w, "forbidden: missing or invalid token", http.StatusForbidden)
			return
		}
		submit(w, r)
	})
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServerFS(planPageAssetFS())))
}

// runPlanPage prints the URL and blocks until the page finishes, the context is
// cancelled (Ctrl-C), or the server stops. It returns a non-nil error only when
// the server failed for an unexpected reason.
func runPlanPage(ctx context.Context, server *planPageServer, stdout io.Writer) error {
	fmt.Fprintf(stdout, "plan page: %s\n", server.URL())
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.http.Serve(server.listener) }()
	select {
	case <-server.done:
	case <-ctx.Done():
	case err := <-serveErr:
		server.finish()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.http.Shutdown(shutdownCtx)
	return nil
}

// renderPlanPage writes a page view with a 200 status.
func renderPlanPage(w http.ResponseWriter, view planPageView) {
	renderPlanPageStatus(w, http.StatusOK, view)
}

// renderPlanPageStatus writes a page view with an explicit status. A render
// failure is reported to the caller; the page is small and the template is
// static, so a failure is a bug.
func renderPlanPageStatus(w http.ResponseWriter, status int, view planPageView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := planPageTemplate.Execute(w, view); err != nil {
		http.Error(w, "render plan page: "+err.Error(), http.StatusInternalServerError)
	}
}

// planRequirementDocID returns the requirement document ID an item ID belongs
// to: "req-alpha/REQ-1" -> "req-alpha". An item ID without a slash is itself.
func planRequirementDocID(itemID string) string {
	if doc, _, ok := strings.Cut(itemID, "/"); ok {
		return doc
	}
	return itemID
}
