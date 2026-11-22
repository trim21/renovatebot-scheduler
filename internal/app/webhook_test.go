package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestIsZeroSHA(t *testing.T) {
	if !isZeroSHA("0000000000000000000000000000000000000000") {
		t.Error("all-zero SHA should be detected")
	}
	for _, sha := range []string{"", "0000000000000000000000000000000000000001"} {
		if isZeroSHA(sha) {
			t.Errorf("isZeroSHA(%q) = true, want false", sha)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	const secret = "s3cret"
	body := []byte(`{"ref":"refs/heads/main"}`)

	mac := hmacSHA256Hex(secret, body)
	if !verifySignature(secret, body, mac) {
		t.Fatal("valid signature rejected")
	}
	if verifySignature(secret, body, mac[:len(mac)-1]) {
		t.Fatal("truncated signature accepted")
	}
	if verifySignature(secret, body, "") {
		t.Fatal("empty signature accepted")
	}
	if verifySignature("other", body, mac) {
		t.Fatal("signature with wrong secret accepted")
	}
}

func TestDecideTriggerPush(t *testing.T) {
	const bot = "renovate-bot"
	push := func(ref, defaultBranch, after, sender string) *hookPayload {
		p := &hookPayload{Ref: ref, After: after}
		p.Repository.DefaultBranch = defaultBranch
		p.Sender.Login = sender
		return p
	}

	cases := []struct {
		name    string
		payload *hookPayload
		want    bool
	}{
		{"default branch", push("refs/heads/main", "main", "abc123", "alice"), true},
		{"non-default branch", push("refs/heads/feature", "main", "abc123", "alice"), false},
		{"deleted branch", push("refs/heads/main", "main", "0000000000000000000000000000000000000000", "alice"), false},
		{"by bot", push("refs/heads/main", "main", "abc123", bot), false},
		{"missing default branch", push("refs/heads/main", "", "abc123", "alice"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTrigger("push", tc.payload, bot)
			if got.Trigger != tc.want {
				t.Fatalf("decideTrigger() = %v (%s), want %v", got.Trigger, got.Reason, tc.want)
			}
		})
	}
}

func TestDecideTriggerPullRequest(t *testing.T) {
	const bot = "renovate-bot"
	pr := func(action, state, author, sender string, changes *hookChanges) *hookPayload {
		p := &hookPayload{Action: action, Changes: changes, PullRequest: &hookPullRequest{State: state}}
		p.PullRequest.User.Login = author
		p.Sender.Login = sender
		return p
	}
	bodyEdit := &hookChanges{Body: &hookChange{From: "old body"}}

	cases := []struct {
		name    string
		event   string
		payload *hookPayload
		want    bool
	}{
		{"bot pr body edited", "pull_request", pr("edited", "open", bot, "alice", bodyEdit), true},
		{"bot pr synchronized", "pull_request_sync", pr("synchronized", "open", bot, "alice", nil), true},
		{"bot pr synchronized via pull_request event", "pull_request", pr("synchronized", "open", bot, "alice", nil), true},
		{"human pr synchronized", "pull_request_sync", pr("synchronized", "open", "alice", "alice", nil), false},
		{"bot pr title edited", "pull_request", pr("edited", "open", bot, "alice", nil), false},
		{"human pr body edited", "pull_request", pr("edited", "open", "alice", "alice", bodyEdit), false},
		{"bot pr opened", "pull_request", pr("opened", "open", bot, "alice", nil), false},
		{"closed pr body edited", "pull_request", pr("edited", "closed", bot, "alice", bodyEdit), false},
		{"bot edits its own pr body", "pull_request", pr("edited", "open", bot, bot, bodyEdit), false},
		{"bot pushes to its own pr branch", "pull_request_sync", pr("synchronized", "open", bot, bot, nil), false},
		{"malformed payload", "pull_request", &hookPayload{Action: "edited"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTrigger(tc.event, tc.payload, bot)
			if got.Trigger != tc.want {
				t.Fatalf("decideTrigger() = %v (%s), want %v", got.Trigger, got.Reason, tc.want)
			}
		})
	}
}

func TestDecideTriggerIssueComment(t *testing.T) {
	const bot = "renovate-bot"
	comment := func(body, sender string) *hookPayload {
		p := &hookPayload{Action: "created", Comment: &hookComment{Body: body}}
		p.Sender.Login = sender
		return p
	}

	if got := decideTrigger("issue_comment", comment("@renovate-bot rebase", "alice"), bot); !got.Trigger {
		t.Fatalf("bot mention should trigger: %s", got.Reason)
	}
	if got := decideTrigger("issue_comment", comment("thanks!", "alice"), bot); got.Trigger {
		t.Fatalf("plain comment should not trigger: %s", got.Reason)
	}
	if got := decideTrigger("issue_comment", comment("@renovate-bot rebase", bot), bot); got.Trigger {
		t.Fatalf("bot comment should not trigger: %s", got.Reason)
	}
}

func TestDecideTriggerIssues(t *testing.T) {
	const bot = "renovate-bot"
	issue := func(action, state, author, sender string, changes *hookChanges) *hookPayload {
		p := &hookPayload{Action: action, Changes: changes, Issue: &hookIssue{State: state}}
		p.Issue.User.Login = author
		p.Sender.Login = sender
		return p
	}
	bodyEdit := &hookChanges{Body: &hookChange{From: "old body"}}

	cases := []struct {
		name    string
		payload *hookPayload
		want    bool
	}{
		{"dashboard body edited by human", issue("edited", "open", bot, "alice", bodyEdit), true},
		{"bot updates its own dashboard", issue("edited", "open", bot, bot, bodyEdit), false},
		{"human issue body edited", issue("edited", "open", "alice", "alice", bodyEdit), false},
		{"dashboard title edited", issue("edited", "open", bot, "alice", nil), false},
		{"dashboard issue opened", issue("opened", "open", bot, "alice", nil), false},
		{"closed dashboard body edited", issue("edited", "closed", bot, "alice", bodyEdit), false},
		{"malformed payload", &hookPayload{Action: "edited", Changes: bodyEdit}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTrigger("issues", tc.payload, bot)
			if got.Trigger != tc.want {
				t.Fatalf("decideTrigger() = %v (%s), want %v", got.Trigger, got.Reason, tc.want)
			}
		})
	}
}

func TestDecideTriggerOtherEvents(t *testing.T) {
	for _, event := range []string{"ping", "wiki", "release"} {
		if got := decideTrigger(event, &hookPayload{}, "renovate-bot"); got.Trigger {
			t.Errorf("%s triggered a run: %s", event, got.Reason)
		}
	}
}

func TestDeliveryCache(t *testing.T) {
	cache := newDeliveryCache(time.Minute, 2)
	if cache.Seen("a") {
		t.Fatal("first delivery must not be a duplicate")
	}
	if !cache.Seen("a") {
		t.Fatal("second delivery must be a duplicate")
	}
	if cache.Seen("b") || cache.Seen("c") {
		t.Fatal("unexpected duplicate")
	}
	// "a" was evicted when the cache exceeded its limit.
	if cache.Seen("a") {
		t.Fatal("evicted delivery must not be a duplicate")
	}
}
