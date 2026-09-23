package verification

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OperationChannel is a short-lived loopback transport. Its nonce admits a
// local client, never an authenticated operator (VK-4.1, VK-9).
type OperationChannel struct {
	URL, Nonce string
	server     *http.Server
}

func NewOperationChannel(ctx context.Context, origins []string, relay func(context.Context, json.RawMessage) (any, error)) (*OperationChannel, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		listener.Close()
		return nil, err
	}
	c := &OperationChannel{URL: "http://" + listener.Addr().String(), Nonce: hex.EncodeToString(nonce)}
	allowed := map[string]bool{c.URL: true}
	for _, origin := range origins {
		u, e := url.Parse(origin)
		if e != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			listener.Close()
			return nil, fmt.Errorf("kit UI requires a loopback origin")
		}
		allowed[origin] = true
	}
	c.server = &http.Server{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	c.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != listener.Addr().String() || r.URL.Path != "/operations" || r.URL.RawQuery != "" {
			http.Error(w, "invalid local endpoint", 403)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && !allowed[origin] {
			http.Error(w, "origin refused", 403)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Conveyor-Kit-Nonce")
			w.WriteHeader(204)
			return
		}
		if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Conveyor-Kit-Nonce")), []byte(c.Nonce)) != 1 {
			http.Error(w, "local session refused", 403)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			http.Error(w, "invalid operation", 400)
			return
		}
		result, err := relay(r.Context(), data)
		if err != nil {
			http.Error(w, "operation was not acknowledged; reconcile before retrying", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	go func() { _ = c.server.Serve(listener) }()
	return c, nil
}
func (c *OperationChannel) Close() error { return c.server.Close() }

// ResolvePermissionPath checks the nearest existing ancestor for new outputs;
// lexical containment alone does not detect symlink escapes.
func ResolvePermissionPath(root, relative string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute exercise path refused")
	}
	target := filepath.Join(root, relative)
	ancestor := target
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(ancestor)
		if e == nil {
			target = filepath.Join(append([]string{resolved}, suffix...)...)
			break
		}
		if !os.IsNotExist(e) || ancestor == filepath.Dir(ancestor) {
			return "", e
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = filepath.Dir(ancestor)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("exercise path escapes root")
	}
	return target, nil
}

// EvidenceSpool retains only already sanitized envelopes, outside tracked
// inputs. A write barrier runs before every new file and every upload.
type EvidenceSpool struct {
	Directory string
	Limit     int64
	Check     func(context.Context) error
	mu        sync.Mutex
}

func (s *EvidenceSpool) Put(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirectory(); err != nil {
		return err
	}
	if !VerificationBindingName(key) {
		return fmt.Errorf("invalid spool identity")
	}
	if err := s.Check(ctx); err != nil {
		return err
	}
	path := filepath.Join(s.Directory, key+".json")
	if old, err := s.read(path); err == nil {
		if string(old) != string(data) {
			return fmt.Errorf("spool identity conflict")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(s.Directory)
	if err != nil {
		return err
	}
	var used int64
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("spool symlink refused")
		}
		i, err := e.Info()
		if err != nil {
			return err
		}
		used += i.Size()
	}
	if used+int64(len(data)) > s.Limit {
		return fmt.Errorf("evidence spool limit reached")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
func (s *EvidenceSpool) Flush(ctx context.Context, upload func(context.Context, []byte) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirectory(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.Directory)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".json") {
			return fmt.Errorf("invalid spool entry")
		}
		if err := s.Check(ctx); err != nil {
			return err
		}
		p := filepath.Join(s.Directory, e.Name())
		b, err := s.read(p)
		if err != nil {
			return err
		}
		if err = upload(ctx, b); err != nil {
			return err
		}
		if err = s.Check(ctx); err != nil {
			return err
		}
		if err = os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *EvidenceSpool) read(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.Limit {
		return nil, fmt.Errorf("invalid or oversized spool entry")
	}
	b, err := io.ReadAll(io.LimitReader(f, s.Limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > s.Limit {
		return nil, fmt.Errorf("spool entry exceeds limit")
	}
	return b, nil
}

func (s *EvidenceSpool) checkDirectory() error {
	absolute, err := filepath.Abs(s.Directory)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	if absolute != resolved {
		return fmt.Errorf("spool directory symlink refused")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("spool directory must be private")
	}
	return nil
}
