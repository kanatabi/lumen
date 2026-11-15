// The sample/config package's guards (2026-11-15): env.yaml.sample is a
// LOADABLE template — a dev runs `make config-init` and the loader parses
// it clean. The guards keep it honest: every uncommented key is a known
// binding (a typo'd key would be READ BY NOTHING — silent drift), the
// secret keys stay valueless (a committed value is a doctrine break), and
// git itself agrees env.yaml is ignored while the sample stays tracked.
package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

const samplePath = "../../env.yaml.sample"

// keysLine matches an uncommented top-level `key:` — the loadable surface.
var keysLine = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):`)

func TestEnvSampleIsLoadable(t *testing.T) {
	v := viper.New()
	v.SetConfigFile(samplePath)
	v.SetConfigType("yaml") // the extension is .sample — the CONTENT is yaml
	if err := v.ReadInConfig(); err != nil {
		t.Fatalf("the sample must parse clean YAML: %v", err)
	}
	if v.GetString("port") != "8080" {
		t.Errorf("the sample's defaults must be readable as-is: port=%q", v.GetString("port"))
	}
	if v.GetString("metrics_port") != "9090" {
		t.Errorf("metrics_port default: %q", v.GetString("metrics_port"))
	}
}

func TestEnvSampleKeysAreBindings(t *testing.T) {
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, b := range Bindings {
		known[b.Key] = true
	}
	for i, line := range strings.Split(string(raw), "\n") {
		m := keysLine.FindStringSubmatch(line)
		if m == nil {
			continue // comments, blanks, the env-only/CLI-only prose lists
		}
		if !known[m[1]] {
			t.Errorf("sample line %d: key %q is not a known binding — it would be READ BY NOTHING (typo, or the binding table moved)", i+1, m[1])
		}
	}
}

func TestEnvSampleSecretsAreValueless(t *testing.T) {
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.SetConfigFile(samplePath)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	// every key that carries a credential must PRESENT (so the dev sees
	// the fill-in slot) but EMPTY (a committed value is a doctrine break)
	for _, secret := range []string{"db_url", "session_jwt_secret", "meili_key", "media_access_key", "media_secret_key"} {
		if !v.IsSet(secret) {
			t.Errorf("secret %q must be PRESENT in the sample as the fill-in slot", secret)
			continue
		}
		if got := v.GetString(secret); got != "" {
			_ = raw // keep the read honest (file exists check above)
			t.Errorf("secret %q carries a VALUE in the sample — a doctrine break; empty it", secret)
		}
	}
}

func TestGitignoreCoversEnvYamlButNotTheSample(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH — the ignore semantics are git's, a text match would lie")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string) (ignored bool, why string) {
		out, err := exec.Command("git", "-C", repoRoot, "check-ignore", "-v", name).Output()
		return err == nil, strings.TrimSpace(string(out))
	}
	if ignored, why := run("env.yaml"); !ignored {
		t.Errorf("env.yaml must be IGNORED (the loadable copy carries values the moment a dev fills it): %s", why)
	}
	if ignored, why := run("env.yaml.sample"); ignored {
		t.Errorf("env.yaml.sample must stay TRACKED (valueless by design): %s", why)
	}
}
