package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Remote is the app-plane directory: it speaks the admin plane's token-gated
// identity door. The token is the in-cluster identity secret (LUMEN_IDENTITY_
// TOKEN); it is not a user session and never reaches a browser.
type Remote struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewRemote binds the HTTP directory. The timeout is deliberately short: the
// admin plane is in-cluster, and a slow identity lookup must not hold a task
// request open.
func NewRemote(baseURL, token string) *Remote {
	return &Remote{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

type personJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (p personJSON) person() Person {
	return Person{ID: p.ID, Username: p.Username, Name: p.Name}
}

func (r *Remote) IDByUsername(ctx context.Context, username string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := r.do(ctx, http.MethodGet,
		"/admin/identity/by-username/"+url.PathEscape(username), nil, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", ErrNotFound
	}
	return out.ID, nil
}

func (r *Remote) PeopleByIDs(ctx context.Context, ids []string) (map[string]Person, error) {
	if len(ids) == 0 {
		return map[string]Person{}, nil
	}
	var out struct {
		Users []personJSON `json:"users"`
	}
	err := r.do(ctx, http.MethodPost, "/admin/identity/users",
		map[string]any{"ids": ids}, &out)
	if err != nil {
		return nil, err
	}
	people := make(map[string]Person, len(out.Users))
	for _, row := range out.Users {
		people[row.ID] = row.person()
	}
	return people, nil
}

func (r *Remote) SearchPeople(ctx context.Context, query string, limit int) ([]Person, error) {
	if limit <= 0 {
		limit = 8
	}
	path := fmt.Sprintf("/admin/identity/people?q=%s&limit=%d",
		url.QueryEscape(query), limit)
	var out struct {
		People []personJSON `json:"people"`
	}
	if err := r.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	people := make([]Person, len(out.People))
	for index, row := range out.People {
		people[index] = row.person()
	}
	return people, nil
}

func (r *Remote) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encode: %v", ErrUnavailable, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("%w: request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read: %v", ErrUnavailable, err)
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return fmt.Errorf("%w: status %d: %s", ErrUnavailable, response.StatusCode,
			strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(out); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	return nil
}

// interface assertion: the compiler keeps the adapters honest.
var _ Directory = (*Remote)(nil)
var _ Directory = (*Local)(nil)
