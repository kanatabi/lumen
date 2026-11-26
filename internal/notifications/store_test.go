// store_test.go — the notifications store against REAL Postgres
// (LUMEN_INTEGRATION=1, the existing battery gate):
//
//	LUMEN_INTEGRATION=1 LUMEN_DB_URL="postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable" \
//	  go test ./internal/notifications -count=1
//
// Each test gets a fresh SCHEMA (not database) in the dev cluster and a
// pool whose search_path points at it — the rolesplit battery's trick —
// so parallel runs never see each other's rows and the migrations run
// from zero every time.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanatabi/lumen/internal/store"
)

type testFixture struct {
	notifications *Store
	pool          *pgxpool.Pool
	userID        string
	otherID       string
}

func integrationBaseURL(t *testing.T) string {
	t.Helper()
	if os.Getenv("LUMEN_INTEGRATION") == "" {
		t.Skip("LUMEN_INTEGRATION unset")
	}
	base := os.Getenv("LUMEN_DB_URL")
	if base == "" {
		base = "postgres://lumen:lumen-dev@localhost:5433/lumen?sslmode=disable"
	}
	return base
}

// newTestFixture migrates a private schema and seeds two users (the
// identity rows the store's FK needs; the passhash is never verified
// here — "x" is enough to own a row).
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	ctx := context.Background()
	base := integrationBaseURL(t)

	maintenancePool, err := store.NewPool(ctx, base)
	if err != nil {
		t.Fatalf("maintenance pool: %v", err)
	}
	t.Cleanup(maintenancePool.Close)

	// The schema name is GENERATED here (digits only), never a wire
	// value: the one place SQL text is assembled, exactly like the
	// rolesplit fixture.
	schema := fmt.Sprintf("notif_it_%d", time.Now().UnixNano())
	if _, err := maintenancePool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	// LIFO cleanup: close the test pool, drop the schema, close the
	// maintenance pool — in that order.
	t.Cleanup(func() {
		if _, err := maintenancePool.Exec(context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	dsn := base
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	testPool, err := store.NewPool(ctx, dsn+separator+"search_path="+schema)
	if err != nil {
		t.Fatalf("test pool: %v", err)
	}
	t.Cleanup(testPool.Close)

	if err := store.Migrate(ctx, testPool); err != nil {
		t.Fatalf("migrate fresh schema: %v", err)
	}

	pg := store.NewPG(testPool)
	for _, username := range []string{"notif-user", "notif-other"} {
		if err := pg.UpsertUser(ctx, username, "x"); err != nil {
			t.Fatalf("seed user %s: %v", username, err)
		}
	}

	return &testFixture{
		notifications: NewStore(testPool),
		pool:          testPool,
		userID:        userIDByUsername(t, testPool, "notif-user"),
		otherID:       userIDByUsername(t, testPool, "notif-other"),
	}
}

func userIDByUsername(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatalf("read user %s id: %v", username, err)
	}
	return id
}

func notificationIDByTitle(t *testing.T, pool *pgxpool.Pool, userID, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM notifications WHERE user_id = $1 AND title = $2`,
		userID, title).Scan(&id); err != nil {
		t.Fatalf("read notification %q id: %v", title, err)
	}
	return id
}

func readAtByID(t *testing.T, pool *pgxpool.Pool, id string) *time.Time {
	t.Helper()
	var readAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT read_at FROM notifications WHERE id = $1`, id).Scan(&readAt); err != nil {
		t.Fatalf("read read_at for %s: %v", id, err)
	}
	return readAt
}

func titlesOf(items []Notification) []string {
	titles := make([]string, len(items))
	for index, item := range items {
		titles[index] = item.Title
	}
	return titles
}

func TestSendBatchDefaultExpiryAndRoundTrip(t *testing.T) {
	setup := newTestFixture(t)
	ctx := context.Background()
	before := time.Now().UTC()

	sent, err := setup.notifications.Send(ctx, []SendParams{
		{
			UserID: setup.userID, App: "tasks", Type: "project.invite",
			Title: "📦 You've been invited", Body: "<b>admin</b> invited you to collaborate.",
			Payload: json.RawMessage(`{"url":"/tasks","invite":"inv-1"}`),
		},
		{
			UserID: setup.otherID, App: "content", Type: "content.published",
			Title: "📝 New post", Body: "",
		},
	})
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2 (one row per recipient)", sent)
	}

	items, err := setup.notifications.List(ctx, setup.userID, ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items for the first recipient = %d, want 1 (user boundary)", len(items))
	}
	item := items[0]

	if item.UserID != setup.userID || item.App != "tasks" ||
		item.Type != "project.invite" || item.Title != "📦 You've been invited" ||
		item.Body != "<b>admin</b> invited you to collaborate." {
		t.Fatalf("field round-trip drift: %+v", item)
	}
	if item.ReadAt != nil {
		t.Fatal("a fresh notification is already read")
	}
	if item.ID == "" {
		t.Fatal("notification id is empty")
	}

	// the owner's default: nil expires_at means now()+30 days
	wantExpiry := before.Add(30 * 24 * time.Hour)
	if delta := item.ExpiresAt.Sub(wantExpiry); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("default expires_at = %s, want ~%s (30 days)", item.ExpiresAt, wantExpiry)
	}
	// every timestamp leaves the store in UTC (the wire prints RFC 3339 Z)
	if item.ExpiresAt.Location() != time.UTC || item.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps not UTC: expires=%s created=%s",
			item.ExpiresAt.Location(), item.CreatedAt.Location())
	}
	if item.CreatedAt.Before(before.Add(-time.Minute)) || item.CreatedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("created_at = %s, want ~now", item.CreatedAt)
	}

	var payload struct {
		URL    string `json:"url"`
		Invite string `json:"invite"`
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v (%s)", err, item.Payload)
	}
	if payload.URL != "/tasks" || payload.Invite != "inv-1" {
		t.Fatalf("payload round-trip drift: %+v", payload)
	}

	otherItems, err := setup.notifications.List(ctx, setup.otherID, ListOpts{})
	if err != nil {
		t.Fatalf("list other recipient: %v", err)
	}
	if len(otherItems) != 1 || otherItems[0].Title != "📝 New post" {
		t.Fatalf("other recipient items = %+v, want only its own row", titlesOf(otherItems))
	}
	// a nil payload persists as the column's `{}`, never NULL
	if string(otherItems[0].Payload) != "{}" {
		t.Fatalf("empty payload stored as %q, want {}", otherItems[0].Payload)
	}
}

func TestListNewestFirstLimitAndBeforeCursor(t *testing.T) {
	setup := newTestFixture(t)
	ctx := context.Background()

	// separate Sends: each lands in its own transaction, then the test
	// backdates every row to a known, distinct stamp (deterministic order
	// — rows inserted in ONE transaction share now()).
	for _, title := range []string{"oldest", "middle", "newest"} {
		if _, err := setup.notifications.Send(ctx, []SendParams{{
			UserID: setup.userID, App: "tasks", Type: "task.assigned", Title: title,
		}}); err != nil {
			t.Fatalf("send %s: %v", title, err)
		}
	}
	base := time.Now().UTC().Add(-time.Hour)
	for offset, title := range []string{"oldest", "middle", "newest"} {
		if _, err := setup.pool.Exec(ctx,
			`UPDATE notifications SET created_at = $2 WHERE user_id = $1 AND title = $3`,
			setup.userID, base.Add(time.Duration(offset)*time.Minute), title); err != nil {
			t.Fatalf("backdate %s: %v", title, err)
		}
	}

	items, err := setup.notifications.List(ctx, setup.userID, ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"newest", "middle", "oldest"}; !slices.Equal(titlesOf(items), want) {
		t.Fatalf("order = %v, want %v (newest first)", titlesOf(items), want)
	}

	page, err := setup.notifications.List(ctx, setup.userID, ListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("limited list: %v", err)
	}
	if want := []string{"newest", "middle"}; !slices.Equal(titlesOf(page), want) {
		t.Fatalf("limit 2 = %v, want %v", titlesOf(page), want)
	}

	// the cursor is STRICT: the boundary row itself never repeats
	page, err = setup.notifications.List(ctx, setup.userID, ListOpts{Before: items[1].CreatedAt})
	if err != nil {
		t.Fatalf("before cursor: %v", err)
	}
	if want := []string{"oldest"}; !slices.Equal(titlesOf(page), want) {
		t.Fatalf("before %s = %v, want %v (strict cursor)", items[1].CreatedAt, titlesOf(page), want)
	}
}

func TestUnreadOnly(t *testing.T) {
	setup := newTestFixture(t)
	ctx := context.Background()

	for _, title := range []string{"first", "second"} {
		if _, err := setup.notifications.Send(ctx, []SendParams{{
			UserID: setup.userID, App: "tasks", Type: "task.assigned", Title: title,
		}}); err != nil {
			t.Fatalf("send %s: %v", title, err)
		}
	}
	if err := setup.notifications.MarkRead(ctx, setup.userID,
		notificationIDByTitle(t, setup.pool, setup.userID, "first")); err != nil {
		t.Fatalf("mark first read: %v", err)
	}

	unread, err := setup.notifications.List(ctx, setup.userID, ListOpts{UnreadOnly: true})
	if err != nil {
		t.Fatalf("unread list: %v", err)
	}
	if want := []string{"second"}; !slices.Equal(titlesOf(unread), want) {
		t.Fatalf("unread = %v, want %v", titlesOf(unread), want)
	}

	all, err := setup.notifications.List(ctx, setup.userID, ListOpts{})
	if err != nil {
		t.Fatalf("all list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %v, want both rows", titlesOf(all))
	}
	for _, item := range all {
		if item.Title == "first" && item.ReadAt == nil {
			t.Fatal("read row lost its read_at")
		}
		if item.Title == "second" && item.ReadAt != nil {
			t.Fatal("unread row gained a read_at")
		}
		if item.ReadAt != nil && item.ReadAt.Location() != time.UTC {
			t.Fatalf("read_at not UTC: %s", item.ReadAt.Location())
		}
	}

	count, err := setup.notifications.UnreadCount(ctx, setup.userID)
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if count != 1 {
		t.Fatalf("unread count = %d, want 1", count)
	}
}

func TestMarkReadIdempotenceAndDenials(t *testing.T) {
	setup := newTestFixture(t)
	ctx := context.Background()

	for _, params := range []SendParams{
		{UserID: setup.userID, App: "tasks", Type: "task.assigned", Title: "mine"},
		{UserID: setup.otherID, App: "tasks", Type: "task.assigned", Title: "theirs"},
	} {
		if _, err := setup.notifications.Send(ctx, []SendParams{params}); err != nil {
			t.Fatalf("send %s: %v", params.Title, err)
		}
	}
	mineID := notificationIDByTitle(t, setup.pool, setup.userID, "mine")
	theirsID := notificationIDByTitle(t, setup.pool, setup.otherID, "theirs")

	if err := setup.notifications.MarkRead(ctx, setup.userID, mineID); err != nil {
		t.Fatalf("mark mine read: %v", err)
	}
	if readAtByID(t, setup.pool, mineID) == nil {
		t.Fatal("mark read did not stamp read_at")
	}

	// idempotent: the WHERE does not filter on read_at
	if err := setup.notifications.MarkRead(ctx, setup.userID, mineID); err != nil {
		t.Fatalf("second mark read must succeed: %v", err)
	}

	// a foreign row is the ONE denial, and it must stay untouched
	if err := setup.notifications.MarkRead(ctx, setup.otherID, mineID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign mark read = %v, want ErrNotFound", err)
	}
	if err := setup.notifications.MarkRead(ctx, setup.userID, theirsID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("marking someone else's row = %v, want ErrNotFound", err)
	}
	if readAtByID(t, setup.pool, theirsID) != nil {
		t.Fatal("a denied mark read still stamped the foreign row")
	}

	// unknown but well-formed, and malformed: both name no row
	if err := setup.notifications.MarkRead(ctx, setup.userID,
		"11111111-1111-1111-1111-111111111111"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
	if err := setup.notifications.MarkRead(ctx, setup.userID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v, want ErrNotFound (never a 500)", err)
	}

	// the foreign row is still unread for its owner
	count, err := setup.notifications.UnreadCount(ctx, setup.otherID)
	if err != nil {
		t.Fatalf("other unread count: %v", err)
	}
	if count != 1 {
		t.Fatalf("other unread count = %d, want 1", count)
	}
}

func TestExpiredRowsAreInvisible(t *testing.T) {
	setup := newTestFixture(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	sent, err := setup.notifications.Send(ctx, []SendParams{
		{
			UserID: setup.userID, App: "tasks", Type: "project.invite",
			Title: "expired", ExpiresAt: &past,
		},
		{
			UserID: setup.userID, App: "tasks", Type: "task.assigned",
			Title: "live", ExpiresAt: &future,
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}

	items, err := setup.notifications.List(ctx, setup.userID, ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"live"}; !slices.Equal(titlesOf(items), want) {
		t.Fatalf("list = %v, want %v (expired rows never surface)", titlesOf(items), want)
	}
	if delta := items[0].ExpiresAt.Sub(future); delta < -time.Second || delta > time.Second {
		t.Fatalf("explicit expiry round-trip = %s, want ~%s", items[0].ExpiresAt, future)
	}

	count, err := setup.notifications.UnreadCount(ctx, setup.userID)
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if count != 1 {
		t.Fatalf("unread count = %d, want 1 (the bell counts only live rows)", count)
	}

	expiredID := notificationIDByTitle(t, setup.pool, setup.userID, "expired")
	liveID := notificationIDByTitle(t, setup.pool, setup.userID, "live")

	if err := setup.notifications.MarkRead(ctx, setup.userID, expiredID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark expired read = %v, want ErrNotFound", err)
	}
	if readAtByID(t, setup.pool, expiredID) != nil {
		t.Fatal("an expired row was marked read")
	}

	touched, err := setup.notifications.MarkAllRead(ctx, setup.userID)
	if err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	if touched != 1 {
		t.Fatalf("mark all read touched %d, want 1 (expired rows stay alone)", touched)
	}
	if readAtByID(t, setup.pool, expiredID) != nil {
		t.Fatal("mark all read stamped an expired row")
	}
	if readAtByID(t, setup.pool, liveID) == nil {
		t.Fatal("mark all read missed the live row")
	}

	count, err = setup.notifications.UnreadCount(ctx, setup.userID)
	if err != nil {
		t.Fatalf("unread count after mark all: %v", err)
	}
	if count != 0 {
		t.Fatalf("unread count after mark all = %d, want 0", count)
	}

	touched, err = setup.notifications.MarkAllRead(ctx, setup.userID)
	if err != nil {
		t.Fatalf("second mark all read: %v", err)
	}
	if touched != 0 {
		t.Fatalf("second mark all read touched %d, want 0", touched)
	}
}
