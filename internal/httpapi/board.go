package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type boardResponse struct {
	Items  []store.BoardItem `json:"items"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func (s *Server) readBoardPage(ctx context.Context, limit, offset int) (boardResponse, []byte, error) {
	limit = min(limit, 100)
	opts, err := s.nextActionOptionsFor(ctx)
	if err != nil {
		return boardResponse{}, nil, err
	}
	board, err := store.ReadBoard(ctx, s.Store, store.BoardOptions{Limit: limit, Offset: offset, NextAction: opts})
	if err != nil {
		return boardResponse{}, nil, err
	}
	page := boardResponse{Items: board.Items, Total: board.Total, Limit: limit, Offset: offset}
	if err := s.nameTaskCrates(ctx, &page); err != nil {
		return boardResponse{}, nil, err
	}
	for {
		body, err := json.Marshal(page)
		if err != nil {
			return boardResponse{}, nil, err
		}
		if len(body) < 32768 {
			return page, body, nil
		}
		if len(page.Items) <= 1 {
			return boardResponse{}, nil, fmt.Errorf("board line exceeds response budget")
		}
		page.Items = page.Items[:len(page.Items)-1]
	}
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) {
	page, ok := readListPage(w, r, 100)
	if !ok {
		return
	}
	result, body, err := s.readBoardPage(r.Context(), page.Limit, page.Offset)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	page.Limit = result.Limit
	page.writeHeaders(w, result.Total)
	tag := fmt.Sprintf("\"%x\"", sha256.Sum256(body))
	w.Header().Set("ETag", tag)
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == tag || strings.TrimSpace(candidate) == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
