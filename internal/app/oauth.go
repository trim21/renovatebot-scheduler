package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	tokens *tokenStore
}

// errSessionExpired means the session's OAuth token can no longer be refreshed
// and the user has to sign in again. Callers should drop the session cookie and
// send the browser to /login.
var errSessionExpired = errors.New("session expired, please sign in again")

func (s *Session) accessToken(ctx context.Context, a *Auth) (string, error) {
	token, err := s.tokens.AccessToken(ctx)
	if errors.Is(err, errSessionExpired) {
		a.sessions.Delete(s.ID)
	}
	return token, err
}

// tokenStore holds one session's OAuth token and refreshes it on demand.
//
// Refreshes are serialized because Forgejo rotates the refresh token on every
// use: two concurrent refreshes would make the second one fail and permanently
// break the session.
type tokenStore struct {
	auth *Auth

	mu       sync.Mutex
	token    *oauthToken
	rejected bool
}

func newTokenStore(auth *Auth, token *oauthToken) *tokenStore {
	return &tokenStore{auth: auth, token: token}
}

// AccessToken returns a usable access token, refreshing it when it is about to
// expire. It returns errSessionExpired when the token cannot be refreshed.
func (s *tokenStore) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rejected {
		return "", errSessionExpired
	}
	if s.token.valid() {
		return s.token.AccessToken, nil
	}
	if s.token == nil || s.token.RefreshToken == "" {
		s.rejected = true
		return "", errSessionExpired
	}

	token, err := s.auth.refreshToken(ctx, s.token.RefreshToken)
	if err != nil {
		// A rejected refresh token cannot be retried: Forgejo already moved the
		// grant to the next counter.
		s.rejected = true
		s.token = nil
		return "", fmt.Errorf("%w: %v", errSessionExpired, err)
	}
	s.token = token
	return token.AccessToken, nil
}

type SessionStore struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*Session)}
}

func (s *SessionStore) Create(login string, tokens *tokenStore) *Session {
	sess := &Session{
		ID:      randomToken(24),
		Login:   login,
		CSRF:    randomToken(24),
		Expires: time.Now().Add(24 * time.Hour),
		tokens:  tokens,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	// Forgejo keeps one OAuth grant per user and application and rotates its
	// refresh token on every use. Two live sessions of the same user would
	// invalidate each other's refresh token, so a new login replaces them.
	for id, existing := range s.m {
		if strings.EqualFold(existing.Login, login) {
			delete(s.m, id)
		}
	}
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.Error != "" {
			detail := oauthErr.Description
			if detail == "" {
				detail = strings.TrimSpace(string(body))
			}
			return nil, fmt.Errorf("token request failed: %s (%s: %s)", resp.Status, oauthErr.Error, detail)
		}
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

	sess := a.sessions.Create(login, newTokenStore(a, token))
	a.setSessionCookie(w, sess)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "sid", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
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
	a.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func checkCSRF(r *http.Request, sess *Session) bool {
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) == 1
}
