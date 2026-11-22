package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestParseMounts(t *testing.T) {
	mounts, err := ParseMounts("")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected the default cache mount, got %+v", mounts)
	}
	got := mounts[0]
	if got.Type != MountVolume || got.Source != "renovate-cache" || got.Target != "/tmp/renovate/cache" || got.ReadOnly {
		t.Fatalf("unexpected default mount: %+v", got)
	}

	mounts, err = ParseMounts("renovate-cache:/tmp/renovate/cache, /srv/renovate/config.js:/usr/local/etc/renovate/config.js:ro")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected two mounts, got %+v", mounts)
	}
	if mounts[0].Type != MountVolume || mounts[0].ReadOnly {
		t.Fatalf("unexpected volume mount: %+v", mounts[0])
	}
	if mounts[1].Type != MountBind || !mounts[1].ReadOnly {
		t.Fatalf("unexpected bind mount: %+v", mounts[1])
	}

	disabled, err := ParseMounts("-")
	if err != nil || len(disabled) != 0 {
		t.Fatalf("expected no mounts for -, got %+v (err %v)", disabled, err)
	}

	for _, raw := range []string{
		"cache",
		"cache:/cache:rw",
		"./relative:/cache",
		"cache:relative",
		"a:/a:ro:b",
	} {
		if _, err := ParseMounts(raw); err == nil {
			t.Fatalf("expected an error for %q", raw)
		}
	}
}

func TestConfigArchive(t *testing.T) {
	content := []byte(`{"extends":["config:recommended"]}`)
	archive, err := configArchive(content)
	if err != nil {
		t.Fatal(err)
	}

	reader := tar.NewReader(bytes.NewReader(archive))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "renovate-config.json" {
		t.Fatalf("unexpected entry name: %q", header.Name)
	}
	if header.Mode != 0o600 {
		t.Fatalf("unexpected mode: %o", header.Mode)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("RENOVATE_PULL", "always")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PullPolicy != PullAlways {
		t.Fatalf("unexpected pull policy: %q", cfg.PullPolicy)
	}

	t.Setenv("RENOVATE_PULL", "sometimes")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected an error for an invalid pull policy")
	}
}
