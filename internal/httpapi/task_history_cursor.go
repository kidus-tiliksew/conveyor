package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task history cursors are stateless: base64url JSON with an HMAC-SHA256 tag
// under a key drawn once per process and named by a random key ID. A cursor
// binds everything its next page depends on, so a replica or a restarted
// process refuses it with a restart instead of misreading it (task
// 261015-3721c6; component-http-api "Task history pages").
const (
	taskHistoryCursorVersion  = 1
	taskHistoryCursorMaxBytes = 1024
	taskHistoryCursorTTL      = 15 * time.Minute
	taskHistoryMaxLimit       = 50

	taskHistoryOlder = "older"
	taskHistoryNewer = "newer"

	taskHistoryTechnical = "technical"
	taskHistoryHuman     = "human"
)

var (
	// errTaskHistoryCursorInvalid is a malformed, tampered, or foreign cursor.
	errTaskHistoryCursorInvalid = errors.New("history cursor is invalid")
	// errTaskHistoryRestart is an expired cursor or one signed under another
	// process's key; the client starts again from the newest page.
	errTaskHistoryRestart = errors.New("history cursor expired: restart from the newest page")
)

// taskHistoryCursorCodec signs and verifies cursors. now is injectable so
// tests can expire a cursor without sleeping.
type taskHistoryCursorCodec struct {
	key   []byte
	keyID string
	now   func() time.Time
}

func newTaskHistoryCursorCodec() (*taskHistoryCursorCodec, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return &taskHistoryCursorCodec{key: key, keyID: hex.EncodeToString(id), now: time.Now}, nil
}

// historyCursorCodec draws the process key once, on first use, unless a test
// installed its own codec first.
func (s *Server) historyCursorCodec() (*taskHistoryCursorCodec, error) {
	s.historyCursorsOnce.Do(func() {
		if s.historyCursors == nil {
			s.historyCursors, s.historyCursorsErr = newTaskHistoryCursorCodec()
		}
	})
	return s.historyCursors, s.historyCursorsErr
}

// taskHistoryCursor is the signed cursor body. Short keys keep the encoded
// cursor well inside 1,024 bytes.
type taskHistoryCursor struct {
	Version   int    `json:"v"`
	KeyID     string `json:"k"`
	Workspace string `json:"w"`
	TaskID    string `json:"t"`
	Selection string `json:"s"`
	// Credential is the base64url SHA-256 of the authenticated credential ID.
	Credential string `json:"c"`
	MaxID      string `json:"m"`
	Count      int    `json:"n"`
	// AtMicros and EventID are the seek tuple: UTC Unix microseconds and a
	// canonical decimal event ID.
	AtMicros  int64  `json:"a"`
	EventID   string `json:"i"`
	Direction string `json:"d"`
	Limit     int    `json:"l"`
	// Expires is UTC Unix microseconds, 15 minutes after the first page.
	Expires int64 `json:"e"`
}

// taskHistoryBinding is what a cursor must match on the request presenting it.
type taskHistoryBinding struct {
	workspace, taskID, credential string
}

func credentialHash(credentialID string) string {
	sum := sha256.Sum256([]byte(credentialID))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (c *taskHistoryCursorCodec) tag(body []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write(body)
	return mac.Sum(nil)
}

// issue signs a cursor for the page continuing from seek in direction.
func (c *taskHistoryCursorCodec) issue(binding taskHistoryBinding, selection string, boundary store.TaskEventBoundary, seek store.TaskEventPosition, direction string, limit int, expires time.Time) (string, error) {
	body, err := json.Marshal(taskHistoryCursor{
		Version: taskHistoryCursorVersion, KeyID: c.keyID, Workspace: binding.workspace, TaskID: binding.taskID,
		Selection: selection, Credential: credentialHash(binding.credential),
		MaxID: strconv.FormatInt(boundary.MaxID, 10), Count: boundary.Count,
		AtMicros: seek.At.UnixMicro(), EventID: strconv.FormatInt(seek.ID, 10),
		Direction: direction, Limit: limit, Expires: expires.UnixMicro(),
	})
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(c.tag(body))
	if len(token) > taskHistoryCursorMaxBytes {
		return "", errors.New("history cursor exceeds 1024 bytes")
	}
	return token, nil
}

// decode verifies a cursor against the presenting request. The selection is
// checked by the caller, which may adopt the cursor's selection.
func (c *taskHistoryCursorCodec) decode(token string, binding taskHistoryBinding) (taskHistoryCursor, store.TaskEventBoundary, store.TaskEventPosition, error) {
	var cursor taskHistoryCursor
	fail := func(err error) (taskHistoryCursor, store.TaskEventBoundary, store.TaskEventPosition, error) {
		return taskHistoryCursor{}, store.TaskEventBoundary{}, store.TaskEventPosition{}, err
	}
	encodedBody, encodedTag, found := strings.Cut(token, ".")
	if len(token) > taskHistoryCursorMaxBytes || !found {
		return fail(errTaskHistoryCursorInvalid)
	}
	body, bodyErr := base64.RawURLEncoding.DecodeString(encodedBody)
	tag, tagErr := base64.RawURLEncoding.DecodeString(encodedTag)
	if bodyErr != nil || tagErr != nil {
		return fail(errTaskHistoryCursorInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || decoder.More() || cursor.Version != taskHistoryCursorVersion {
		return fail(errTaskHistoryCursorInvalid)
	}
	if cursor.KeyID != c.keyID {
		return fail(errTaskHistoryRestart)
	}
	if !hmac.Equal(tag, c.tag(body)) {
		return fail(errTaskHistoryCursorInvalid)
	}
	if cursor.Workspace != binding.workspace || cursor.TaskID != binding.taskID || cursor.Credential != credentialHash(binding.credential) {
		return fail(errTaskHistoryCursorInvalid)
	}
	maxID, maxErr := parseCanonicalEventID(cursor.MaxID)
	eventID, idErr := parseCanonicalEventID(cursor.EventID)
	if maxErr != nil || idErr != nil || cursor.Count < 0 || cursor.Limit < 1 || cursor.Limit > taskHistoryMaxLimit ||
		cursor.Direction != taskHistoryOlder && cursor.Direction != taskHistoryNewer ||
		cursor.Selection != taskHistoryTechnical && cursor.Selection != taskHistoryHuman {
		return fail(errTaskHistoryCursorInvalid)
	}
	if !c.now().Before(timeFromMicros(cursor.Expires)) {
		return fail(errTaskHistoryRestart)
	}
	return cursor, store.TaskEventBoundary{MaxID: maxID, Count: cursor.Count},
		store.TaskEventPosition{At: timeFromMicros(cursor.AtMicros), ID: eventID}, nil
}

func timeFromMicros(micros int64) time.Time { return time.UnixMicro(micros).UTC() }

func parseCanonicalEventID(text string) (int64, error) {
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != text {
		return 0, errTaskHistoryCursorInvalid
	}
	return id, nil
}
