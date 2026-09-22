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
