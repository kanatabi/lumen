package notifications

// remote.go — the app-plane sender. Tasks runs in lumen-staging/
// lumen-production (its own DB), while the shared inbox lives on the admin
// plane; this client speaks the existing token-gated SEND door with the
// recipient's users.id (`toIds`) and the deployment's env.
//
// It implements the tasks module's Notifier seam without importing tasks:
// the interface is structural (Send with this package's SendParams), and the
// best-effort semantics stay with the caller.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RemoteNotifier posts each recipient's notification to the admin send door.
// Env is the deployment tenant (staging|production); a per-param Env wins.
type RemoteNotifier struct {
	baseURL string
	token   string
	env     string
	client  *http.Client
}

// NewRemoteNotifier binds the door client. An empty token means the door is
// not configured on this plane; every send fails and the caller logs it.
func NewRemoteNotifier(baseURL, token, env string) *RemoteNotifier {
	return &RemoteNotifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		env:     env,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

type sendWire struct {
	App       string          `json:"app"`
	ToIDs     []string        `json:"toIds"`
	Env       string          `json:"env,omitempty"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	ExpiresAt *string         `json:"expiresAt,omitempty"`
}

// Send delivers the batch one event at a time. The tasks module's batches are
// single-recipient by construction (an invite or an assignment), so the loop
// is one request in practice; it keeps the interface honest for a future
// fan-out.
func (r *RemoteNotifier) Send(ctx context.Context, batch []SendParams) (int, error) {
	sent := 0
	for _, params := range batch {
		env := params.Env
		if env == "" {
			env = r.env
		}
		wire := sendWire{
			App:     params.App,
			ToIDs:   []string{params.UserID},
			Env:     env,
			Type:    params.Type,
			Title:   params.Title,
			Body:    params.Body,
			Payload: params.Payload,
		}
		if params.ExpiresAt != nil {
			expiresAt := params.ExpiresAt.UTC().Format(time.RFC3339)
			wire.ExpiresAt = &expiresAt
		}
		body, err := json.Marshal(wire)
		if err != nil {
			return sent, fmt.Errorf("notifications: encode: %w", err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			r.baseURL+"/admin/notifications", bytes.NewReader(body))
		if err != nil {
			return sent, fmt.Errorf("notifications: request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+r.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := r.client.Do(request)
		if err != nil {
			return sent, fmt.Errorf("notifications: send: %w", err)
		}
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return sent, fmt.Errorf("notifications: status %d: %s",
				response.StatusCode, strings.TrimSpace(string(payload)))
		}
		sent++
	}
	return sent, nil
}
