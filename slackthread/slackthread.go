// Package slackthread reports the result of a /two access request as a reply
// in the Slack thread the request came from, posted as slack-bot. Every access
// backend uses it, so the payload fields, the wording and the log events are
// the same everywhere.
package slackthread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Thread is where the request came from. Embed it in a backend's request
// struct to accept the shared payload fields.
type Thread struct {
	ChannelID string `json:"slack_channel_id,omitempty"`
	ThreadTS  string `json:"slack_thread_ts,omitempty"`
	UserID    string `json:"slack_user_id,omitempty"`
}

// Valid reports whether there is a thread to reply in.
func (t Thread) Valid() bool {
	return t.ChannelID != "" && t.ThreadTS != ""
}

// Outcome is what happened to the request.
type Outcome int

const (
	Granted Outcome = iota
	GrantedPermanently
	Revoked
	Expired
	NothingToRemove
	Kept
	Failed
	Partial
	ExpiryNotSet
)

var outcomeNames = [...]string{"granted", "granted_permanently", "revoked", "expired", "nothing_to_remove", "kept", "failed", "partial", "expiry_not_set"}

func (o Outcome) String() string {
	if o < 0 || int(o) >= len(outcomeNames) {
		return "unknown"
	}
	return outcomeNames[o]
}

// Reply is one message. What is built by the backend, e.g. "`roles/x` on `proj`".
type Reply struct {
	Outcome Outcome
	Op      string     // GRANT | REVOKE | DOWNGRADE, used by Failed and Partial
	What    string     // for Partial, the parts that succeeded
	Who     string     // principal, used by Kept only
	Until   *time.Time // Granted only
	Err     error      // Failed and Partial; for Partial, the parts that failed and why
}

// FormatTime renders a time the way every reply does: "2026-10-08 14:02 UTC".
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

func opTitle(op string) string {
	if op == "" {
		return "Request"
	}
	return strings.ToUpper(op[:1]) + strings.ToLower(op[1:])
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// Text is the message for a reply: emoji, mention when known, one sentence.
func Text(t Thread, r Reply) string {
	var emoji, sentence string
	switch r.Outcome {
	case Granted:
		emoji = ":white_check_mark:"
		if r.Until != nil {
			sentence = fmt.Sprintf("Access granted: %s until %s.", r.What, FormatTime(*r.Until))
		} else {
			sentence = fmt.Sprintf("Access granted: %s.", r.What)
		}
	case GrantedPermanently:
		emoji, sentence = ":white_check_mark:", fmt.Sprintf("Access granted: %s permanently.", r.What)
	case Revoked:
		emoji, sentence = ":no_entry:", fmt.Sprintf("Access revoked: %s.", r.What)
	case Expired:
		emoji, sentence = ":no_entry:", fmt.Sprintf("Access expired and was removed: %s.", r.What)
	case NothingToRemove:
		emoji, sentence = ":information_source:", fmt.Sprintf("Nothing to remove: %s was already gone.", r.What)
	case Kept:
		// What is "membership of `group`", so this reads "keeps their permanent membership of `group`".
		emoji, sentence = ":information_source:", fmt.Sprintf("Nothing removed: %s keeps their permanent %s.", r.Who, r.What)
	case Failed:
		emoji, sentence = ":x:", fmt.Sprintf("%s failed for %s: %s.", opTitle(r.Op), r.What, errText(r.Err))
	case Partial:
		emoji, sentence = ":warning:", fmt.Sprintf("%s partly failed: done for %s; failed: %s.", opTitle(r.Op), r.What, errText(r.Err))
	case ExpiryNotSet:
		emoji, sentence = ":warning:", fmt.Sprintf("Access granted: %s, but its automatic removal could not be scheduled. Revoke it when you are done.", r.What)
	default:
		emoji, sentence = ":information_source:", fmt.Sprintf("%s finished for %s.", opTitle(r.Op), r.What)
	}
	if t.UserID != "" {
		return fmt.Sprintf("%s <@%s> %s", emoji, t.UserID, sentence)
	}
	return emoji + " " + sentence
}

const defaultMaxAttempts = 5

// FinalAttempt reports whether no further Cloud Tasks retry will follow this
// request, so a failure should be reported now. A request without the retry
// header did not come from Cloud Tasks and will not be retried.
//
// ponytail: compares X-CloudTasks-TaskRetryCount with CLOUD_TASKS_MAX_ATTEMPTS
// and ignores the queue's max_retry_duration, so a queue that gives up on time
// first never reports. Read the queue config instead if that starts to matter.
func FinalAttempt(r *http.Request) bool {
	header := r.Header.Get("X-CloudTasks-TaskRetryCount")
	if header == "" {
		return true
	}
	retries, err := strconv.Atoi(header)
	if err != nil {
		return true
	}
	maxAttempts := defaultMaxAttempts
	if v, err := strconv.Atoi(os.Getenv("CLOUD_TASKS_MAX_ATTEMPTS")); err == nil && v > 0 {
		maxAttempts = v
	}
	return retries >= maxAttempts-1
}

// Overridable in tests.
var (
	PostMessageURL = "https://slack.com/api/chat.postMessage"
	HTTPClient     = &http.Client{Timeout: 10 * time.Second}
)

const defaultTokenFile = "/etc/secrets/slack-bot/SLACK_BOT_TOKEN"

// Post replies in the thread. It is best-effort: it never returns an error,
// and a request with no thread is only logged. The token is read on every call
// from SLACK_BOT_TOKEN_FILE, so a rotation needs no redeploy.
func Post(ctx context.Context, log *slog.Logger, prefix string, t Thread, r Reply) {
	if log == nil {
		log = slog.Default()
	}
	attrs := []any{
		"slack_channel_id", t.ChannelID,
		"slack_thread_ts", t.ThreadTS,
		"slack_user_id", t.UserID,
		"outcome", r.Outcome.String(),
		"op", r.Op,
	}
	logWith := func(level slog.Level, event, message string, extra ...any) {
		log.Log(ctx, level, prefix+".slack."+event, append(append([]any{"message", message}, extra...), attrs...)...)
	}

	if !t.Valid() {
		logWith(slog.LevelInfo, "no_thread", "Request carries no Slack thread, result not reported to Slack")
		return
	}

	tokenFile := os.Getenv("SLACK_BOT_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = defaultTokenFile
	}
	raw, err := os.ReadFile(tokenFile)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		reason := "empty token file"
		if err != nil {
			reason = err.Error()
		}
		logWith(slog.LevelError, "token_unreadable", "Cannot read Slack bot token: "+reason)
		return
	}

	body, err := json.Marshal(map[string]string{
		"channel":   t.ChannelID,
		"thread_ts": t.ThreadTS,
		"text":      Text(t, r),
	})
	if err != nil {
		logWith(slog.LevelError, "post_error", err.Error())
		return
	}

	// Keep the trace, not the caller's cancellation: the operation already happened.
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, PostMessageURL, bytes.NewReader(body))
	if err != nil {
		logWith(slog.LevelError, "post_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		logWith(slog.LevelError, "post_error", err.Error())
		return
	}
	defer resp.Body.Close()

	// chat.postMessage answers 200 even when it refuses; only `ok` says it posted.
	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slackResp); err != nil || !slackResp.OK {
		logWith(slog.LevelError, "post_rejected", "Slack did not post the reply",
			"status_code", resp.StatusCode, "slack_error", slackResp.Error)
		return
	}

	logWith(slog.LevelInfo, "posted", "Result replied in the request thread")
}
