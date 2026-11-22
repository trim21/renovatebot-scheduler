package app

import (
	"context"
	"sync"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

// Scheduler coalesces webhook events per repository.
//
// Each repository has a single worker. Events first collect for the debounce
// window, so a burst of pushes results in one run. Events that arrive while a
// run is in progress queue another run instead of being dropped, which is what
// keeps the scheduler from losing updates while still avoiding a run per event.
type Scheduler struct {
	executor Executor
	debounce time.Duration
	retries  int
	backoff  time.Duration

	sem       chan struct{}
	runCtx    context.Context
	runCancel context.CancelFunc
	stopCh    chan struct{}
	stopOnce  sync.Once

	mu      sync.Mutex
	workers map[string]*repoWorker
	wg      sync.WaitGroup
}

func NewScheduler(executor Executor, debounce time.Duration, concurrency, retries int, backoff time.Duration) *Scheduler {
	runCtx, runCancel := context.WithCancel(context.Background())
	return &Scheduler{
		executor:  executor,
		debounce:  debounce,
		retries:   retries,
		backoff:   backoff,
		sem:       make(chan struct{}, concurrency),
		runCtx:    runCtx,
		runCancel: runCancel,
		stopCh:    make(chan struct{}),
		workers:   make(map[string]*repoWorker),
	}
}

// Enqueue schedules a run for repo. It returns false once the scheduler is
// stopping. urgent skips the debounce window.
func (s *Scheduler) Enqueue(repo, trigger string, urgent bool) bool {
	select {
	case <-s.stopCh:
		return false
	default:
	}

	s.mu.Lock()
	worker, ok := s.workers[repo]
	if !ok {
		worker = &repoWorker{
			sched: s,
			repo:  repo,
			wake:  make(chan struct{}, 1),
			state: WorkerStateIdle,
		}
		s.workers[repo] = worker
		s.wg.Add(1)
		go worker.loop()
	}
	s.mu.Unlock()

	worker.enqueue(trigger, urgent)
	return true
}

// Stop stops accepting events and waits for in-flight runs. Queued runs that
// never started are dropped; STARTUP_RUN compensates for them on the next boot.
func (s *Scheduler) Stop(timeout time.Duration) {
	s.stopOnce.Do(func() { close(s.stopCh) })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		s.runCancel()
		<-done
	}
	s.runCancel()
}

// WorkerState is the lifecycle state of one repository's worker.
type WorkerState string

const (
	WorkerStateIdle    WorkerState = "idle"
	WorkerStateQueued  WorkerState = "queued"
	WorkerStateRunning WorkerState = "running"
)

type RepoStatus struct {
	Repo       string
	State      WorkerState
	Queued     int
	Trigger    string
	LastFinish time.Time
	LastExit   int
	LastError  string
}

func (s *Scheduler) Status(repo string) RepoStatus {
	s.mu.Lock()
	worker, ok := s.workers[repo]
	s.mu.Unlock()
	if !ok {
		return RepoStatus{Repo: repo, State: WorkerStateIdle}
	}
	return worker.status()
}

func (s *Scheduler) Statuses() map[string]RepoStatus {
	s.mu.Lock()
	workers := make([]*repoWorker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.mu.Unlock()

	out := make(map[string]RepoStatus, len(workers))
	for _, worker := range workers {
		out[worker.repo] = worker.status()
	}
	return out
}

type repoWorker struct {
	sched *Scheduler
	repo  string
	wake  chan struct{}

	mu         sync.Mutex
	state      WorkerState
	trigger    string
	queued     int
	urgent     bool
	lastFinish time.Time
	lastExit   int
	lastError  string
}

func (w *repoWorker) enqueue(trigger string, urgent bool) {
	w.mu.Lock()
	if trigger != "" {
		w.trigger = trigger
	}
	w.queued++
	if urgent {
		w.urgent = true
	}
	if w.state == WorkerStateIdle {
		w.state = WorkerStateQueued
	}
	w.mu.Unlock()

	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *repoWorker) status() RepoStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return RepoStatus{
		Repo:       w.repo,
		State:      w.state,
		Queued:     w.queued,
		Trigger:    w.trigger,
		LastFinish: w.lastFinish,
		LastExit:   w.lastExit,
		LastError:  w.lastError,
	}
}

func (w *repoWorker) takeUrgent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	urgent := w.urgent
	w.urgent = false
	return urgent
}

func (w *repoWorker) takeTrigger() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trigger
}

func (w *repoWorker) setRunning() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = WorkerStateRunning
	w.queued = 0
	w.urgent = false
}

func (w *repoWorker) setStopped() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastFinish = time.Now()
	if w.queued > 0 {
		w.state = WorkerStateQueued
	} else {
		w.state = WorkerStateIdle
	}
}

func (w *repoWorker) loop() {
	defer w.sched.wg.Done()

	for {
		select {
		case <-w.sched.stopCh:
			return
		case <-w.wake:
		}

		delay := w.sched.debounce
		if w.takeUrgent() {
			delay = 0
		}
		timer := time.NewTimer(delay)

	debounce:
		for {
			select {
			case <-w.sched.stopCh:
				timer.Stop()
				return
			case <-w.wake:
				if w.takeUrgent() {
					break debounce
				}
				timer.Reset(w.sched.debounce)
			case <-timer.C:
				break debounce
			}
		}
		timer.Stop()

		w.execute()
	}
}

func (w *repoWorker) execute() {
	w.setRunning()
	defer w.setStopped()

	trigger := w.takeTrigger()
	for attempt := 1; attempt <= w.sched.retries+1; attempt++ {
		select {
		case w.sched.sem <- struct{}{}:
		case <-w.sched.stopCh:
			return
		}

		result := w.sched.executor.Run(w.sched.runCtx, runner.RunRequest{
			Repo:    w.repo,
			Trigger: trigger,
			Attempt: attempt,
		})
		<-w.sched.sem

		w.mu.Lock()
		w.lastExit = result.ExitCode
		w.lastError = errorString(result.Err)
		w.mu.Unlock()

		if result.Err == nil && result.ExitCode == 0 {
			return
		}
		if attempt > w.sched.retries {
			return
		}
		if !sleepContext(w.sched.runCtx, time.Duration(attempt)*w.sched.backoff) {
			return
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
