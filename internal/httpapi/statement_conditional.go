package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Conditional statement reads (component-next-action invariant 58). A tag
// covers every input of its response: the route, workspace, caller,
// capabilities, and task or query in its scope; the recorded events,
// pending document proposals, passed deadlines, and membership in the
// validation read; and the encoded body, which reflects every other input
// the statements read, merge readiness and configuration included. Its
// expiry is no later than the next UTC minute, any stated deadline, the
// review lease horizon, or the expiry of a merge-readiness value it read.

// statementReadAttempts bounds how many times a per-task statement read
// projects again when its inputs moved under it. The last attempt that still
// sees movement answers 200 with no ETag.
const statementReadAttempts = 3

// statementConditional is the validator state of one statement response:
// the activity validator over its scope and validation read, plus the digest
// of the encoded body it answers.
func statementConditional(now time.Time, opts store.NextActionOptions, scope []byte, validation store.ActivityValidation, body []byte, reads *readinessReads) *activityConditional {
	hash := sha256.New()
	hash.Write(body)
	hash.Write([]byte{0})
	hash.Write(reads.digest())
	return &activityConditional{now: now, opts: opts, scope: scope, validation: validation, covered: hash.Sum(nil)}
}

var timeType = reflect.TypeFor[time.Time]()

// orderHorizon bounds a statement read by every work-order time after the
// read's instant. The validation read counts each work-order time once it
// passes, and a passed time can change a statement, so a body read before
// one is not reused past it (component-next-action invariant 58).
func orderHorizon(ctx context.Context, orders []core.WorkOrder) {
	for i := range orders {
		value := reflect.ValueOf(orders[i])
		for j := range value.NumField() {
			field := value.Field(j)
			if !field.CanInterface() {
				continue
			}
			switch {
			case field.Type() == timeType:
				memoHorizon(ctx, field.Interface().(time.Time))
			case field.Type() == reflect.PointerTo(timeType) && !field.IsNil():
				memoHorizon(ctx, *field.Interface().(*time.Time))
			}
		}
	}
}

// statementNotFound marks the task read's not-found, which answers 404.
type statementNotFound struct{ err error }

func (e statementNotFound) Error() string { return e.err.Error() }
func (e statementNotFound) Unwrap() error { return e.err }

// answerStatement serves one per-task statement read. It projects first,
// then decides: a 304 answers only when the request's tag equals the tag of
// the body projected now, only when the validation read and every
// merge-readiness value the projection peeked are unchanged at answer time,
// and only when the answer instant is still before the tag's expiry. A
// read whose inputs moved, or whose expiry passed during the projection,
// projects again at a fresh instant; after statementReadAttempts it answers
// 200 with no ETag (component-next-action invariant 58).
func (s *Server) answerStatement(w http.ResponseWriter, r *http.Request, route, taskID string, project func(context.Context, store.NextActionOptions) (any, error)) {
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	scope := taskObservationValidatorScope(r.Context(), route, taskID, opts.Capabilities)
	for attempt := 1; ; attempt++ {
		now := activityClock().UTC().Truncate(time.Microsecond)
		opts.Now = now
		validation, err := s.Store.ReadActivityValidation(r.Context(), now)
		if err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		horizon := &readMemoHorizon{now: now, until: now.Truncate(time.Minute).Add(time.Minute)}
		reads := &readinessReads{}
		ctx := context.WithValue(r.Context(), readMemoHorizonKey{}, horizon)
		ctx = context.WithValue(ctx, readinessReadsKey{}, reads)
		body, err := project(ctx, opts)
		if err != nil {
			var missing statementNotFound
			if errors.As(err, &missing) {
				http.Error(w, "task not found", http.StatusNotFound)
				return
			}
			writeStoreFailure(w, r, err)
			return
		}
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		after, err := s.Store.ReadActivityValidation(r.Context(), now)
		if err != nil {
			writeStoreFailure(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// A review lease, a deadline, a readiness expiry, or the minute
		// boundary that passed while the read ran ends the tag's validity:
		// both validation reads used the instant captured before the
		// projection, so only the answer clock can see it.
		expired := !activityClock().UTC().Before(horizon.until)
		if expired || after != validation || !reads.unchanged(&s.mergeReadiness) {
			if attempt < statementReadAttempts {
				continue
			}
			// The inputs kept moving or the horizon kept passing: this body
			// is answered once, uncovered, and no tag certifies it.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encoded.Bytes())
			return
		}
		tag := statementConditional(now, opts, scope, validation, encoded.Bytes(), reads).etag(horizon.until, "")
		w.Header().Set("ETag", tag)
		if ifNoneMatchAdmits(r.Header.Values("If-None-Match"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded.Bytes())
		return
	}
}

// ifNoneMatchAdmits reports whether an If-None-Match field admits a 304 for
// the current tag: a `*`, or any listed entity tag that weakly matches it
// (RFC 9110, section 13.1.2). Commas inside a quoted tag do not split it.
func ifNoneMatchAdmits(fields []string, tag string) bool {
	if tag == "" {
		return false
	}
	current := strings.TrimPrefix(tag, "W/")
	for _, field := range fields {
		for _, candidate := range splitEntityTags(field) {
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == current {
				return true
			}
		}
	}
	return false
}

// splitEntityTags splits an If-None-Match field value into its members,
// splitting only on commas outside quotes.
func splitEntityTags(field string) []string {
	var members []string
	quoted, start := false, 0
	for i := 0; i < len(field); i++ {
		switch field[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				members = append(members, strings.TrimSpace(field[start:i]))
				start = i + 1
			}
		}
	}
	members = append(members, strings.TrimSpace(field[start:]))
	out := members[:0]
	for _, member := range members {
		if member != "" {
			out = append(out, member)
		}
	}
	return out
}
