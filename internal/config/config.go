// Package config resolves runtime configuration with viper (owner ruling,
// 2026-11-08). Precedence is deliberate — env > file > default — matching the
// deployment truth: pods receive ESO-rendered ENVIRONMENT (INFRA §5.3), while
// local dev may drop a gitignored env.yaml for comfort. The sample (env.yaml.sample)
// documents KEYS; values live in Bao, never in git.
//
// There is NO autoload of .env files (godotenv-style): the production contract
// is real environment variables, and a loader that quietly invents them from a
// dotfile is how "works on my machine" reaches a cluster.
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Port        string // default 8080
	DBURL       string // postgres://… (required unless Fixture)
	Origin      string // absolute base (feed/D2 later; unused in M1)
	FixturePath string // dev escape hatch: in-memory twin instead of Postgres
}

// Load reads configuration. searchPath is an optional explicit env.yaml path
// (LUMEN_CONFIG); when empty it looks for ./env.yaml without failing if absent.
func Load() (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("LUMEN")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv() // LUMEN_DB_URL → db_url, PORT → port, ORIGIN → origin…

	v.SetDefault("port", "8080")
	// PORT is the workspace's env convention (earendil/mellon containers use
	// it; vortex manifests will too) — bind BOTH names, LUMEN_-prefixed wins
	// by convention, plain PORT by reality. Multiple names = ordered fallback.
	_ = v.BindEnv("port", "LUMEN_PORT", "PORT")
	for _, key := range []string{"db_url", "origin", "fixture", "config"} {
		_ = v.BindEnv(key)
	}

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

	c := Config{
		Port:        v.GetString("port"),
		DBURL:       v.GetString("db_url"),
		Origin:      v.GetString("origin"),
		FixturePath: v.GetString("fixture"),
	}
	if c.DBURL == "" && c.FixturePath == "" {
		return c, fmt.Errorf("db_url (LUMEN_DB_URL) required (or fixture/LUMEN_FIXTURE for offline dev)")
	}
	return c, nil
}
