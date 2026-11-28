package identity

import (
	"context"
	"errors"

	"github.com/kanatabi/lumen/internal/store"
)

// Local is the in-process directory: the legacy role=all dev plane (and
// tests) read the admin store directly. It exists so the tasks module never
// imports the store itself — the dependency is an adapter decision.
type Local struct {
	pg *store.PG
}

// NewLocal binds the directory to the admin store.
func NewLocal(pg *store.PG) *Local { return &Local{pg: pg} }

func (l *Local) IDByUsername(ctx context.Context, username string) (string, error) {
	id, err := l.pg.UserIDByUsername(ctx, username)
	if errors.Is(err, store.ErrNoSuchUser) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	return id, nil
}

func (l *Local) PeopleByIDs(ctx context.Context, ids []string) (map[string]Person, error) {
	rows, err := l.pg.PeopleByIDs(ctx, ids)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make(map[string]Person, len(rows))
	for _, row := range rows {
		out[row.ID] = Person{ID: row.ID, Username: row.Username, Name: row.Name}
	}
	return out, nil
}

func (l *Local) SearchPeople(ctx context.Context, query string, limit int) ([]Person, error) {
	rows, err := l.pg.SearchPeople(ctx, query, limit)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make([]Person, len(rows))
	for index, row := range rows {
		out[index] = Person{ID: row.ID, Username: row.Username, Name: row.Name}
	}
	return out, nil
}
