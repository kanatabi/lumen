// Package adminsvc is the admin API's business logic (§8/§9): sessions,
// the publish unit of work, desk/photo/vocabulary orchestration, the
// secrets relay surface. Handlers (internal/api/admin) DECODE and ENCODE;
// everything with a decision lives here — and the decisions that need no
// row (publish derivations, state machine, throttle keying) are PURE and
// unit-tested without any database.
package adminsvc

import (
	"errors"
	"time"

	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

// Sentinels handlers map onto statuses (handlers never import bao/queue —
// the vocabulary of failure is ours).
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmptyCredentials   = errors.New("username and password required")
	ErrNoMediaStore       = errors.New("media store not configured")
	ErrStaleCAS           = errors.New("stale cas — someone wrote first")
	ErrUnsupportedImage   = errors.New("undecodable or unsupported image")
)

type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string { return "too many attempts" }

type Service struct {
	store     *store.PG
	queue     *queue.Client
	engine    *search.Service // admin door: same engine, no public restrictions
	jwtSecret string
	limiter   *throttle.Limiter

	media         media.Store
	mediaEndpoint string
	mediaBucket   string
	mediaCDNBase  string
	mediaPrefix   string // LUMEN_MEDIA_PREFIX — applied once in UploadPhoto (§5)

	bao baoAPI // concrete: *bao.Client; tests: the fake in secrets_test.go
}

func New(st *store.PG, q *queue.Client, eng *search.Service, jwtSecret string, limiter *throttle.Limiter) *Service {
	return &Service{store: st, queue: q, engine: eng, jwtSecret: jwtSecret, limiter: limiter}
}

func (s *Service) EnableMedia(store media.Store, endpoint, bucket, cdnBase string) {
	s.media, s.mediaEndpoint, s.mediaBucket, s.mediaCDNBase = store, endpoint, bucket, cdnBase
}

// SetMediaPrefix scopes every uploaded object key (LUMEN_MEDIA_PREFIX,
// role split 2026-11-13): the prefix is applied once at the upload site,
// so puts, returned URLs and the stored src/ledger stay coherent. Empty
// (the default) keeps dev byte-identical.
func (s *Service) SetMediaPrefix(prefix string) { s.mediaPrefix = prefix }

func (s *Service) EnableBao(client *bao.Client) { s.bao = client }

// SecretsEnabled gates the /admin/bao mount: honest absence when the plane
// is not configured (no routes at all, not 404 theater).
func (s *Service) SecretsEnabled() bool { return s.bao != nil }
