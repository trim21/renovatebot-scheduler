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
	pr := func(action, state, sender string) *hookPayload {
		p := &hookPayload{Action: action, PullRequest: &hookPullRequest{State: state}}
		p.Sender.Login = sender
		return p
	}

	cases := []struct {
		name    string
		event   string
		payload *hookPayload
		want    bool
	}{
		{"opened", "pull_request", pr("opened", "open", "alice"), true},
		{"reopened", "pull_request", pr("reopened", "open", "alice"), true},
		{"synchronized", "pull_request_sync", pr("synchronized", "open", "alice"), true},
		{"closed", "pull_request", pr("closed", "closed", "alice"), false},
		{"edited", "pull_request", pr("edited", "open", "alice"), false},
		{"opened by bot", "pull_request", pr("opened", "open", bot), false},
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

func TestDecideTriggerDashboard(t *testing.T) {
	dashboard := &hookPayload{Action: "edited", Issue: &hookIssue{Title: "Dependency Dashboard"}}
	dashboard.Sender.Login = "alice"

	if got := decideTrigger("issues", dashboard, "renovate-bot"); !got.Trigger {
		t.Fatalf("dashboard edit should trigger: %s", got.Reason)
	}

	other := &hookPayload{Action: "edited", Issue: &hookIssue{Title: "Something else"}}
	other.Sender.Login = "alice"
	if got := decideTrigger("issues", other, "renovate-bot"); got.Trigger {
		t.Fatalf("unrelated issue should not trigger: %s", got.Reason)
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
