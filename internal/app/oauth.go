package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	expiry       time.Time
}

func (t *oauthToken) valid() bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	if t.expiry.IsZero() {
		return true
	}
	return time.Now().Before(t.expiry.Add(-30 * time.Second))
}

type Session struct {
	ID      string
	Login   string
	CSRF    string
	Expires time.Time

	mu    sync.Mutex
	Token *oauthToken
}

func (s *Session) accessToken(ctx context.Context, a *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Token.valid() {
		return s.Token.AccessToken, nil
	}
	if s.Token == nil || s.Token.RefreshToken == "" {
		return "", errors.New("session expired, please log in again")
	}
	token, err := a.refreshToken(ctx, s.Token.RefreshToken)
	if err != nil {
		return "", err
	}
	s.Token = token
	return token.AccessToken, nil
}

type SessionStore struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*Session)}
}

func (s *SessionStore) Create(login string, token *oauthToken) *Session {
	sess := &Session{
		ID:      randomToken(24),
		Login:   login,
		CSRF:    randomToken(24),
		Expires: time.Now().Add(24 * time.Hour),
		Token:   token,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.m[sess.ID] = sess
	return sess
}

func (s *SessionStore) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil
	}
	if time.Now().After(sess.Expires) {
		delete(s.m, id)
		return nil
	}
	return sess
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func (s *SessionStore) gcLocked() {
	now := time.Now()
	for id, sess := range s.m {
		if now.After(sess.Expires) {
			delete(s.m, id)
		}
	}
}

type Auth struct {
	cfg      *Config
	forgejo  *ForgejoClient
	sessions *SessionStore
	http     *http.Client
}

func NewAuth(cfg *Config, forgejo *ForgejoClient) *Auth {
	return &Auth{
		cfg:      cfg,
		forgejo:  forgejo,
		sessions: NewSessionStore(),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Auth) authorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", a.cfg.OAuthClientID)
	q.Set("redirect_uri", a.cfg.callbackURL())
	q.Set("response_type", "code")
	q.Set("state", state)
	return a.cfg.ForgejoURL + "/login/oauth/authorize?" + q.Encode()
}

func (a *Auth) exchangeCode(ctx context.Context, code string) (*oauthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", a.cfg.OAuthClientID)
	form.Set("client_secret", a.cfg.OAuthClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", a.cfg.callbackURL())
	return a.tokenRequest(ctx, form)
}

func (a *Auth) refreshToken(ctx context.Context, refreshToken string) (*oauthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", a.cfg.OAuthClientID)
	form.Set("client_secret", a.cfg.OAuthClientSecret)
	form.Set("refresh_token", refreshToken)
	return a.tokenRequest(ctx, form)
}

func (a *Auth) tokenRequest(ctx context.Context, form url.Values) (*oauthToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.ForgejoURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed: %s", resp.Status)
	}
	var token oauthToken
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if token.AccessToken == "" {
		return nil, errors.New("forgejo returned an empty access token")
	}
	if token.ExpiresIn > 0 {
		token.expiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return &token, nil
}

func (a *Auth) session(r *http.Request) *Session {
	cookie, err := r.Cookie("sid")
	if err != nil {
		return nil
	}
	return a.sessions.Get(cookie.Value)
}

func (a *Auth) setSessionCookie(w http.ResponseWriter, sess *Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.Expires,
	})
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken(16)
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.Redirect(w, r, a.authorizeURL(state), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if errParam := query.Get("error"); errParam != "" {
		writeError(w, http.StatusBadRequest, "Forgejo rejected the authorization: "+errParam+" "+query.Get("error_description"))
		return
	}

	stateCookie, err := r.Cookie("oauth_state")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing OAuth state cookie, restart the login flow")
		return
	}
	state := query.Get("state")
	if subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
		writeError(w, http.StatusBadRequest, "OAuth state mismatch, restart the login flow")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "oauth_state", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.secureCookies(), SameSite: http.SameSiteLaxMode,
	})

	code := query.Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "missing authorization code")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	token, err := a.exchangeCode(ctx, code)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	login, err := a.forgejo.CurrentUser(ctx, token.AccessToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, "look up Forgejo user: "+err.Error())
		return
	}
	if !a.cfg.canLogin(login) {
		writeError(w, http.StatusForbidden, "user "+login+" is not allowed to use this service")
		return
	}

	sess := a.sessions.Create(login, token)
	a.setSessionCookie(w, sess)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := a.session(r)
	if sess != nil && !checkCSRF(r, sess) {
		writeError(w, http.StatusForbidden, "invalid CSRF token")
		return
	}
	if sess != nil {
		a.sessions.Delete(sess.ID)
	}
	http.SetCookie(w, &http.Cookie{
		Name: "sid", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func checkCSRF(r *http.Request, sess *Session) bool {
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) == 1
}
