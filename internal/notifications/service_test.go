// service_test.go — the notifications module's RULES, offline. The store
// has its own integration battery (store_test.go); this file proves the
// pieces the service owns and the store deliberately does not: the inline
// HTML allowlist, the caps, slug/payload validation, the constant-time
// token, and the fixed-window limiter. The username lookup is the one
// DB-dependent rule, so it rides the same LUMEN_INTEGRATION gate as the
// store suite.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// validSendParams is a baseline the boundary tests mutate one field at a
// time: every case proves exactly ONE rule, never an accidental stack.
func validSendParams() SendParams {
	return SendParams{
		UserID:  "11111111-1111-1111-1111-111111111111",
		App:     "tasks",
		Type:    "project.invite",
		Title:   "📦 You've been invited",
		Body:    "<b>admin</b> invited you to collaborate.",
		Payload: json.RawMessage(`{"url":"/tasks","invite":"inv-1"}`),
	}
}

func TestSanitizerInlineAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		contains []string
		absent   []string
	}{
		{
			name:     "emphasis, code and breaks survive",
			in:       "<b>bold</b> <strong>strong</strong> <i>ital</i> <em>em</em> <u>under</u> <s>strike</s> <code>code</code><br>",
			contains: []string{"<b>bold</b>", "<strong>strong</strong>", "<i>ital</i>", "<em>em</em>", "<u>under</u>", "<s>strike</s>", "<code>code</code>", "<br"},
		},
		{
			name:     "same-origin link keeps its href",
			in:       `<a href="/posts/hello">open</a>`,
			contains: []string{`href="/posts/hello"`, "open"},
		},
		{
			name:     "javascript href is stripped, text survives",
			in:       `<a href="javascript:alert(1)">click</a>`,
			contains: []string{"click"},
			absent:   []string{"javascript", "<a"},
		},
		{
			name:     "external href is stripped",
			in:       `<a href="https://evil.example/x">out</a>`,
			contains: []string{"out"},
			absent:   []string{"evil.example", "<a"},
		},
		{
			name:     "protocol-relative href is not same-origin",
			in:       `<a href="//evil.example/x">out</a>`,
			contains: []string{"out"},
			absent:   []string{"evil.example", "<a"},
		},
		{
			name:     "backslash cannot smuggle an authority",
			in:       `<a href="/\evil.example/x">out</a>`,
			contains: []string{"out"},
			absent:   []string{"evil.example", "<a"},
		},
		{
			name:     "span keeps colour and weight only",
			in:       `<span style="color: red; background-color: #ff0000; font-weight: bold; position: fixed">x</span>`,
			contains: []string{"color", "background-color", "font-weight", "x"},
			absent:   []string{"position"},
		},
		{
			name:     "a disallowed-only style loses the whole attribute",
			in:       `<span style="position: fixed; z-index: 9">x</span>`,
			contains: []string{"x"},
			absent:   []string{"position", "z-index"},
		},
		{
			name:   "script is removed with its content",
			in:     `<script>alert("xss")</script><b>safe</b>`,
			absent: []string{"script", "alert", "xss"},
		},
		{
			name:   "iframe is removed",
			in:     `<iframe src="https://evil.example"></iframe>`,
			absent: []string{"iframe", "evil.example"},
		},
		{
			name:   "img and event handlers are removed",
			in:     `<img src="x" onerror="alert(1)">`,
			absent: []string{"<img", "onerror", "alert"},
		},
		{
			name:     "event handlers die with their element",
			in:       `<b onclick="alert(1)">safe</b>`,
			contains: []string{"<b>safe</b>"},
			absent:   []string{"onclick", "alert"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := notificationPolicy.Sanitize(tc.in)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("sanitized %q missing %q", got, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Fatalf("sanitized %q still carries %q", got, unwanted)
				}
			}
		})
	}
}

func TestValidateBatchTitleAndBodyCaps(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*SendParams)
		wantErr bool
	}{
		{"title of 200 runes is allowed (runes, not bytes)", func(p *SendParams) {
			p.Title = strings.Repeat("é", 200)
		}, false},
		{"title of 201 runes is rejected", func(p *SendParams) {
			p.Title = strings.Repeat("é", 201)
		}, true},
		{"empty title is rejected", func(p *SendParams) { p.Title = "" }, true},
		{"whitespace-only title is rejected", func(p *SendParams) { p.Title = "   " }, true},
		{"invalid UTF-8 title is rejected", func(p *SendParams) {
			p.Title = string([]byte{0xff, 0xfe})
		}, true},
		{"body of 8192 bytes is allowed", func(p *SendParams) {
			p.Body = strings.Repeat("a", 8192)
		}, false},
		{"body of 8193 bytes is rejected", func(p *SendParams) {
			p.Body = strings.Repeat("a", 8193)
		}, true},
		{"invalid UTF-8 body is rejected", func(p *SendParams) {
			p.Body = string([]byte{0xff, 0xfe})
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := validSendParams()
			tc.mutate(&params)
			_, err := validateBatch([]SendParams{params})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateBatchSanitizesBody(t *testing.T) {
	params := validSendParams()
	params.Body = `<script>alert(1)</script><b>kept</b>`
	validated, err := validateBatch([]SendParams{params})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if validated[0].Body != "<b>kept</b>" {
		t.Fatalf("stored body = %q, want the sanitized form", validated[0].Body)
	}
	// the caller's slice is not mutated (the in-process seam may reuse it)
	if params.Body != `<script>alert(1)</script><b>kept</b>` {
		t.Fatalf("caller's body was mutated: %q", params.Body)
	}
}

func TestValidateBatchPayloadRules(t *testing.T) {
	// exactly 8192 bytes: `{"x":"` (6) + 8184 + `"}` (2)
	maxPayload := json.RawMessage(`{"x":"` + strings.Repeat("a", 8184) + `"}`)
	if len(maxPayload) != 8192 {
		t.Fatalf("fixture length = %d, want 8192", len(maxPayload))
	}
	cases := []struct {
		name    string
		payload json.RawMessage
		wantErr bool
	}{
		{"absent payload is allowed", nil, false},
		{"empty object is allowed", json.RawMessage(`{}`), false},
		{"object at the byte cap is allowed", maxPayload, false},
		{"object past the byte cap is rejected", json.RawMessage(`{"x":"` + strings.Repeat("a", 8185) + `"}`), true},
		{"array is rejected", json.RawMessage(`[]`), true},
		{"string is rejected", json.RawMessage(`"text"`), true},
		{"null is rejected", json.RawMessage(`null`), true},
		{"same-origin url is allowed", json.RawMessage(`{"url":"/tasks?x=1"}`), false},
		{"bare root url is allowed", json.RawMessage(`{"url":"/"}`), false},
		{"relative url without slash is rejected", json.RawMessage(`{"url":"tasks"}`), true},
		{"protocol-relative url is rejected", json.RawMessage(`{"url":"//evil.example"}`), true},
		{"backslash url is rejected", json.RawMessage(`{"url":"/\\evil.example"}`), true},
		{"non-string url is rejected", json.RawMessage(`{"url":42}`), true},
		{"object without url is allowed", json.RawMessage(`{"ref":"inv-1"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := validSendParams()
			params.Payload = tc.payload
			_, err := validateBatch([]SendParams{params})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateBatchSlugsAndRecipientSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*SendParams)
		wantErr bool
	}{
		{"plain slug", func(*SendParams) {}, false},
		{"dotted and hyphenated type", func(p *SendParams) { p.Type = "project.member-role" }, false},
		{"32-character slug is the ceiling", func(p *SendParams) { p.App = strings.Repeat("a", 32) }, false},
		{"33-character slug is rejected", func(p *SendParams) { p.App = strings.Repeat("a", 33) }, true},
		{"uppercase slug is rejected", func(p *SendParams) { p.App = "Tasks" }, true},
		{"leading digit is rejected", func(p *SendParams) { p.App = "1tasks" }, true},
		{"empty type is rejected", func(p *SendParams) { p.Type = "" }, true},
		{"empty recipient id is rejected", func(p *SendParams) { p.UserID = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := validSendParams()
			tc.mutate(&params)
			_, err := validateBatch([]SendParams{params})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}

	if _, err := validateBatch(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty batch = %v, want ErrInvalid", err)
	}

	batch := make([]SendParams, 100)
	for index := range batch {
		batch[index] = validSendParams()
	}
	if _, err := validateBatch(batch); err != nil {
		t.Fatalf("100 recipients must be allowed: %v", err)
	}
	batch = append(batch, validSendParams())
	if _, err := validateBatch(batch); !errors.Is(err, ErrInvalid) {
		t.Fatalf("101 recipients = %v, want ErrInvalid", err)
	}
}

func TestSendRejectsInvalidInputBeforeTheStore(t *testing.T) {
	// NewService(nil) would panic if Send reached the store: this proves
	// validation is the gate, not a post-insert reaction.
	service := NewService(nil)
	if _, err := service.Send(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty send = %v, want ErrInvalid", err)
	}
	params := validSendParams()
	params.Title = ""
	if _, err := service.Send(context.Background(), []SendParams{params}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad title send = %v, want ErrInvalid", err)
	}
}

func TestAuthorizeSend(t *testing.T) {
	t.Setenv(notificationTokenEnv, "")
	unconfigured := NewService(nil)
	if unconfigured.SendEnabled() {
		t.Fatal("no token must mean the send door is disabled")
	}
	if unconfigured.AuthorizeSend("") || unconfigured.AuthorizeSend("anything") {
		t.Fatal("an unconfigured service must authorize nobody")
	}

	t.Setenv(notificationTokenEnv, "s3cret-token")
	configured := NewService(nil)
	if !configured.SendEnabled() {
		t.Fatal("a configured token must enable the send door")
	}
	if !configured.AuthorizeSend("s3cret-token") {
		t.Fatal("the configured token must authorize")
	}
	for _, wrong := range []string{"", "s3cret", "s3cret-token ", "S3CRET-TOKEN", "other"} {
		if configured.AuthorizeSend(wrong) {
			t.Fatalf("wrong token %q authorized", wrong)
		}
	}
}

func TestSendLimiterFixedWindow(t *testing.T) {
	limiter := newSendLimiter(2, time.Minute)
	start := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return start }

	if !limiter.Allow("tasks") || !limiter.Allow("tasks") {
		t.Fatal("the first two sends in a window must pass")
	}
	if limiter.Allow("tasks") {
		t.Fatal("the third send with a 2/min window must be rejected")
	}
	// another app has its own allowance
	if !limiter.Allow("content") {
		t.Fatal("one app's window must not starve another")
	}
	// a new window resets the counter (fixed, not sliding)
	limiter.now = func() time.Time { return start.Add(2 * time.Minute) }
	if !limiter.Allow("tasks") {
		t.Fatal("the next window must allow again")
	}
}

func TestSendConsultsTheLimiterBeforeTheStore(t *testing.T) {
	// NewService(nil) would panic if Send reached the store, so an
	// ErrRateLimited answer proves the limiter gates persistence.
	service := NewService(nil)
	service.limiter = newSendLimiter(1, time.Minute)
	if !service.limiter.Allow("tasks") {
		t.Fatal("fixture: the first slot must be available")
	}
	_, err := service.Send(context.Background(), []SendParams{validSendParams()})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("send = %v, want ErrRateLimited", err)
	}
}

// TestUserIDsForUsernames preserves the caller's order and rejects the
// WHOLE list when any name is unknown. It needs the users table, so it
// rides the store suite's LUMEN_INTEGRATION gate.
func TestUserIDsForUsernames(t *testing.T) {
	fixture := newTestFixture(t)
	service := NewService(fixture.notifications)
	ctx := context.Background()

	ids, err := service.UserIDsForUsernames(ctx, []string{"notif-other", "notif-user"})
	if err != nil {
		t.Fatalf("resolve usernames: %v", err)
	}
	if want := []string{fixture.otherID, fixture.userID}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v (order preserved)", ids, want)
	}

	deduped, err := service.UserIDsForUsernames(ctx, []string{"notif-user", "notif-user"})
	if err != nil {
		t.Fatalf("dedupe names: %v", err)
	}
	if want := []string{fixture.userID}; !slices.Equal(deduped, want) {
		t.Fatalf("deduped = %v, want %v (a repeated name is one recipient)", deduped, want)
	}

	if _, err := service.UserIDsForUsernames(ctx, []string{"notif-user", "ghost"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown name = %v, want ErrInvalid", err)
	}
	if _, err := service.UserIDsForUsernames(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty list = %v, want ErrInvalid", err)
	}
}
