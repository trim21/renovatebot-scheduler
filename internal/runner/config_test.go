package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExtraConfig(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		config, err := LoadExtraConfig()
		if err != nil {
			t.Fatal(err)
		}
		if config != nil {
			t.Fatalf("expected no config, got %q", config)
		}
	})

	t.Run("inline", func(t *testing.T) {
		t.Setenv("RENOVATE_CONFIG", `{"extends":["config:recommended"]}`)
		config, err := LoadExtraConfig()
		if err != nil {
			t.Fatal(err)
		}
		if string(config) != `{"extends":["config:recommended"]}` {
			t.Fatalf("unexpected config: %q", config)
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"onboarding":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RENOVATE_CONFIG_FILE", path)

		config, err := LoadExtraConfig()
		if err != nil {
			t.Fatal(err)
		}
		if string(config) != `{"onboarding":false}` {
			t.Fatalf("unexpected config: %q", config)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("RENOVATE_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
		if _, err := LoadExtraConfig(); err == nil {
			t.Fatal("expected an error for a missing config file")
		}
	})

	t.Run("both set", func(t *testing.T) {
		t.Setenv("RENOVATE_CONFIG", `{}`)
		t.Setenv("RENOVATE_CONFIG_FILE", "/tmp/config.json")
		if _, err := LoadExtraConfig(); err == nil {
			t.Fatal("expected an error when both sources are set")
		}
	})
}
