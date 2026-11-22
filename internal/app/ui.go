package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Server struct {
	cfg     *Config
	store   *Store
	forgejo *ForgejoClient
	auth    *Auth
	sched   *Scheduler
	dedupe  *deliveryCache
	botUser string
	tmpl    *template.Template
}

func NewServer(cfg *Config, store *Store, forgejo *ForgejoClient, auth *Auth, sched *Scheduler, botUser string) *Server {
	return &Server{
		cfg:     cfg,
		store:   store,
		forgejo: forgejo,
		auth:    auth,
		sched:   sched,
		dedupe:  newDeliveryCache(10*time.Minute, 4096),
		botUser: botUser,
		tmpl:    template.Must(template.New("").Funcs(templateFuncs).Parse(templates)),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /login", s.auth.handleLogin)
	mux.HandleFunc("GET /oauth/callback", s.auth.handleCallback)
	mux.HandleFunc("POST /logout", s.auth.handleLogout)
	mux.HandleFunc("POST /repos/enable", s.handleEnable)
	mux.HandleFunc("POST /repos/disable", s.handleDisable)
	mux.HandleFunc("POST /repos/run", s.handleRun)
	mux.HandleFunc("GET /runs", s.handleRuns)
	mux.HandleFunc("GET /runs/{id}", s.handleRunDetail)
	mux.HandleFunc("GET /api/status", s.handleStatusAPI)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("POST /webhook/{token}", s.handleWebhook)
	return mux
}

var templateFuncs = template.FuncMap{
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
	"fmtDuration": func(d time.Duration) string {
		return d.Round(time.Second).String()
	},
}

type repoRow struct {
	FullName      string
	DefaultBranch string
	Missing       bool
	Status        RepoStatus
}

type pageData struct {
	Login      string
	CSRF       string
	Error      string
	RepoFilter string
	Enabled    []repoRow
	Other      []repoRow
	Runs       []RunRecord
	Run        *RunRecord
	Log        string
}

func (s *Server) session(r *http.Request) *Session {
	return s.auth.session(r)
}

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	sess := s.session(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return nil, false
	}
	return sess, true
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		logger.Error().Err(err).Str("template", name).Msg("render template")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Error</title></head><body><h1>%d</h1><p>%s</p><p><a href="/">Back</a></p></body></html>`,
		status, template.HTMLEscapeString(message))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == nil {
		s.render(w, "login", pageData{})
		return
	}

	data := pageData{Login: sess.Login, CSRF: sess.CSRF}

	tracked := s.store.List()
	enabled := make(map[string]*TrackedRepo, len(tracked))
	for _, repo := range tracked {
		enabled[repo.FullName] = repo
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	seen := make(map[string]bool, len(tracked))
	token, err := sess.accessToken(ctx, s.auth)
	if err == nil {
		var visible []ForgejoRepo
		visible, err = s.forgejo.ListRepos(ctx, token)
		if err == nil {
			for _, repo := range visible {
				seen[repo.FullName] = true
				_, isEnabled := enabled[repo.FullName]
				row := repoRow{
					FullName:      repo.FullName,
					DefaultBranch: repo.DefaultBranch,
					Status:        s.sched.Status(repo.FullName),
				}
				if isEnabled {
					data.Enabled = append(data.Enabled, row)
				} else if repo.Permissions.Admin {
					data.Other = append(data.Other, row)
				}
			}
		}
	}
	// Tracked repositories stay visible even when the Forgejo API call failed
	// or the account can no longer see them.
	for _, repo := range tracked {
		if !seen[repo.FullName] {
			data.Enabled = append(data.Enabled, repoRow{
				FullName:      repo.FullName,
				DefaultBranch: repo.DefaultBranch,
				Missing:       true,
				Status:        s.sched.Status(repo.FullName),
			})
		}
	}
	sort.Slice(data.Enabled, func(i, j int) bool { return data.Enabled[i].FullName < data.Enabled[j].FullName })

	if err != nil {
		data.Error = "Could not list repositories: " + err.Error()
		if strings.Contains(err.Error(), "401") {
			data.Error = "Your Forgejo session expired. Log out and sign in again."
		}
	}

	s.render(w, "index", data)
}

func (s *Server) formSession(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return nil, false
	}
	sess := s.session(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return nil, false
	}
	if !checkCSRF(r, sess) {
		writeError(w, http.StatusForbidden, "invalid CSRF token")
		return nil, false
	}
	return sess, true
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.formSession(w, r)
	if !ok {
		return
	}
	fullName := strings.TrimSpace(r.PostFormValue("repo"))
	if fullName == "" {
		writeError(w, http.StatusBadRequest, "missing repository")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	token, err := sess.accessToken(ctx, s.auth)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	repos, err := s.forgejo.ListRepos(ctx, token)
	if err != nil {
		writeError(w, http.StatusBadGateway, "list repositories: "+err.Error())
		return
	}
	var target *ForgejoRepo
	for i := range repos {
		if repos[i].FullName == fullName {
			target = &repos[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "repository not found or not visible to your account")
		return
	}
	if !target.Permissions.Admin {
		writeError(w, http.StatusForbidden, "you need repository admin permission to install the webhook")
		return
	}

	hookToken := randomToken(24)
	if err := s.removeManagedHooks(ctx, token, fullName); err != nil {
		logger.Warn().Err(err).Str("repo", fullName).Msg("clean up old webhooks")
	}

	hook, err := s.forgejo.CreateHook(ctx, token, fullName, createHookOption{
		Type:   "forgejo",
		Active: true,
		Events: []string{"push", "pull_request", "pull_request_sync", "issue_comment", "issues"},
		Config: map[string]string{
			"url":          s.cfg.webhookURL(hookToken),
			"content_type": "json",
			"http_method":  "post",
			"secret":       hookToken,
		},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "create webhook: "+err.Error())
		return
	}

	if err := s.store.Put(&TrackedRepo{
		FullName:      fullName,
		DefaultBranch: target.DefaultBranch,
		HookID:        hook.ID,
		HookToken:     hookToken,
		EnabledBy:     sess.Login,
		EnabledAt:     time.Now(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "save repository: "+err.Error())
		return
	}

	logger.Info().
		Str("repo", fullName).
		Str("by", sess.Login).
		Int64("hook", hook.ID).
		Msg("repository enabled")
	// Run once right away so onboarding PRs appear without waiting for an event.
	s.sched.Enqueue(fullName, "enable", true)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) removeManagedHooks(ctx context.Context, token, fullName string) error {
	hooks, err := s.forgejo.ListHooks(ctx, token, fullName)
	if err != nil {
		return err
	}
	prefix := s.cfg.BaseURL + "/webhook/"
	for _, hook := range hooks {
		if !strings.HasPrefix(hook.URL, prefix) {
			continue
		}
		if err := s.forgejo.DeleteHook(ctx, token, fullName, hook.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleDisable(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.formSession(w, r)
	if !ok {
		return
	}
	fullName := strings.TrimSpace(r.PostFormValue("repo"))
	repo, tracked := s.store.Get(fullName)
	if !tracked {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if token, err := sess.accessToken(ctx, s.auth); err == nil {
		if err := s.forgejo.DeleteHook(ctx, token, fullName, repo.HookID); err != nil {
			logger.Warn().Err(err).Str("repo", fullName).Int64("hook", repo.HookID).Msg("delete webhook")
		}
	} else {
		logger.Warn().Err(err).Str("repo", fullName).Msg("cannot delete webhook, no valid token")
	}

	if err := s.store.Delete(fullName); err != nil {
		writeError(w, http.StatusInternalServerError, "delete repository: "+err.Error())
		return
	}
	logger.Info().Str("repo", fullName).Str("by", sess.Login).Msg("repository disabled")
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.formSession(w, r)
	if !ok {
		return
	}
	fullName := strings.TrimSpace(r.PostFormValue("repo"))
	if _, tracked := s.store.Get(fullName); !tracked {
		writeError(w, http.StatusNotFound, "repository is not enabled")
		return
	}
	s.sched.Enqueue(fullName, "manual", true)
	logger.Info().Str("repo", fullName).Str("by", sess.Login).Msg("manual run requested")
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	repoFilter := strings.TrimSpace(r.URL.Query().Get("repo"))

	var runs []RunRecord
	var err error
	if repoFilter == "" {
		runs, err = s.store.RecentRuns(200)
	} else {
		runs, err = s.store.RunsForRepo(repoFilter, 200)
	}

	data := pageData{Login: sess.Login, CSRF: sess.CSRF, Runs: runs, RepoFilter: repoFilter}
	if err != nil {
		data.Error = "Could not read run history: " + err.Error()
	}
	s.render(w, "runs", data)
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	record, found := s.store.FindRun(id)
	if !found {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	logText, err := s.store.ReadRunLog(id, 2000)
	if err != nil {
		logText = "could not read log: " + err.Error()
	}
	s.render(w, "run", pageData{Login: sess.Login, CSRF: sess.CSRF, Run: &record, Log: logText})
}

type repoStatusJSON struct {
	State  WorkerState `json:"state"`
	Queued int         `json:"queued"`
}

func (s *Server) handleStatusAPI(w http.ResponseWriter, r *http.Request) {
	if s.session(r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	out := make(map[string]repoStatusJSON)
	for _, repo := range s.store.List() {
		status := s.sched.Status(repo.FullName)
		out[repo.FullName] = repoStatusJSON{
			State:  status.State,
			Queued: status.Queued,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	hookToken := r.PathValue("token")
	repo, ok := s.store.GetByToken(hookToken)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Forgejo-Signature")
	if signature == "" {
		signature = r.Header.Get("X-Gitea-Signature")
	}
	if !verifySignature(hookToken, body, signature) {
		logger.Warn().Str("repo", repo.FullName).Msg("webhook signature mismatch")
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	delivery := r.Header.Get("X-Forgejo-Delivery")
	if delivery == "" {
		delivery = r.Header.Get("X-Gitea-Delivery")
	}
	if s.dedupe.Seen(delivery) {
		logger.Info().Str("repo", repo.FullName).Str("delivery", delivery).Msg("duplicate webhook delivery ignored")
		_, _ = fmt.Fprint(w, `{"ok":true,"duplicate":true}`)
		return
	}

	var payload hookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	if payload.Repository.FullName != "" && payload.Repository.FullName != repo.FullName {
		logger.Warn().
			Str("expected", repo.FullName).
			Str("payload", payload.Repository.FullName).
			Msg("webhook repository mismatch")
		http.Error(w, "repository mismatch", http.StatusBadRequest)
		return
	}

	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		event = r.Header.Get("X-Gitea-Event")
	}

	decision := decideTrigger(event, &payload, s.botUser)
	if !decision.Trigger {
		logger.Debug().Str("repo", repo.FullName).Str("event", event).Str("reason", decision.Reason).Msg("webhook ignored")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ignored":%q}`, decision.Reason)
		return
	}

	s.sched.Enqueue(repo.FullName, event, false)
	logger.Info().Str("repo", repo.FullName).Str("event", event).Str("reason", decision.Reason).Msg("webhook accepted")
	_, _ = fmt.Fprint(w, `{"ok":true}`)
}
