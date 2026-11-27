package admin

// notifications.go — the estate-generic notifications module's transport
// (M2.A). The shape mirrors webauthn.go: handlers DECODE, CALL the
// module, MAP sentinels, ENCODE. NO business rule lives here beyond the
// wire's own grammar (query parsing); sanitization, slug checks, payload
// URLs, and the rate limit are internal/notifications decisions.
//
// Two audiences share this file:
//   - the bell/panel: four authenticated routes (any user, NO scope —
//     every account owns its inbox);
//   - the SEND door: one PUBLIC route mounted only when the shared token
//     is configured. It speaks usernames because external callers know
//     identities, not UUIDs; the module resolves them.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/httpx"
	"github.com/kanatabi/lumen/internal/notifications"
)

const (
	// notificationPageDefault mirrors the store's default (20 rows); the
	// handler clamps BEFORE asking so the cursor math (`len == limit`)
	// cannot disagree with the store's clamp.
	notificationPageDefault = 20
	notificationPageMax     = 100

	// notificationSendBodyMax caps the door's request body. ONE event is
	// copied to many recipients: 100 usernames plus the 8 KiB body and
	// payload caps fit comfortably under 64 KiB.
	notificationSendBodyMax = 64 << 10
)

// notificationsService is the module surface this package needs. It is
// an interface so the handler tests can prove the sentinel mapping and
// principal scoping without a database, and so the composition root stays
// the only place the real Service is named.
type notificationsService interface {
	SendEnabled() bool
	AuthorizeSend(token string) bool
	Send(ctx context.Context, batch []notifications.SendParams) (int, error)
	UserIDsForUsernames(ctx context.Context, names []string) ([]string, error)
	List(ctx context.Context, userID string, opts notifications.ListOpts) ([]notifications.Notification, error)
	UnreadCount(ctx context.Context, userID string) (int, error)
	MarkRead(ctx context.Context, userID, id string) error
	MarkAllRead(ctx context.Context, userID string) (int, error)
}

// notificationItem is the panel's row shape (§2.6). Payload stays raw so
// the FE renders the stored object verbatim; timestamps are UTC RFC 3339.
type notificationItem struct {
	ID        string          `json:"id"`
	App       string          `json:"app"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   json.RawMessage `json:"payload"`
	ReadAt    *time.Time      `json:"readAt"`
	ExpiresAt time.Time       `json:"expiresAt"`
	CreatedAt time.Time       `json:"createdAt"`
}

// notificationList is GET /admin/notifications — the caller's own page,
// newest first. `before` is the opaque cursor from `nextBefore`; omit it
// for the first page.
func (h *Handler) notificationList(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	query := r.URL.Query()
	unreadOnly := false
	switch raw := query.Get("unread"); raw {
	case "", "false":
	case "true":
		unreadOnly = true
	default:
		httpx.WriteErr(w, http.StatusBadRequest, "unread must be true or false")
		return
	}

	var before time.Time
	if raw := query.Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteErr(w, http.StatusBadRequest, "before must be an RFC 3339 timestamp")
			return
		}
		before = parsed
	}

	limit, err := httpx.QueryInt(r, "limit", notificationPageDefault)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if limit <= 0 {
		limit = notificationPageDefault
	}
	if limit > notificationPageMax {
		limit = notificationPageMax
	}

	items, err := h.notifications.List(r.Context(), principal.ID, notifications.ListOpts{
		UnreadOnly: unreadOnly,
		Before:     before,
		Limit:      limit,
	})
	if err != nil {
		h.notificationFail(w, r, err)
		return
	}

	out := make([]notificationItem, len(items))
	for index, item := range items {
		payload := item.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		out[index] = notificationItem{
			ID:        item.ID,
			App:       item.App,
			Type:      item.Type,
			Title:     item.Title,
			Body:      item.Body,
			Payload:   payload,
			ReadAt:    item.ReadAt,
			ExpiresAt: item.ExpiresAt,
			CreatedAt: item.CreatedAt,
		}
	}

	// The cursor is the last row's exact instant (RFC 3339 with
	// nanoseconds): truncating to seconds would silently skip rows that
	// share that second. Only a FULL page can have more behind it.
	var nextBefore *string
	if len(items) == limit {
		cursor := items[len(items)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
		nextBefore = &cursor
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "nextBefore": nextBefore})
}

// notificationUnreadCount is GET /admin/notifications/unread-count — the
// bell's number. Expired rows never count (the store's TTL filter).
func (h *Handler) notificationUnreadCount(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	count, err := h.notifications.UnreadCount(r.Context(), principal.ID)
	if err != nil {
		h.notificationFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"count": count})
}

// notificationMarkRead is POST /admin/notifications/{id}/read — turn one
// row read. Foreign, expired, unknown, and malformed ids are all the same
// 404 (existence is not a signal).
func (h *Handler) notificationMarkRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.notifications.MarkRead(r.Context(), principal.ID, chi.URLParam(r, "id")); err != nil {
		h.notificationFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notificationMarkAllRead is POST /admin/notifications/read-all — clear
// the caller's unread set, reporting how many rows moved (a second call
// is honestly 0).
func (h *Handler) notificationMarkAllRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	updated, err := h.notifications.MarkAllRead(r.Context(), principal.ID)
	if err != nil {
		h.notificationFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// notificationSend is POST /admin/notifications — the SEND door. PUBLIC
// mount (in-cluster only; the admin plane has no ingress) gated by ONE
// shared bearer token. The `app` slug is display/audit provenance, not an
// authentication boundary: any token holder may name any app (owner
// decision, one-token estate).
func (h *Handler) notificationSend(w http.ResponseWriter, r *http.Request) {
	if !h.notifications.AuthorizeSend(bearerToken(r.Header.Get("Authorization"))) {
		// The audit line never carries the token (or the body).
		slog.Info("notifications.send.denied", "ip", clientIP(r))
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var in struct {
		App       string          `json:"app"`
		To        []string        `json:"to"`
		Type      string          `json:"type"`
		Title     string          `json:"title"`
		Body      string          `json:"body"`
		Payload   json.RawMessage `json:"payload"`
		ExpiresAt *string         `json:"expiresAt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, notificationSendBodyMax)).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var expiresAt *time.Time
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, *in.ExpiresAt)
		if err != nil {
			httpx.WriteErr(w, http.StatusBadRequest, "expiresAt must be an RFC 3339 timestamp")
			return
		}
		utc := parsed.UTC()
		expiresAt = &utc
	}

	userIDs, err := h.notifications.UserIDsForUsernames(r.Context(), in.To)
	if err != nil {
		h.notificationFail(w, r, err)
		return
	}

	batch := make([]notifications.SendParams, 0, len(userIDs))
	for _, userID := range userIDs {
		batch = append(batch, notifications.SendParams{
			UserID:    userID,
			App:       in.App,
			Type:      in.Type,
			Title:     in.Title,
			Body:      in.Body,
			Payload:   in.Payload,
			ExpiresAt: expiresAt,
		})
	}

	sent, err := h.notifications.Send(r.Context(), batch)
	if err != nil {
		h.notificationFail(w, r, err)
		return
	}
	slog.Info("notifications.sent",
		"app", in.App, "type", in.Type, "recipients", len(userIDs), "sent", sent, "ip", clientIP(r))
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"sent": sent})
}

// notificationFail maps the module's sentinels; anything else takes the
// shared 500 voice (httpx.Fail logs the correlation id).
func (h *Handler) notificationFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notifications.ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, notifications.ErrInvalid):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, notifications.ErrRateLimited):
		httpx.WriteErr(w, http.StatusTooManyRequests, "send rate limit exceeded")
	default:
		httpx.Fail(w, r, err)
	}
}

// bearerToken extracts the credential from an Authorization header,
// accepting the scheme case-insensitively (HTTP auth schemes are
// case-insensitive; the token itself is compared verbatim, constant time).
func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return header[len(prefix):]
}
