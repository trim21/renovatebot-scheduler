package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type hookUser struct {
	Login string `json:"login"`
}

type hookRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type hookPullRequest struct {
	State string `json:"state"`
}

type hookIssue struct {
	Title string `json:"title"`
}

type hookComment struct {
	Body string `json:"body"`
}

// hookPayload is the upstream (Forgejo) webhook schema. Only the fields this
// service actually reads are declared; everything else is ignored by the JSON
// decoder on purpose.
type hookPayload struct {
	Action string `json:"action"`
	Ref    string `json:"ref"`
	After  string `json:"after"`

	Repository  hookRepository   `json:"repository"`
	Sender      hookUser         `json:"sender"`
	PullRequest *hookPullRequest `json:"pull_request"`
	Issue       *hookIssue       `json:"issue"`
	Comment     *hookComment     `json:"comment"`
}

type triggerDecision struct {
	Trigger bool
	Reason  string
}

// decideTrigger is the whole "does this event deserve a Renovate run" policy.
// Events sent by the Renovate bot itself never trigger a run: Renovate opens
// pull requests, pushes renovate/* branches and edits its dashboard issue, so
// reacting to those would loop forever.
func decideTrigger(event string, p *hookPayload, botUser string) triggerDecision {
	if botUser != "" && strings.EqualFold(p.Sender.Login, botUser) {
		return triggerDecision{Reason: "sender is the renovate bot"}
	}

	switch event {
	case "push":
		if p.Ref == "" || p.After == "" {
			return triggerDecision{Reason: "malformed push payload"}
		}
		if isZeroSHA(p.After) {
			return triggerDecision{Reason: "branch deleted"}
		}
		if p.Repository.DefaultBranch == "" {
			return triggerDecision{Reason: "payload has no default branch"}
		}
		if p.Ref != "refs/heads/"+p.Repository.DefaultBranch {
			return triggerDecision{Reason: "push to non-default branch"}
		}
		return triggerDecision{Trigger: true, Reason: "push to default branch"}

	case "pull_request", "pull_request_sync":
		if p.PullRequest == nil {
			return triggerDecision{Reason: "malformed pull request payload"}
		}
		switch p.Action {
		case "opened", "reopened", "synchronized":
		default:
			return triggerDecision{Reason: "pull request action " + p.Action}
		}
		if p.PullRequest.State != "" && p.PullRequest.State != "open" {
			return triggerDecision{Reason: "pull request is " + p.PullRequest.State}
		}
		return triggerDecision{Trigger: true, Reason: "pull request " + p.Action}

	case "issue_comment":
		if p.Action != "created" {
			return triggerDecision{Reason: "issue comment action " + p.Action}
		}
		if p.Comment == nil {
			return triggerDecision{Reason: "malformed issue comment payload"}
		}
		if botUser != "" && mentionsCommand(p.Comment.Body, botUser) {
			return triggerDecision{Trigger: true, Reason: "comment mentions " + botUser}
		}
		return triggerDecision{Reason: "comment does not mention the bot"}

	case "issues":
		if p.Action != "edited" || p.Issue == nil {
			return triggerDecision{Reason: "irrelevant issue event"}
		}
		if strings.Contains(strings.ToLower(p.Issue.Title), "dependency dashboard") {
			return triggerDecision{Trigger: true, Reason: "dependency dashboard edited"}
		}
		return triggerDecision{Reason: "title is not the dependency dashboard"}

	case "ping":
		return triggerDecision{Reason: "ping"}

	default:
		return triggerDecision{Reason: "unhandled event " + event}
	}
}

// mentionsCommand reports whether a comment addresses the bot, e.g.
// "@renovatebot rebase". Any mention counts: Renovate itself decides whether
// the command is valid.
func mentionsCommand(body, botUser string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "@"+strings.ToLower(botUser))
}

func isZeroSHA(sha string) bool {
	for _, r := range sha {
		if r != '0' {
			return false
		}
	}
	return sha != ""
}

func verifySignature(secret string, body []byte, signature string) bool {
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.ToLower(signature)))
}

// deliveryCache deduplicates webhook deliveries that Forgejo retries.
type deliveryCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	limit int
	seen  map[string]time.Time
	order []string
}

func newDeliveryCache(ttl time.Duration, limit int) *deliveryCache {
	return &deliveryCache{
		ttl:   ttl,
		limit: limit,
		seen:  make(map[string]time.Time, limit),
	}
}

func (c *deliveryCache) Seen(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if at, ok := c.seen[id]; ok {
		if now.Sub(at) < c.ttl {
			return true
		}
		// Expired, but keep its slot in order so the id stays unique there.
		c.seen[id] = now
		return false
	}
	c.seen[id] = now
	c.order = append(c.order, id)

	for len(c.order) > c.limit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, oldest)
	}
	return false
}
