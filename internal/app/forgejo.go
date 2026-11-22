package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ForgejoClient talks to the Forgejo REST API on behalf of an OAuth2 token or a PAT.
type ForgejoClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewForgejoClient(baseURL string) *ForgejoClient {
	return &ForgejoClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type ForgejoUser struct {
	Login string `json:"login"`
}

// ForgejoRepo is the subset of the Forgejo repository schema this service reads.
type ForgejoRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Permissions   struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// ForgejoHook is the subset of the Forgejo webhook schema this service reads.
type ForgejoHook struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

type createHookOption struct {
	Type   string            `json:"type"`
	Active bool              `json:"active"`
	Events []string          `json:"events"`
	Config map[string]string `json:"config"`
}

// TokenSource returns a currently valid access token for the given session.
type TokenSource func(ctx context.Context) (string, error)

func (c *ForgejoClient) CurrentUser(ctx context.Context, token string) (string, error) {
	var user ForgejoUser
	if err := c.do(ctx, http.MethodGet, "/api/v1/user", token, nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("forgejo returned an empty login")
	}
	return user.Login, nil
}

// ListRepos returns every repository the token owner can see, newest first.
func (c *ForgejoClient) ListRepos(ctx context.Context, token string) ([]ForgejoRepo, error) {
	const perPage = 50
	repos := make([]ForgejoRepo, 0, perPage)
	for page := 1; page <= 20; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(perPage))
		q.Set("sort", "updated")
		q.Set("order", "desc")

		var batch []ForgejoRepo
		if err := c.do(ctx, http.MethodGet, "/api/v1/user/repos?"+q.Encode(), token, nil, &batch); err != nil {
			return nil, err
		}
		repos = append(repos, batch...)
		if len(batch) < perPage {
			break
		}
	}
	return repos, nil
}

func (c *ForgejoClient) ListHooks(ctx context.Context, token, fullName string) ([]ForgejoHook, error) {
	var hooks []ForgejoHook
	err := c.do(ctx, http.MethodGet, repoPath(fullName)+"/hooks", token, nil, &hooks)
	return hooks, err
}

func (c *ForgejoClient) CreateHook(ctx context.Context, token, fullName string, opt createHookOption) (*ForgejoHook, error) {
	var hook ForgejoHook
	if err := c.do(ctx, http.MethodPost, repoPath(fullName)+"/hooks", token, opt, &hook); err != nil {
		return nil, err
	}
	return &hook, nil
}

func (c *ForgejoClient) DeleteHook(ctx context.Context, token, fullName string, id int64) error {
	err := c.do(ctx, http.MethodDelete, repoPath(fullName)+"/hooks/"+strconv.FormatInt(id, 10), token, nil, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

func repoPath(fullName string) string {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return "/api/v1/repos/" + url.PathEscape(fullName)
	}
	return "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (c *ForgejoClient) do(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}
