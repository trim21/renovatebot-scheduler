package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func Main() int {
	if err := Run(); err != nil {
		logger.Error().Err(err).Msg("startup failed")
		return 1
	}
	return 0
}

func Run() error {
	logger = newLogger()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error().Err(err).Msg("close store")
		}
	}()

	if n, err := store.FailStaleRuns(); err != nil {
		logger.Warn().Err(err).Msg("mark interrupted runs as failed")
	} else if n > 0 {
		logger.Warn().Int("runs", n).Msg("marked interrupted runs as failed")
	}

	forgejo := NewForgejoClient(cfg.ForgejoURL)

	botUser := cfg.BotUser
	if botUser == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		botUser, err = forgejo.CurrentUser(ctx, cfg.RenovateToken)
		cancel()
		if err != nil {
			return fmt.Errorf("resolve BOT_USER with RENOVATE_TOKEN: %w", err)
		}
		logger.Info().Str("user", botUser).Msg("resolved renovate bot account")
	}
	if len(cfg.AllowedUsers) == 0 {
		logger.Warn().Str("user", botUser).Msg("ALLOWED_USERS is not set, only the bot account can sign in")
	}

	executor, err := NewRunner(cfg, store)
	if err != nil {
		return err
	}

	if preparer, ok := executor.(interface {
		Prepare(context.Context, io.Writer) error
	}); ok {
		prepareCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := preparer.Prepare(prepareCtx, os.Stderr); err != nil {
			logger.Warn().Err(err).Msg("runner preparation failed, retrying on the first run")
		}
		cancel()
	}

	scheduler := NewScheduler(executor, cfg.Debounce, cfg.Concurrency, cfg.MaxRetries, cfg.RetryBackoff)
	auth := NewAuth(cfg, forgejo)
	server := NewServer(cfg, store, forgejo, auth, scheduler, botUser)

	if cfg.StartupRun {
		repos := store.List()
		for _, repo := range repos {
			scheduler.Enqueue(repo.FullName, "startup", true)
		}
		if len(repos) > 0 {
			logger.Info().Int("repositories", len(repos)).Msg("queued startup runs")
		}
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info().
			Str("addr", cfg.Addr).
			Str("base_url", cfg.BaseURL).
			Str("runner", string(cfg.Runner)).
			Msg("listening")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case err := <-serveErr:
		return err
	case sig := <-signals:
		logger.Info().Str("signal", sig.String()).Msg("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn().Err(err).Msg("http shutdown")
	}
	scheduler.Stop(cfg.ShutdownTimeout)
	return nil
}
