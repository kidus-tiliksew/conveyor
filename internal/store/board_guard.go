package store

import (
	"context"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

type BoardOptions struct {
	NextAction NextActionOptions
}

type BoardItem struct {
	ID         string
	NextAction nextaction.Compact
}

type Board struct {
	Items []BoardItem
}

func ReadBoard(context.Context, Store, BoardOptions) (Board, error) {
	return Board{}, errors.New("board read not implemented")
}
