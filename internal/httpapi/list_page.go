package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Default pages for list routes whose rows grow with workspace history
// (DEC-105(4)). A document or membership list passes no default.
const (
	// defaultInboxPageLimit is the default page of the operator inbox and its
	// act log.
	defaultInboxPageLimit = 50
	// defaultHistoryPageLimit is the default page of a task's jobs and events.
	defaultHistoryPageLimit = 100
)

// listPage is one list route's parsed page parameters. Paged is false only for
// a route with no default page when the request names no limit, and such a
// route reads its whole list.
type listPage struct {
	Limit  int
	Offset int
	Paged  bool
}

// parseListPage reads the shared limit and offset parameters. limit appears
// once and runs from 1 to 200, and offset requires limit and runs from 0 to
// 2147483647. A violation returns an error that the route answers with 400. A
// positive defaultLimit is the route's default page; zero leaves a request
// without limit unpaged (DEC-105(4); component-http-api invariant 19).
func parseListPage(values url.Values, defaultLimit int) (listPage, error) {
	limitValues, limited := values["limit"]
	offsetValues, hasOffset := values["offset"]
	if len(offsetValues) > 1 {
		return listPage{}, fmt.Errorf("offset must be supplied once")
	}
	if !limited {
		if hasOffset {
			return listPage{}, fmt.Errorf("offset requires limit")
		}
		return listPage{Limit: defaultLimit, Paged: defaultLimit > 0}, nil
	}
	if len(limitValues) != 1 {
		return listPage{}, fmt.Errorf("limit must be supplied once")
	}
	limit, err := strconv.Atoi(limitValues[0])
	if err != nil || limit < 1 || limit > store.MaxTaskOperationsLimit {
		return listPage{}, fmt.Errorf("limit must be between 1 and %d", store.MaxTaskOperationsLimit)
	}
	offset := 0
	if hasOffset {
		// Bound the offset here, before it reaches a store: past
		// store.MaxTaskOperationsOffset the two stores disagree, because
		// Postgres narrows the bind parameter to int32.
		offset, err = strconv.Atoi(offsetValues[0])
		if err != nil || offset < 0 || offset > store.MaxTaskOperationsOffset {
			return listPage{}, fmt.Errorf("offset must be between 0 and %d", store.MaxTaskOperationsOffset)
		}
	}
	return listPage{Limit: limit, Offset: offset, Paged: true}, nil
}

// readListPage parses the request's page parameters and answers a violation
// with 400. ok is false after it has answered.
func readListPage(w http.ResponseWriter, r *http.Request, defaultLimit int) (page listPage, ok bool) {
	page, err := parseListPage(r.URL.Query(), defaultLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return listPage{}, false
	}
	return page, true
}

// writeHeaders sets the three paging headers of a paged response. An unpaged
// response carries none.
func (p listPage) writeHeaders(w http.ResponseWriter, total int) {
	if !p.Paged {
		return
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(p.Limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(p.Offset))
}

// query is the page as the store reads it.
func (p listPage) query() store.PageQuery {
	return store.PageQuery{Limit: p.Limit, Offset: p.Offset}
}
