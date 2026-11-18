package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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
// the body projected now, and only when the validation read and every
// merge-readiness value the projection peeked are unchanged at answer time.
// A read whose inputs moved projects again; after statementReadAttempts it
// answers 200 with no ETag (component-next-action invariant 58).
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
		if after != validation || !reads.unchanged(&s.mergeReadiness) {
			if attempt < statementReadAttempts {
				continue
			}
			// The inputs kept moving: this body is answered once, uncovered,
			// and no tag certifies it.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encoded.Bytes())
			return
		}
		tag := statementConditional(now, opts, scope, validation, encoded.Bytes(), reads).etag(horizon.until, "")
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded.Bytes())
		return
	}
}
