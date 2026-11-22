package app

import (
	"context"
	"time"
)

// scheduleCheckInterval is how often due repositories are looked for. The check
// only queries the database, so it is cheap enough to run every minute.
const scheduleCheckInterval = time.Minute

// Enqueuer is the part of the scheduler the periodic run loop needs.
type Enqueuer interface {
	Enqueue(repo, trigger string, urgent bool) bool
}

// runScheduleLoop queues every repository that has not run within interval,
// first immediately and then on every checkInterval tick.
func runScheduleLoop(ctx context.Context, store *Store, enq Enqueuer, interval, checkInterval time.Duration) {
	enqueueStaleRepos(store, enq, interval, time.Now())

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enqueueStaleRepos(store, enq, interval, time.Now())
		}
	}
}

// enqueueStaleRepos queues the repositories whose last run started before
// now-interval, including repositories that never ran.
func enqueueStaleRepos(store *Store, enq Enqueuer, interval time.Duration, now time.Time) {
	recent, err := store.ReposRunSince(now.Add(-interval))
	if err != nil {
		logger.Error().Err(err).Msg("check repositories due for a run")
		return
	}

	queued := 0
	for _, repo := range store.List() {
		if _, ok := recent[repo.FullName]; ok {
			continue
		}
		if enq.Enqueue(repo.FullName, "schedule", true) {
			queued++
			logger.Info().Str("repo", repo.FullName).Msg("scheduled run due")
		}
	}
	if queued > 0 {
		logger.Info().Int("repositories", queued).Msg("queued scheduled runs")
	}
}
