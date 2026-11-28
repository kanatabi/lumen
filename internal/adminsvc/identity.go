package adminsvc

// identity.go — the service wrappers the admin identity door calls. The
// store owns the SQL; this layer adds nothing (no policy belongs to an
// in-cluster read that the control plane already authorizes by token).

import (
	"context"

	"github.com/kanatabi/lumen/internal/store"
)

func (s *Service) PeopleByIDs(ctx context.Context, ids []string) ([]store.PersonRow, error) {
	return s.store.PeopleByIDs(ctx, ids)
}

func (s *Service) SearchPeople(ctx context.Context, query string, limit int) ([]store.PersonRow, error) {
	return s.store.SearchPeople(ctx, query, limit)
}

func (s *Service) UserIDByUsername(ctx context.Context, username string) (string, error) {
	return s.store.UserIDByUsername(ctx, username)
}
