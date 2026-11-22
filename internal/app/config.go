package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

type Config struct {
	Addr              string
	BaseURL           string
	ForgejoURL        string
	OAuthClientID     string
	OAuthClientSecret string
	RenovateToken     string
	BotUser           string
	AllowedUsers      []string
	DataDir           string
	Debounce          time.Duration
	RunTimeout        time.Duration
	ShutdownTimeout   time.Duration
	Concurrency       int
	MaxRetries        int
	RetryBackoff      time.Duration
	Runner            runner.Kind
	StartupRun        bool
	// ScheduleInterval is the maximum time between runs of one repository.
	// Zero disables periodic runs.
	ScheduleInterval time.Duration
}

func (c *Config) secureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

func (c *Config) webhookURL(token string) string {
	return c.BaseURL + "/webhook/" + token
}

func (c *Config) callbackURL() string {
	return c.BaseURL + "/oauth/callback"
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		Addr:              getenv("ADDR", ":8080"),
		BaseURL:           strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		ForgejoURL:        strings.TrimRight(os.Getenv("FORGEJO_URL"), "/"),
		OAuthClientID:     os.Getenv("OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("OAUTH_CLIENT_SECRET"),
		RenovateToken:     os.Getenv("RENOVATE_TOKEN"),
		BotUser:           strings.TrimSpace(os.Getenv("BOT_USER")),
		DataDir:           getenv("DATA_DIR", "data"),
		Runner:            runner.Kind(getenv("RUNNER", string(runner.KindDocker))),
	}

	var problems []string
	if cfg.BaseURL == "" {
		problems = append(problems, "BASE_URL is required, e.g. https://renovate.example.com")
	}
	if cfg.ForgejoURL == "" {
		problems = append(problems, "FORGEJO_URL is required, e.g. https://git.example.com")
	}
	if cfg.OAuthClientID == "" {
		problems = append(problems, "OAUTH_CLIENT_ID is required")
	}
	if cfg.OAuthClientSecret == "" {
		problems = append(problems, "OAUTH_CLIENT_SECRET is required")
	}
	if cfg.RenovateToken == "" {
		problems = append(problems, "RENOVATE_TOKEN is required (PAT of the bot account)")
	}

	var err error
	if cfg.Debounce, err = envDuration("DEBOUNCE", 20*time.Second); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.RunTimeout, err = envDuration("RUN_TIMEOUT", 30*time.Minute); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", 5*time.Minute); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.RetryBackoff, err = envDuration("RETRY_BACKOFF", time.Minute); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.Concurrency, err = envInt("CONCURRENCY", 1); err != nil {
		problems = append(problems, err.Error())
	} else if cfg.Concurrency < 1 {
		problems = append(problems, "CONCURRENCY must be at least 1")
	}
	if cfg.MaxRetries, err = envInt("MAX_RETRIES", 2); err != nil {
		problems = append(problems, err.Error())
	} else if cfg.MaxRetries < 0 {
		problems = append(problems, "MAX_RETRIES must not be negative")
	}
	if cfg.StartupRun, err = envBool("STARTUP_RUN", true); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.ScheduleInterval, err = envDuration("SCHEDULE_INTERVAL", 24*time.Hour); err != nil {
		problems = append(problems, err.Error())
	} else if cfg.ScheduleInterval < 0 {
		problems = append(problems, "SCHEDULE_INTERVAL must not be negative")
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}

	cfg.AllowedUsers = strings.FieldsFunc(os.Getenv("ALLOWED_USERS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})

	return cfg, nil
}

func (c *Config) canLogin(login string) bool {
	if len(c.AllowedUsers) == 0 {
		return strings.EqualFold(login, c.BotUser)
	}
	for _, allowed := range c.AllowedUsers {
		if strings.EqualFold(login, allowed) {
			return true
		}
	}
	return false
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
