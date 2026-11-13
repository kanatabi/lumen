// Package config resolves runtime configuration with viper (owner ruling,
// 2026-11-08). Precedence is deliberate — env > file > default — matching the
// deployment truth: pods receive ESO-rendered ENVIRONMENT (INFRA §5.3), while
// local dev may drop a gitignored env.yaml for comfort. The sample (env.yaml.sample)
// documents KEYS; values live in Bao, never in git.
//
// There is NO autoload of .env files (godotenv-style): the production contract
// is real environment variables, and a loader that quietly invents them from a
// dotfile is how "works on my machine" reaches a cluster.
//
// The role split (owner ruling 2026-11-13) made Bindings the single source:
// every key declares WHICH PLANES bind it, Load populates the struct only for
// its role's keys, and `lumen config keys --role X` prints the same table —
// the machine-readable bindings surface (epoch-agent P3). A cms process does
// NOT read the signing secret; an admin process does not read media/meili
// keys. Values left unbound are the zero string, never a borrowed secret.
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"github.com/kanatabi/lumen/internal/role"
)

type Config struct {
	// Role is resolved FIRST (meta key): it decides which of the bindings
	// below bind at all. Unset LUMEN_ROLE = the legacy "all" dev plane.
	Role role.Role

	Port          string // default 8080
	DBURL         string // postgres://… (required — there is no fixture mode)
	Origin        string // absolute base (feed/D2; the cms plane's public reads)
	MeiliURL      string // read-serving engine (§4) — required on the cms plane
	MeiliKey      string // master key (dev placeholder in compose; Bao in prod)
	SessionSecret string // HS256 key, 32B, Bao lumen/{env} — ADMIN-PLANE ONLY (§8)
	AdminURL      string // cms plane: base URL of the admin plane to introspect (§8 verify)

	// Meilisearch index uids (role split: planes may live in different
	// engine deployments; dev defaults keep the loop unchanged).
	MeiliIndexPosts  string // LUMEN_MEILI_INDEX_POSTS, default "posts"
	MeiliIndexPhotos string // LUMEN_MEILI_INDEX_PHOTOS, default "photos"
	// The tags index stays constant ("tags"): the ruling enumerates only
	// posts/photos; tags is the engine's shared vocabulary surface.

	// Media (D1-revised, §5): S3-compatible object store + CDN base.
	// VALUES live in Bao lumen/{env}; provider choice is HUMAN-GATE #9 —
	// this struct is provider-agnostic by construction.
	Media Media
}

type Media struct {
	Endpoint     string
	Region       string
	Bucket       string
	CDNBase      string
	AccessKey    string
	SecretKey    string
	ForcePath    bool
	CreateBucket bool
	// Prefix scopes every object key (LUMEN_MEDIA_PREFIX, default ""):
	// it is applied to ALL puts AND to the returned URL composition, so
	// base+prefix+key stays coherent and two planes can share a bucket
	// with separate key spaces. Empty = byte-identical to the old shape.
	Prefix string
}

// Binding declares one config key with the env names it reads and the
// PLANES that bind it at all. This table IS the truth — Load, the keys
// verb, and env.yaml.sample (its doc mirror) all derive from it.
// Consumed marks keys the viper struct does not carry because a lower
// package reads the env directly (internal/bao) — declared here so
// `config keys` is COMPLETE for a plane, never a guess.
type Binding struct {
	Key      string
	Envs     []string
	Planes   role.Planes
	Note     string
	consumed bool // read outside this package; struct stays empty
}

const bothPlanes = role.PlanesAll

var Bindings = []Binding{
	{Key: "role", Envs: []string{"LUMEN_ROLE"}, Planes: bothPlanes},
	{Key: "config", Envs: []string{"LUMEN_CONFIG"}, Planes: bothPlanes},
	{Key: "db_url", Envs: []string{"LUMEN_DB_URL"}, Planes: bothPlanes},
	{Key: "port", Envs: []string{"LUMEN_PORT", "PORT"}, Planes: bothPlanes},
	{Key: "session_jwt_secret", Envs: []string{"LUMEN_SESSION_JWT_SECRET"}, Planes: role.PlaneAdmin},
	{Key: "admin_url", Envs: []string{"LUMEN_ADMIN_URL"}, Planes: role.PlaneCMS},
	{Key: "origin", Envs: []string{"LUMEN_ORIGIN"}, Planes: role.PlaneCMS},
	{Key: "meili_url", Envs: []string{"LUMEN_MEILI_URL"}, Planes: role.PlaneCMS},
	{Key: "meili_key", Envs: []string{"LUMEN_MEILI_KEY"}, Planes: role.PlaneCMS},
	{Key: "meili_index_posts", Envs: []string{"LUMEN_MEILI_INDEX_POSTS"}, Planes: role.PlaneCMS},
	{Key: "meili_index_photos", Envs: []string{"LUMEN_MEILI_INDEX_PHOTOS"}, Planes: role.PlaneCMS},
	{Key: "media_endpoint", Envs: []string{"LUMEN_MEDIA_ENDPOINT"}, Planes: role.PlaneCMS},
	{Key: "media_region", Envs: []string{"LUMEN_MEDIA_REGION"}, Planes: role.PlaneCMS},
	{Key: "media_bucket", Envs: []string{"LUMEN_MEDIA_BUCKET"}, Planes: role.PlaneCMS},
	{Key: "media_cdn_base", Envs: []string{"LUMEN_MEDIA_CDN_BASE"}, Planes: role.PlaneCMS},
	{Key: "media_access_key", Envs: []string{"LUMEN_MEDIA_ACCESS_KEY"}, Planes: role.PlaneCMS},
	{Key: "media_secret_key", Envs: []string{"LUMEN_MEDIA_SECRET_KEY"}, Planes: role.PlaneCMS},
	{Key: "media_force_path", Envs: []string{"LUMEN_MEDIA_FORCE_PATH"}, Planes: role.PlaneCMS},
	{Key: "media_create_bucket", Envs: []string{"LUMEN_MEDIA_CREATE_BUCKET"}, Planes: role.PlaneCMS, Note: "dev-only"},
	{Key: "media_prefix", Envs: []string{"LUMEN_MEDIA_PREFIX"}, Planes: role.PlaneCMS},
	// the §9 vault door — internal/bao.New reads these directly (lazy
	// AppRole login); declared so the admin plane's key set is complete
	{Key: "bao_addr", Envs: []string{"LUMEN_BAO_ADDR"}, Planes: role.PlaneAdmin, consumed: true},
	{Key: "bao_role_id", Envs: []string{"LUMEN_BAO_ROLE_ID"}, Planes: role.PlaneAdmin, consumed: true},
	{Key: "bao_secret_id", Envs: []string{"LUMEN_BAO_SECRET_ID"}, Planes: role.PlaneAdmin, consumed: true},
}

// EnvKeys is the machine-readable bindings surface for `lumen config keys`:
// every LUMEN_* env NAME the role actually binds, sorted. Plain PORT (the
// workspace alias) is deliberately absent — the command's contract is the
// LUMEN_* family.
func EnvKeys(r role.Role) []string {
	seen := map[string]bool{}
	for _, b := range Bindings {
		if !b.Planes.Has(r.Planes()) {
			continue
		}
		for _, e := range b.Envs {
			if strings.HasPrefix(e, "LUMEN_") {
				seen[e] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Load reads configuration for the resolved process role. searchPath is an
// optional explicit env.yaml path (LUMEN_CONFIG); when empty it looks for
// ./env.yaml without failing if absent.
func Load() (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("LUMEN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv() // LUMEN_DB_URL → db_url, PORT → port, ORIGIN → origin…

	// meta keys bind for every role — they DECIDE the rest
	_ = v.BindEnv("role", "LUMEN_ROLE")
	_ = v.BindEnv("config", "LUMEN_CONFIG")

	// role resolution: env now, file after it loads; validated fail-fast —
	// a typo in LUMEN_ROLE must kill the pod, not silently serve "all"
	rt := role.Role(v.GetString("role"))

	v.SetDefault("port", "8080")
	// PORT is the workspace's env convention (earendil/mellon containers use
	// it; vortex manifests will too) — the binding table carries BOTH names,
	// LUMEN_-prefixed wins by convention, plain PORT by reality.
	v.SetDefault("meili_index_posts", "posts")
	v.SetDefault("meili_index_photos", "photos")

	// the dev file: explicit via LUMEN_CONFIG, opportunistic via ./env.yaml —
	// env vars always WIN (viper's native precedence: env > config > defaults)
	if explicit := v.GetString("config"); explicit != "" {
		v.SetConfigFile(explicit)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("config file: %w", err) // explicit = must exist
		}
	} else {
		v.SetConfigName("env")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		_ = v.ReadInConfig() // absent is fine: pure-env mode (production)
	}
	if v.GetString("role") != "" { // the file may carry it too (dev comfort)
		rt = role.Role(v.GetString("role"))
	}
	// empty rt stays role.All — Parse's documented legacy default
	rt, err := role.Parse(string(rt))
	if err != nil {
		return Config{}, err
	}

	// bind ONLY this role's keys: an unbound key reads nothing, so a cms
	// process cannot see LUMEN_SESSION_JWT_SECRET even if the pod env has
	// one (ruling: remove it from cms bindings)
	bound := map[string]bool{}
	for _, b := range Bindings {
		if !b.Planes.Has(rt.Planes()) || b.consumed {
			continue
		}
		_ = v.BindEnv(append([]string{b.Key}, b.Envs...)...)
		bound[b.Key] = true
	}
	get := func(key string) string {
		if bound[key] {
			return v.GetString(key)
		}
		return "" // unbound = the zero value, never a borrowed secret
	}
	getBool := func(key string) bool { return bound[key] && v.GetBool(key) }

	c := Config{
		Role:             rt,
		Port:             v.GetString("port"),
		DBURL:            get("db_url"),
		Origin:           get("origin"),
		MeiliURL:         get("meili_url"),
		MeiliKey:         get("meili_key"),
		SessionSecret:    get("session_jwt_secret"),
		AdminURL:         get("admin_url"),
		MeiliIndexPosts:  get("meili_index_posts"),
		MeiliIndexPhotos: get("meili_index_photos"),
		Media: Media{
			Endpoint:     get("media_endpoint"),
			Region:       get("media_region"),
			Bucket:       get("media_bucket"),
			CDNBase:      get("media_cdn_base"),
			AccessKey:    get("media_access_key"),
			SecretKey:    get("media_secret_key"),
			ForcePath:    getBool("media_force_path"),
			CreateBucket: getBool("media_create_bucket"),
			Prefix:       get("media_prefix"),
		},
	}
	if c.DBURL == "" {
		return c, fmt.Errorf("db_url (LUMEN_DB_URL) required — fixture mode was retired (2026-11-09 real-by-default ruling); the dev loop is 'cd ../lumen && make dev'")
	}
	return c, nil
}

// ValidateServer is the HTTP-role requirement check per PLANE (role split,
// 2026-11-13): the admin plane mints sessions and must hold the signing
// key; the cms plane mints NOTHING (auth is introspected) and must know
// WHERE to introspect — LUMEN_ADMIN_URL. The legacy "all" plane keeps the
// old rule (it embeds the admin plane). A missing requirement is a start-up
// death, never a 500 discovered at first login.
func (c Config) ValidateServer() error {
	switch c.Role {
	case role.Admin, role.All:
		if c.SessionSecret == "" {
			return fmt.Errorf("session_jwt_secret (LUMEN_SESSION_JWT_SECRET) required to serve the admin plane")
		}
	case role.CMS:
		if c.AdminURL == "" {
			return fmt.Errorf("LUMEN_ADMIN_URL required on the cms plane — bearer tokens are introspected against the admin plane (fail-closed design; the cms plane holds no signing key by ruling)")
		}
	}
	return nil
}
