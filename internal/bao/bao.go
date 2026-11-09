// Package bao is the secrets-plane relay (lumen §9): a MINIMAL, explicit
// surface over the KV v2 API — list metadata (policy-scoped), read (+
// version pin), version list, CAS write. The fzr-portal AppRole lives in
// lumen's env (rendered by ESO from the §5.6 seed); the portal UI NEVER
// sees these credentials — it authenticates to LUMEN, lumen authenticates
// to Bao. No sys/ route exists here by construction, not by convention.
//
// The wire API is the KV-v2/AppRole contract OpenBao inherits from its
// upstream; dev compose runs OpenBao ITSELF (ghcr-pinned — dev == prod,
// not a stand-in), and the hashicorp/vault/api Go client is OpenBao's
// documented compatibility path, not a vendor concession.
package bao

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
)

// Payload mirrors mellon's SecretPayload; Meta mirrors SecretMeta.
// Error classifies the relay's expected failures so handlers can map
// status codes without string-matching vendor messages downstream.
type ErrorKind int

const (
	ErrCheckAndSet ErrorKind = iota // stale cas (§9: conflicts are the point)
	ErrPermission
)

type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

type Payload struct {
	Data         map[string]string `json:"data"`
	CreatedTime  string            `json:"createdTime"`
	Version      int               `json:"version"`
	Destroyed    bool              `json:"destroyed"`
	DeletionTime *string           `json:"deletionTime"`
}

type VersionInfo struct {
	Version     int    `json:"version"`
	CreatedTime string `json:"createdTime"`
	Destroyed   bool   `json:"destroyed"`
}

type Meta struct {
	CurrentVersion int           `json:"currentVersion"`
	Versions       []VersionInfo `json:"versions"`
}

type Path struct {
	App string `json:"app"`
	Env string `json:"env"`
}

type Client struct {
	mu       sync.Mutex
	v        *api.Client
	addr     string
	roleID   string
	secretID string
	token    string
	expires  time.Time
}

// New builds the unauthenticated client; the AppRole login happens lazily
// (and re-happens before expiry) so a Bao blip never takes reads offline
// harder than it must.
func New() (*Client, error) {
	addr := os.Getenv("LUMEN_BAO_ADDR")
	roleID := os.Getenv("LUMEN_BAO_ROLE_ID")
	secretID := os.Getenv("LUMEN_BAO_SECRET_ID")
	if addr == "" || roleID == "" || secretID == "" {
		return nil, fmt.Errorf("LUMEN_BAO_ADDR / LUMEN_BAO_ROLE_ID / LUMEN_BAO_SECRET_ID required (§9: values in Bao-rendered env, never git)")
	}
	cfg := api.DefaultConfig()
	cfg.Address = addr
	v, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{v: v, addr: addr, roleID: roleID, secretID: secretID}, nil
}

// Enabled reports whether the plane is configured (fixture/dev mode may
// not have it — the endpoints then answer 503 honestly, never fake data).
func Enabled() bool {
	return os.Getenv("LUMEN_BAO_ADDR") != "" && os.Getenv("LUMEN_BAO_ROLE_ID") != "" &&
		os.Getenv("LUMEN_BAO_SECRET_ID") != ""
}

func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return nil
	}
	resp, err := c.v.Logical().WriteWithContext(ctx, "auth/approle/login", map[string]any{
		"role_id":   c.roleID,
		"secret_id": c.secretID,
	})
	if err != nil || resp == nil || resp.Auth == nil {
		return fmt.Errorf("approle login: %w", err)
	}
	c.token = resp.Auth.ClientToken
	ttl := resp.Auth.LeaseDuration
	c.expires = time.Now().Add(time.Duration(ttl) * time.Second)
	// the login token is not a value to log — not even masked
	return nil
}

func (c *Client) client(ctx context.Context) (*api.Client, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	clone, err := c.v.Clone()
	if err != nil {
		return nil, err
	}
	clone.SetToken(tok)
	return clone, nil
}

// ---- the four operations (§9) — nothing else is exposed ----

// ListPaths enumerates the policy-scoped mounts/paths: kv/metadata/<app>/
// → apps; each app's children are envs. Names only — the metadata LIST is
// what a listing returns; bytes never ride a list.
func (c *Client) ListPaths(ctx context.Context) ([]Path, error) {
	vc, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := c.list(ctx, vc, "kv/metadata")
	if err != nil {
		return nil, err
	}
	out := []Path{}
	for _, app := range apps {
		envs, err := c.list(ctx, vc, "kv/metadata/"+app)
		if err != nil {
			continue // an unreadable app subtree is skipped, not fatal
		}
		for _, env := range envs {
			out = append(out, Path{App: strings.TrimSuffix(app, "/"), Env: strings.TrimSuffix(env, "/")})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Env < out[j].Env
	})
	return out, nil
}

func (c *Client) list(ctx context.Context, vc *api.Client, path string) ([]string, error) {
	resp, err := vc.Logical().ListWithContext(ctx, path)
	if err != nil || resp == nil || resp.Data == nil {
		return nil, err
	}
	keys, _ := resp.Data["keys"].([]any)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Read fetches one version (0/absent = current).
func (c *Client) Read(ctx context.Context, app, env string, version int) (*Payload, error) {
	vc, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("kv/data/%s/%s", app, env)
	var resp *api.Secret
	if version > 0 {
		// the SDK treats the path literally — version is QUERY data, and
		// string-splicing it into the path was a 404 waiting to happen
		resp, err = vc.Logical().ReadWithData(path, map[string][]string{"version": {fmt.Sprint(version)}})
	} else {
		resp, err = vc.Logical().ReadWithContext(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Data == nil {
		return nil, nil
	}
	return decodePayload(resp.Data)
}

func decodePayload(d map[string]any) (*Payload, error) {
	p := &Payload{Data: map[string]string{}}
	if md, ok := d["metadata"].(map[string]any); ok {
		p.Version = int(toNum(md["version"]))
		p.CreatedTime = str(md["created_time"])
		p.Destroyed, _ = md["destroyed"].(bool)
		if dt := str(md["deletion_time"]); dt != "" {
			p.DeletionTime = &dt
		}
	}
	data, ok := d["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("malformed kv payload")
	}
	for k, v := range data {
		s, _ := v.(string)
		p.Data[k] = s
	}
	return p, nil
}

func toNum(v any) float64 {
	if f, ok := v.(json.Number); ok {
		n, _ := f.Float64()
		return n
	}
	if n, ok := v.(int); ok {
		return float64(n)
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Meta returns the version list (for the portal's history panel + CAS).
func (c *Client) Meta(ctx context.Context, app, env string) (*Meta, error) {
	vc, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := vc.Logical().Read(fmt.Sprintf("kv/metadata/%s/%s", app, env))
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Data == nil {
		return nil, nil
	}
	m := &Meta{}
	if cv, ok := resp.Data["current_version"]; ok {
		m.CurrentVersion = int(toNum(cv))
	}
	if vm, ok := resp.Data["versions"].(map[string]any); ok {
		for k, v := range vm {
			entry, _ := v.(map[string]any)
			vi := VersionInfo{Version: int(toNum(k))}
			if entry != nil {
				vi.CreatedTime = str(entry["created_time"])
				vi.Destroyed, _ = entry["destroyed"].(bool)
			}
			m.Versions = append(m.Versions, vi)
		}
		sort.Slice(m.Versions, func(i, j int) bool { return m.Versions[i].Version < m.Versions[j].Version })
	}
	if m.Versions == nil {
		m.Versions = []VersionInfo{}
	}
	return m, nil
}

// WriteCAS refuses unless cas == current version (§9: no unconditional
// puts — lost updates are not a feature the portal may lose).
func (c *Client) WriteCAS(ctx context.Context, app, env string, data map[string]string, cas int) (int, error) {
	vc, err := c.client(ctx)
	if err != nil {
		return 0, err
	}
	// cas_required is a property of the mount (set at bootstrap); every
	// write here carries an explicit cas regardless — a version-0 write
	// cannot clobber anything (Bao rejects mismatched CAS outright)
	resp, err := vc.Logical().Write(fmt.Sprintf("kv/data/%s/%s", app, env), map[string]any{
		"data": data,
		"options": map[string]any{
			"cas": cas,
		},
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "check-and-set") || strings.Contains(msg, "Cas mismatch"):
			return 0, &Error{Kind: ErrCheckAndSet, Err: err}
		case strings.Contains(msg, "permission denied"):
			return 0, &Error{Kind: ErrPermission, Err: err}
		}
		return 0, err
	}
	if resp == nil || resp.Data == nil {
		return 0, fmt.Errorf("empty write response")
	}
	// KV-v2 write responses carry version fields at the TOP level of Data
	// (the SDK unwraps one envelope) — looking for "metadata" here would
	// 500 AFTER persisting, which is exactly the trap we fell into.
	if v, ok := resp.Data["version"]; ok {
		return int(toNum(v)), nil
	}
	if md, ok := resp.Data["metadata"].(map[string]any); ok {
		return int(toNum(md["version"])), nil
	}
	return 0, fmt.Errorf("malformed write response")
}
