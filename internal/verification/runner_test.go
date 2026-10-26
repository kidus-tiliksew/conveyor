package verification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestKitOperationChannelAcknowledgementAndOrigin(t *testing.T) {
	var count atomic.Int32
	channel, err := NewOperationChannel(t.Context(), []string{"http://127.0.0.1:8765"}, func(context.Context, json.RawMessage) (any, error) {
		count.Add(1)
		return map[string]string{"state": "durable"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	for _, tc := range []struct {
		nonce, origin string
		status        int
	}{{"wrong", "", 403}, {channel.Nonce, "https://evil.test", 403}, {channel.Nonce, "null", 403}, {channel.Nonce, "http://127.0.0.1:8765", 200}, {channel.Nonce, "", 200}} {
		req, _ := http.NewRequest("POST", channel.URL+"/operations", strings.NewReader(`{}`))
		req.Header.Set("X-Conveyor-Kit-Nonce", tc.nonce)
		req.Header.Set("Origin", tc.origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		if tc.status == 200 && !strings.Contains(string(body), "durable") {
			t.Fatal("response preceded acknowledgement")
		}
	}
	if count.Load() != 2 {
		t.Fatalf("unauthorized relay count %d", count.Load())
	}
	if _, err = NewOperationChannel(t.Context(), []string{"http://0.0.0.0:80"}, nil); err == nil {
		t.Fatal("non-loopback UI admitted")
	}
}

func TestKitEvidenceSpoolRetentionAndLoss(t *testing.T) {
	lost := false
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := EvidenceSpool{Directory: dir, Limit: 32, Check: func(context.Context) error {
		if lost {
			return errors.New("lost")
		}
		return nil
	}}
	if err := s.Put(t.Context(), "one", []byte(`{"safe":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(t.Context(), func(context.Context, []byte) error { return errors.New("offline") }); err == nil {
		t.Fatal("offline upload succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "one.json")); err != nil {
		t.Fatal("offline spool removed")
	}
	lost = true
	if err := s.Put(t.Context(), "two", []byte(`{}`)); err == nil {
		t.Fatal("write after loss")
	}
	if err := s.Flush(t.Context(), func(context.Context, []byte) error { t.Fatal("upload after loss"); return nil }); err == nil {
		t.Fatal("flush after loss")
	}
	lost = false
	if err := s.Flush(t.Context(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("acknowledged spool retained")
	}
}

func TestKitSpoolRejectsSymlinkDirectory(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "spool")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	spool := EvidenceSpool{Directory: link, Limit: 1024, Check: func(context.Context) error { return nil }}
	if err := spool.Put(t.Context(), "one", []byte(`{}`)); err == nil {
		t.Fatal("spool followed symlink")
	}
	if err := spool.Flush(t.Context(), func(context.Context, []byte) error { t.Fatal("uploaded through symlink"); return nil }); err == nil {
		t.Fatal("spool read symlink directory")
	}
}

// Upload holds the spool lock only around its file reads and removal, so a
// relay is acknowledged while a slow upload is in flight
// (component-verification-runner).
func TestKitEvidenceSpoolUploadIsConcurrentAndBarriered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var lost atomic.Bool
	s := &EvidenceSpool{Directory: dir, Limit: 1024, Check: func(context.Context) error {
		if lost.Load() {
			return errors.New("authority lost")
		}
		return nil
	}}
	if err := s.Put(t.Context(), "first", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	inFlight, release := make(chan []byte, 1), make(chan error)
	result := make(chan error, 1)
	go func() {
		result <- s.Upload(t.Context(), "first", func(_ context.Context, b []byte) error {
			inFlight <- b
			return <-release
		})
	}()
	if got := <-inFlight; string(got) != `{"n":1}` {
		t.Fatalf("uploaded %q", got)
	}
	if err := s.Put(t.Context(), "second", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("Put blocked or refused during an in-flight upload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "first.json")); err != nil {
		t.Fatal("entry removed before its upload succeeded")
	}
	release <- nil
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "first.json")); !os.IsNotExist(err) {
		t.Fatal("uploaded entry retained")
	}
	if err := s.Upload(t.Context(), "second", func(context.Context, []byte) error { return errors.New("unavailable") }); err == nil {
		t.Fatal("failed upload reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, "second.json")); err != nil {
		t.Fatal("entry removed after a failed upload")
	}
	lost.Store(true)
	uploaded := false
	if err := s.Upload(t.Context(), "second", func(context.Context, []byte) error { uploaded = true; return nil }); err == nil || uploaded {
		t.Fatalf("upload after authority loss: err=%v uploaded=%v", err, uploaded)
	}
	lost.Store(false)
	go func() {
		result <- s.Upload(t.Context(), "second", func(context.Context, []byte) error {
			lost.Store(true)
			return nil
		})
	}()
	if err := <-result; err == nil {
		t.Fatal("removal after authority loss reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, "second.json")); err != nil {
		t.Fatal("entry removed after authority loss")
	}
}
