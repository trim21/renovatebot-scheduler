package docker

import "testing"

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
