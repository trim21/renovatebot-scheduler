package docker

import (
	"fmt"
	"os"
	"strings"
)

// PullPolicy controls when the Renovate image is pulled from its registry.
type PullPolicy string

const (
	PullMissing PullPolicy = "missing"
	PullAlways  PullPolicy = "always"
	PullNever   PullPolicy = "never"
)

// MountType is the kind of mount passed to the Renovate container.
type MountType string

const (
	MountVolume MountType = "volume"
	MountBind   MountType = "bind"
)

// Mount is one container mount passed to the Renovate container.
// Bind mount sources are resolved by the Docker daemon and therefore must be
// absolute paths on the Docker host, not inside the scheduler container.
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool
}

// Config holds the Docker runner settings. Endpoint and Token are Renovate
// runtime settings injected by the caller, everything else comes from the
// environment through FromEnv.
type Config struct {
	Host       string
	Image      string
	PullPolicy PullPolicy
	Mounts     []Mount
	Endpoint   string
	Token      string
	LogLevel   string
	// ExtraConfig is written into the container and referenced through
	// RENOVATE_CONFIG_FILE.
	ExtraConfig []byte
}

// FromEnv reads the Docker-specific environment variables.
func FromEnv() (Config, error) {
	cfg := Config{
		Host:       os.Getenv("DOCKER_HOST"),
		Image:      getenv("RENOVATE_IMAGE", "ghcr.io/renovatebot/renovate:latest"),
		PullPolicy: PullPolicy(getenv("RENOVATE_PULL", string(PullMissing))),
		LogLevel:   getenv("RENOVATE_LOG_LEVEL", "info"),
	}

	switch cfg.PullPolicy {
	case PullMissing, PullAlways, PullNever:
	default:
		return Config{}, fmt.Errorf(`RENOVATE_PULL must be one of "missing", "always", "never"`)
	}

	mounts, err := ParseMounts(os.Getenv("RENOVATE_MOUNTS"))
	if err != nil {
		return Config{}, err
	}
	cfg.Mounts = mounts

	return cfg, nil
}

// ParseMounts parses RENOVATE_MOUNTS, a comma separated list of
// source:target[:ro] entries. A source starting with / is a bind mount, any
// other source is a named volume. "-" disables the default cache volume.
func ParseMounts(raw string) ([]Mount, error) {
	raw = strings.TrimSpace(raw)
	if raw == "-" {
		return nil, nil
	}
	if strings.TrimSpace(raw) == "" {
		return []Mount{{
			Type:   MountVolume,
			Source: "renovate-cache",
			Target: "/tmp/renovate/cache",
		}}, nil
	}

	parts := strings.Split(raw, ",")
	mounts := make([]Mount, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) < 2 || len(fields) > 3 {
			return nil, fmt.Errorf("RENOVATE_MOUNTS: invalid entry %q, want source:target[:ro]", part)
		}

		m := Mount{Source: fields[0], Target: fields[1]}
		if len(fields) == 3 {
			if fields[2] != "ro" {
				return nil, fmt.Errorf("RENOVATE_MOUNTS: invalid option %q in %q, want ro", fields[2], part)
			}
			m.ReadOnly = true
		}
		if m.Source == "" || m.Target == "" || !strings.HasPrefix(m.Target, "/") {
			return nil, fmt.Errorf("RENOVATE_MOUNTS: invalid entry %q, want source:target[:ro]", part)
		}

		switch {
		case strings.HasPrefix(m.Source, "/"):
			m.Type = MountBind
		case strings.HasPrefix(m.Source, "."):
			return nil, fmt.Errorf("RENOVATE_MOUNTS: bind mount source %q must be an absolute path on the Docker host", m.Source)
		default:
			m.Type = MountVolume
		}

		mounts = append(mounts, m)
	}

	return mounts, nil
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
