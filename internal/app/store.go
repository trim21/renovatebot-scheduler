package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"

	"github.com/trim21/renovatebot-scheduler/internal/ent"
	"github.com/trim21/renovatebot-scheduler/internal/ent/repo"
	"github.com/trim21/renovatebot-scheduler/internal/ent/run"
)

// TrackedRepo is a repository whose webhook we manage and whose events trigger Renovate.
type TrackedRepo struct {
	FullName      string
	DefaultBranch string
	HookID        int64
	HookToken     string
	EnabledBy     string
	EnabledAt     time.Time
}

type RunRecord struct {
	ID         string
	Repo       string
	Trigger    string
	Attempt    int
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Error      string
	Log        []byte
}

func (r RunRecord) Duration() time.Duration {
	return r.FinishedAt.Sub(r.StartedAt)
}

func (r RunRecord) Success() bool {
	return r.ExitCode == 0 && r.Error == ""
}

// Store persists tracked repositories and the run history in SQLite.
type Store struct {
	client *ent.Client
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dsn := "file:" + filepath.Join(dir, "renovate.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return &Store{client: client}, nil
}

func (s *Store) Close() error {
	return s.client.Close()
}

func (s *Store) List() []*TrackedRepo {
	rows, err := s.client.Repo.Query().Order(ent.Asc(repo.FieldFullName)).All(context.Background())
	if err != nil {
		logger.Error().Err(err).Msg("list repositories")
		return nil
	}
	out := make([]*TrackedRepo, 0, len(rows))
	for _, row := range rows {
		out = append(out, toTrackedRepo(row))
	}
	return out
}

func (s *Store) Get(fullName string) (*TrackedRepo, bool) {
	row, err := s.client.Repo.Query().Where(repo.FullNameEQ(fullName)).Only(context.Background())
	if err != nil {
		if !ent.IsNotFound(err) {
			logger.Error().Err(err).Str("repo", fullName).Msg("get repository")
		}
		return nil, false
	}
	return toTrackedRepo(row), true
}

func (s *Store) GetByToken(token string) (*TrackedRepo, bool) {
	row, err := s.client.Repo.Query().Where(repo.HookTokenEQ(token)).Only(context.Background())
	if err != nil {
		if !ent.IsNotFound(err) {
			logger.Error().Err(err).Msg("get repository by hook token")
		}
		return nil, false
	}
	return toTrackedRepo(row), true
}

func (s *Store) Put(t *TrackedRepo) error {
	ctx := context.Background()
	exists, err := s.client.Repo.Query().Where(repo.FullNameEQ(t.FullName)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("look up repository: %w", err)
	}
	if exists {
		return s.client.Repo.Update().
			Where(repo.FullNameEQ(t.FullName)).
			SetDefaultBranch(t.DefaultBranch).
			SetHookID(t.HookID).
			SetHookToken(t.HookToken).
			SetEnabledBy(t.EnabledBy).
			SetEnabledAt(t.EnabledAt).
			Exec(ctx)
	}
	_, err = s.client.Repo.Create().
		SetFullName(t.FullName).
		SetDefaultBranch(t.DefaultBranch).
		SetHookID(t.HookID).
		SetHookToken(t.HookToken).
		SetEnabledBy(t.EnabledBy).
		SetEnabledAt(t.EnabledAt).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create repository: %w", err)
	}
	return nil
}

func (s *Store) Delete(fullName string) error {
	_, err := s.client.Repo.Delete().Where(repo.FullNameEQ(fullName)).Exec(context.Background())
	return err
}

func (s *Store) AppendRun(record RunRecord) error {
	builder := s.client.Run.Create().
		SetID(record.ID).
		SetRepo(record.Repo).
		SetTrigger(record.Trigger).
		SetAttempt(record.Attempt).
		SetStartedAt(record.StartedAt).
		SetFinishedAt(record.FinishedAt).
		SetExitCode(record.ExitCode).
		SetError(record.Error)
	if len(record.Log) > 0 {
		builder = builder.SetLog(record.Log)
	}
	if _, err := builder.Save(context.Background()); err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// RecentRuns returns the most recent records, newest first, without their logs.
func (s *Store) RecentRuns(limit int) ([]RunRecord, error) {
	query := s.client.Run.Query().
		Select(
			run.FieldID,
			run.FieldRepo,
			run.FieldTrigger,
			run.FieldAttempt,
			run.FieldStartedAt,
			run.FieldFinishedAt,
			run.FieldExitCode,
			run.FieldError,
		).
		Order(ent.Desc(run.FieldStartedAt))
	if limit > 0 {
		query = query.Limit(limit)
	}

	rows, err := query.All(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRunRecord(row))
	}
	return out, nil
}

func (s *Store) FindRun(id string) (RunRecord, bool) {
	row, err := s.client.Run.Query().Where(run.IDEQ(id)).Only(context.Background())
	if err != nil {
		if !ent.IsNotFound(err) {
			logger.Error().Err(err).Str("run", id).Msg("find run")
		}
		return RunRecord{}, false
	}
	return toRunRecord(row), true
}

// ReadRunLog returns the last maxLines lines of a run log.
func (s *Store) ReadRunLog(id string, maxLines int) (string, error) {
	row, err := s.client.Run.Query().Select(run.FieldLog).Where(run.IDEQ(id)).Only(context.Background())
	if err != nil {
		return "", err
	}
	return lastLines(row.Log, maxLines), nil
}

func toTrackedRepo(row *ent.Repo) *TrackedRepo {
	return &TrackedRepo{
		FullName:      row.FullName,
		DefaultBranch: row.DefaultBranch,
		HookID:        row.HookID,
		HookToken:     row.HookToken,
		EnabledBy:     row.EnabledBy,
		EnabledAt:     row.EnabledAt,
	}
}

func toRunRecord(row *ent.Run) RunRecord {
	return RunRecord{
		ID:         row.ID,
		Repo:       row.Repo,
		Trigger:    row.Trigger,
		Attempt:    row.Attempt,
		StartedAt:  row.StartedAt,
		FinishedAt: row.FinishedAt,
		ExitCode:   row.ExitCode,
		Error:      row.Error,
		Log:        row.Log,
	}
}

func lastLines(data []byte, maxLines int) string {
	text := strings.TrimRight(string(data), "\n")
	if maxLines <= 0 || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return hex.EncodeToString(buf)
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405") + "-" + randomToken(3)
}
