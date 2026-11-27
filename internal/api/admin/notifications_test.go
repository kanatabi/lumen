// notifications_test.go — the notifications transport, offline: the
// sentinel→status table, the query grammar, the SEND door's token gate,
// the unmounted honesty, and the read routes' principal scoping. The
// real Service is exercised against Postgres in internal/notifications;
// here a fake isolates THIS package's obligations (decode/call/map/
// encode). Principals are injected through the package-private ctx key —
// the authenticator itself is proven in desk_auth_test.go.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/notifications"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/throttle"
)

// fakeNotifications records what the handler asked for and returns the
// test's chosen verdict. Every method is deliberately trivial: the value
// under test is the handler's contract, not a second service.
type fakeNotifications struct {
	sendEnabled bool
	token       string

	listItems []notifications.Notification
	listErr   error
	listUser  string
	listOpts  notifications.ListOpts

	unreadCount int
	unreadErr   error
	unreadUser  string

	markReadErr  error
	markReadUser string
	markReadID   string

	markAllUpdated int
	markAllErr     error
	markAllUser    string

	userIDs       []string
	resolveErr    error
	resolvedNames []string

	sendCalls int
	sendCount int
	sendErr   error
	sendBatch []notifications.SendParams
}

func (f *fakeNotifications) SendEnabled() bool { return f.sendEnabled }

func (f *fakeNotifications) AuthorizeSend(token string) bool {
	return f.token != "" && token == f.token
}

func (f *fakeNotifications) Send(_ context.Context, batch []notifications.SendParams) (int, error) {
	f.sendCalls++
	f.sendBatch = batch
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	if f.sendCount > 0 {
		return f.sendCount, nil
	}
	return len(batch), nil
}

func (f *fakeNotifications) UserIDsForUsernames(_ context.Context, names []string) ([]string, error) {
	f.resolvedNames = names
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.userIDs, nil
}

func (f *fakeNotifications) List(_ context.Context, userID string, opts notifications.ListOpts) ([]notifications.Notification, error) {
	f.listUser = userID
	f.listOpts = opts
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listItems, nil
}

func (f *fakeNotifications) UnreadCount(_ context.Context, userID string) (int, error) {
	f.unreadUser = userID
	if f.unreadErr != nil {
		return 0, f.unreadErr
	}
	return f.unreadCount, nil
}

func (f *fakeNotifications) MarkRead(_ context.Context, userID, id string) error {
	f.markReadUser = userID
	f.markReadID = id
	return f.markReadErr
}

func (f *fakeNotifications) MarkAllRead(_ context.Context, userID string) (int, error) {
	f.markAllUser = userID
	if f.markAllErr != nil {
		return 0, f.markAllErr
	}
	return f.markAllUpdated, nil
}

func notificationTestHandler(fake *fakeNotifications) *Handler {
	handler := New(nil)
	handler.EnableNotifications(fake)
	return handler
}

// notificationRequest injects the authenticated principal exactly where
// localAuthenticator puts it, without standing up the mint.
func notificationRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(context.WithValue(request.Context(), principalKey,
		adminsvc.Principal{ID: "user-1", Username: "alice"}))
}

func TestNotificationListParsesQueryAndReturnsTheWireShape(t *testing.T) {
	created := time.Date(2026, 11, 27, 10, 0, 0, 123456789, time.UTC)
	expires := created.Add(30 * 24 * time.Hour)
	fake := &fakeNotifications{listItems: []notifications.Notification{{
		ID:        "notification-1",
		App:       "tasks",
		Type:      "project.invite",
		Title:     "📦 You've been invited",
		Body:      "<b>admin</b> invited you.",
		Payload:   json.RawMessage(`{"url":"/tasks"}`),
		ExpiresAt: expires,
		CreatedAt: created,
	}}}
	handler := notificationTestHandler(fake)

	recorder := httptest.NewRecorder()
	handler.notificationList(recorder, notificationRequest(http.MethodGet,
		"/admin/notifications?unread=true&before=2026-11-26T10:00:00Z&limit=5", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if fake.listUser != "user-1" {
		t.Fatalf("list user = %q, want the principal's id", fake.listUser)
	}
	if !fake.listOpts.UnreadOnly {
		t.Fatal("unread=true was not passed through")
	}
	if fake.listOpts.Limit != 5 {
		t.Fatalf("limit = %d, want 5", fake.listOpts.Limit)
	}
	wantBefore, _ := time.Parse(time.RFC3339, "2026-11-26T10:00:00Z")
	if !fake.listOpts.Before.Equal(wantBefore) {
		t.Fatalf("before = %s, want %s", fake.listOpts.Before, wantBefore)
	}

	var got struct {
		Items []struct {
			ID        string          `json:"id"`
			App       string          `json:"app"`
			Type      string          `json:"type"`
			Title     string          `json:"title"`
			Body      string          `json:"body"`
			Payload   json.RawMessage `json:"payload"`
			ReadAt    *time.Time      `json:"readAt"`
			ExpiresAt time.Time       `json:"expiresAt"`
			CreatedAt time.Time       `json:"createdAt"`
		} `json:"items"`
		NextBefore *string `json:"nextBefore"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body.String())
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.ID != "notification-1" || item.App != "tasks" || item.Type != "project.invite" ||
		item.Title != "📦 You've been invited" || item.Body != "<b>admin</b> invited you." {
		t.Fatalf("row shape drift: %+v", item)
	}
	if string(item.Payload) != `{"url":"/tasks"}` {
		t.Fatalf("payload = %s, want the stored object verbatim", item.Payload)
	}
	if item.ReadAt != nil {
		t.Fatalf("readAt = %v, want null", item.ReadAt)
	}
	if !item.CreatedAt.Equal(created) || !item.ExpiresAt.Equal(expires) {
		t.Fatalf("timestamps drifted: created=%s expires=%s", item.CreatedAt, item.ExpiresAt)
	}
	if got.NextBefore != nil {
		t.Fatalf("a partial page has no cursor, got %q", *got.NextBefore)
	}
}

func TestNotificationListRejectsBadQuery(t *testing.T) {
	for _, target := range []string{
		"/admin/notifications?unread=yes",
		"/admin/notifications?before=yesterday",
		"/admin/notifications?limit=abc",
		"/admin/notifications?limit=-1",
	} {
		t.Run(target, func(t *testing.T) {
			fake := &fakeNotifications{}
			recorder := httptest.NewRecorder()
			notificationTestHandler(fake).notificationList(recorder,
				notificationRequest(http.MethodGet, target, ""))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if fake.listUser != "" {
				t.Fatal("a bad query must not reach the service")
			}
		})
	}
}

func TestNotificationListCursorIsExact(t *testing.T) {
	created := time.Date(2026, 11, 27, 10, 0, 0, 123456789, time.UTC)
	items := []notifications.Notification{
		{ID: "newer", CreatedAt: created.Add(time.Minute), Payload: json.RawMessage(`{}`)},
		{ID: "last", CreatedAt: created, Payload: json.RawMessage(`{}`)},
	}
	fake := &fakeNotifications{listItems: items}
	handler := notificationTestHandler(fake)

	recorder := httptest.NewRecorder()
	handler.notificationList(recorder, notificationRequest(http.MethodGet, "/admin/notifications?limit=2", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var got struct {
		NextBefore *string `json:"nextBefore"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := created.Format(time.RFC3339Nano)
	if got.NextBefore == nil || *got.NextBefore != want {
		t.Fatalf("nextBefore = %v, want %q (nanosecond precision: seconds would skip rows)", got.NextBefore, want)
	}

	// one row short of the limit means the page is the tail
	fake.listItems = items[:1]
	recorder = httptest.NewRecorder()
	handler.notificationList(recorder, notificationRequest(http.MethodGet, "/admin/notifications?limit=2", ""))
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode partial page: %v", err)
	}
	if got.NextBefore != nil {
		t.Fatalf("partial page cursor = %q, want null", *got.NextBefore)
	}
}

func TestNotificationReadRoutesUseThePrincipal(t *testing.T) {
	now := time.Now().UTC()
	fake := &fakeNotifications{
		unreadCount:    3,
		markAllUpdated: 4,
		listItems:      []notifications.Notification{{ID: "n", CreatedAt: now, Payload: json.RawMessage(`{}`)}},
	}
	handler := notificationTestHandler(fake)

	recorder := httptest.NewRecorder()
	handler.notificationList(recorder, notificationRequest(http.MethodGet, "/admin/notifications", ""))
	if fake.listUser != "user-1" {
		t.Fatalf("list user = %q, want user-1", fake.listUser)
	}

	recorder = httptest.NewRecorder()
	handler.notificationUnreadCount(recorder, notificationRequest(http.MethodGet,
		"/admin/notifications/unread-count", ""))
	if recorder.Code != http.StatusOK || fake.unreadUser != "user-1" {
		t.Fatalf("unread: status %d user %q (body %s)", recorder.Code, fake.unreadUser, recorder.Body.String())
	}
	var count struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &count); err != nil || count.Count != 3 {
		t.Fatalf("count body = %s (err %v), want 3", recorder.Body.String(), err)
	}

	// the id from the PATH is passed alongside the principal's id; the
	// store's WHERE clause is the one that enforces ownership (a foreign
	// row is ErrNotFound, never a touched row). Route through chi so the
	// URL param is populated exactly as the mount's tree does it.
	router := chi.NewRouter()
	router.Post("/admin/notifications/{id}/read", handler.notificationMarkRead)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, notificationRequest(http.MethodPost,
		"/admin/notifications/notification-1/read", ""))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("mark read status %d, want 204 (body %s)", recorder.Code, recorder.Body.String())
	}
	if fake.markReadUser != "user-1" || fake.markReadID != "notification-1" {
		t.Fatalf("mark read called with user %q id %q", fake.markReadUser, fake.markReadID)
	}

	recorder = httptest.NewRecorder()
	handler.notificationMarkAllRead(recorder, notificationRequest(http.MethodPost,
		"/admin/notifications/read-all", ""))
	if recorder.Code != http.StatusOK || fake.markAllUser != "user-1" {
		t.Fatalf("mark all: status %d user %q (body %s)", recorder.Code, fake.markAllUser, recorder.Body.String())
	}
	var updated struct {
		Updated int `json:"updated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil || updated.Updated != 4 {
		t.Fatalf("updated body = %s (err %v), want 4", recorder.Body.String(), err)
	}
}

func TestNotificationMarkReadSentinelMap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"success", nil, http.StatusNoContent},
		{"foreign/expired/unknown → 404", notifications.ErrNotFound, http.StatusNotFound},
		{"invalid id → 400", fmt.Errorf("%w: bad uuid", notifications.ErrInvalid), http.StatusBadRequest},
		{"rate limit is not a read concern, still mapped", notifications.ErrRateLimited, http.StatusTooManyRequests},
		{"anything else → 500", errors.New("database down"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeNotifications{markReadErr: tc.err}
			recorder := httptest.NewRecorder()
			notificationTestHandler(fake).notificationMarkRead(recorder,
				notificationRequest(http.MethodPost, "/admin/notifications/x/read", ""))
			if recorder.Code != tc.wantCode {
				t.Fatalf("status %d, want %d (body %s)", recorder.Code, tc.wantCode, recorder.Body.String())
			}
		})
	}
}

func TestNotificationSendAuthAndBatch(t *testing.T) {
	body := `{"app":"content","to":["admin","sarah"],"type":"content.published",` +
		`"title":"📝 New post","body":"<b>Hello</b> is live.","payload":{"url":"/posts/hello"},` +
		`"expiresAt":"2026-12-01T00:00:00Z"}`

	for _, tc := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"absent token → 401", "", http.StatusUnauthorized},
		{"wrong scheme → 401", "Basic test-token", http.StatusUnauthorized},
		{"wrong token → 401", "Bearer nope", http.StatusUnauthorized},
		{"empty bearer → 401", "Bearer ", http.StatusUnauthorized},
		{"correct token → 201", "Bearer test-token", http.StatusCreated},
		{"scheme is case-insensitive", "bearer test-token", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeNotifications{
				sendEnabled: true,
				token:       "test-token",
				userIDs:     []string{"id-admin", "id-sarah"},
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/notifications", strings.NewReader(body))
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}
			recorder := httptest.NewRecorder()
			notificationTestHandler(fake).notificationSend(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus != http.StatusCreated {
				if fake.sendCalls != 0 {
					t.Fatal("a rejected token must never reach the service")
				}
				return
			}

			if !slices.Equal(fake.resolvedNames, []string{"admin", "sarah"}) {
				t.Fatalf("resolved names = %v, want the body's to list", fake.resolvedNames)
			}
			if len(fake.sendBatch) != 2 {
				t.Fatalf("batch = %d, want one per resolved id", len(fake.sendBatch))
			}
			if fake.sendBatch[0].UserID != "id-admin" || fake.sendBatch[1].UserID != "id-sarah" {
				t.Fatalf("recipient ids = %q/%q, order must follow the names",
					fake.sendBatch[0].UserID, fake.sendBatch[1].UserID)
			}
			first := fake.sendBatch[0]
			if first.App != "content" || first.Type != "content.published" ||
				first.Title != "📝 New post" || first.Body != "<b>Hello</b> is live." {
				t.Fatalf("field mapping drift: %+v", first)
			}
			if string(first.Payload) != `{"url":"/posts/hello"}` {
				t.Fatalf("payload = %s", first.Payload)
			}
			wantExpiry, _ := time.Parse(time.RFC3339, "2026-12-01T00:00:00Z")
			if first.ExpiresAt == nil || !first.ExpiresAt.Equal(wantExpiry) {
				t.Fatalf("expiresAt = %v, want %s", first.ExpiresAt, wantExpiry)
			}

			var sent struct {
				Sent int `json:"sent"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &sent); err != nil || sent.Sent != 2 {
				t.Fatalf("sent body = %s (err %v), want 2", recorder.Body.String(), err)
			}
		})
	}
}

func TestNotificationSendSentinelMap(t *testing.T) {
	validBody := `{"app":"content","to":["admin"],"type":"content.published","title":"hi","body":"","payload":{}}`
	for _, tc := range []struct {
		name       string
		body       string
		fake       *fakeNotifications
		wantStatus int
	}{
		{
			name:       "unknown username → 400",
			body:       validBody,
			fake:       &fakeNotifications{resolveErr: fmt.Errorf("%w: unknown username %q", notifications.ErrInvalid, "ghost")},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid notification → 400",
			body:       validBody,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}, sendErr: notifications.ErrInvalid},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "rate limited → 429",
			body:       validBody,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}, sendErr: notifications.ErrRateLimited},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "unmapped failure → 500",
			body:       validBody,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}, sendErr: errors.New("db down")},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "bad expiresAt → 400",
			body:       `{"app":"content","to":["admin"],"type":"x","title":"hi","body":"","expiresAt":"tomorrow"}`,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed JSON → 400",
			body:       `{`,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "body over 64 KiB → 400 before parsing",
			body: `{"app":"content","to":["admin"],"type":"x","title":"` +
				strings.Repeat("a", 70*1024) + `"}`,
			fake:       &fakeNotifications{userIDs: []string{"id-admin"}},
			wantStatus: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fake.sendEnabled = true
			tc.fake.token = "test-token"
			request := httptest.NewRequest(http.MethodPost, "/admin/notifications", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer test-token")
			recorder := httptest.NewRecorder()
			notificationTestHandler(tc.fake).notificationSend(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
		})
	}
}

// TestNotificationSendAlwaysAnswers401WithoutTheToken pins the door's
// public-mount behavior: authentication is the handler's first act, so an
// anonymous caller learns nothing about the body it sent.
func TestNotificationSendAlwaysAnswers401WithoutTheToken(t *testing.T) {
	fake := &fakeNotifications{sendEnabled: true, token: "test-token", userIDs: []string{"id-admin"}}
	alwaysInvalid := `{`
	request := httptest.NewRequest(http.MethodPost, "/admin/notifications", strings.NewReader(alwaysInvalid))
	recorder := httptest.NewRecorder()
	notificationTestHandler(fake).notificationSend(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 even for a malformed body", recorder.Code)
	}
}

func TestNotificationMountHonesty(t *testing.T) {
	onRoutes := mountNotificationRoutes(t, "test-token")
	for _, want := range []string{
		"GET /admin/notifications",
		"GET /admin/notifications/unread-count",
		"POST /admin/notifications/{id}/read",
		"POST /admin/notifications/read-all",
		"POST /admin/notifications",
	} {
		if !onRoutes[want] {
			t.Fatalf("configured mount is missing %q (got %v)", want, onRoutes)
		}
	}

	offRoutes := mountNotificationRoutes(t, "")
	for _, want := range []string{
		"GET /admin/notifications",
		"GET /admin/notifications/unread-count",
		"POST /admin/notifications/{id}/read",
		"POST /admin/notifications/read-all",
	} {
		if !offRoutes[want] {
			t.Fatalf("unconfigured mount is missing the inbox route %q", want)
		}
	}
	if offRoutes["POST /admin/notifications"] {
		t.Fatal("no token must remove the SEND door entirely (honest absence)")
	}
}

func TestNotificationSendDoorIsMountedWhenConfigured(t *testing.T) {
	mux := mountNotificationRouter(t, "test-token")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/notifications", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("mounted door without a token = %d, want 401", recorder.Code)
	}

	offMux := mountNotificationRouter(t, "")
	recorder = httptest.NewRecorder()
	offMux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/notifications", strings.NewReader(`{}`)))
	// The POST route does NOT exist; chi answers 405 rather than 404 only
	// because the SAME path serves GET (the inbox). The route-table walk
	// above is the definitive absence proof; no handler ran and no token
	// was ever compared.
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unmounted door = %d, want 405 (route absent; GET sibling exists)", recorder.Code)
	}
}

// mountNotificationRoutes builds the real Mount with a token choice and
// returns the registered method+pattern set. Registration touches no DB.
func mountNotificationRoutes(t *testing.T, token string) map[string]bool {
	t.Helper()
	mux := mountNotificationRouter(t, token)
	routes := map[string]bool{}
	if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/") {
			routes[method+" "+route] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	return routes
}

func mountNotificationRouter(t *testing.T, token string) *chi.Mux {
	t.Helper()
	t.Setenv("LUMEN_NOTIFICATION_TOKEN", token)
	// Mount registers only; the authenticator would touch the DB, but no
	// test here serves an authenticated route.
	svc := adminsvc.New(nil, nil, nil, "test-secret-32-bytes-aaaaaaaaaaaaaa", throttle.New(0))
	handler := New(svc)
	handler.EnableNotifications(notifications.NewService(nil))
	mux := chi.NewRouter()
	handler.Mount(mux, role.Admin.Planes())
	return mux
}
