package slackthread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestText(t *testing.T) {
	until := time.Date(2026, 10, 8, 14, 2, 0, 0, time.UTC)
	cases := []struct {
		name  string
		reply Reply
		want  string
	}{
		{"granted", Reply{Outcome: Granted, What: "`roles/viewer` on `proj`", Until: &until},
			":white_check_mark: <@U1> Access granted: `roles/viewer` on `proj` until 2026-10-08 14:02 UTC."},
		{"granted without expiry", Reply{Outcome: Granted, What: "`s` in `proj`"},
			":white_check_mark: <@U1> Access granted: `s` in `proj`."},
		{"granted permanently", Reply{Outcome: GrantedPermanently, What: "membership of `g`"},
			":white_check_mark: <@U1> Access granted: membership of `g` permanently."},
		{"revoked", Reply{Outcome: Revoked, What: "`roles/viewer` on `proj`"},
			":no_entry: <@U1> Access revoked: `roles/viewer` on `proj`."},
		{"expired", Reply{Outcome: Expired, What: "READONLY on `a, b` in `i`"},
			":no_entry: <@U1> Access expired and was removed: READONLY on `a, b` in `i`."},
		{"nothing to remove", Reply{Outcome: NothingToRemove, What: "membership of `g`"},
			":information_source: <@U1> Nothing to remove: membership of `g` was already gone."},
		{"kept", Reply{Outcome: Kept, Who: "a@two.inc", What: "membership of `g`"},
			":information_source: <@U1> Nothing removed: a@two.inc keeps their permanent membership of `g`."},
		{"failed", Reply{Outcome: Failed, Op: "REVOKE", What: "`s` in `proj`", Err: errors.New("boom")},
			":x: <@U1> Revoke failed for `s` in `proj`: boom."},
		{"failed without error", Reply{Outcome: Failed, Op: "GRANT", What: "`s` in `proj`"},
			":x: <@U1> Grant failed for `s` in `proj`: unknown error."},
		{"partial", Reply{Outcome: Partial, Op: "GRANT", What: "READONLY on `a` in `i`", Err: errors.New("`b`: permission denied")},
			":warning: <@U1> Grant partly failed: done for READONLY on `a` in `i`; failed: `b`: permission denied."},
		{"expiry not set", Reply{Outcome: ExpiryNotSet, What: "ADMIN on `a` in `i`"},
			":warning: <@U1> Access granted: ADMIN on `a` in `i`, but its automatic removal could not be scheduled. Revoke it when you are done."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Text(Thread{UserID: "U1"}, c.reply); got != c.want {
				t.Errorf("with mention:\n got %q\nwant %q", got, c.want)
			}
			wantNoMention := strings.Replace(c.want, " <@U1>", "", 1)
			if got := Text(Thread{}, c.reply); got != wantNoMention {
				t.Errorf("without mention:\n got %q\nwant %q", got, wantNoMention)
			}
		})
	}
}

func TestOutcomeString(t *testing.T) {
	if Partial.String() != "partial" || ExpiryNotSet.String() != "expiry_not_set" || Outcome(99).String() != "unknown" {
		t.Errorf("unexpected names: %s %s %s", Partial, ExpiryNotSet, Outcome(99))
	}
}

func TestFinalAttempt(t *testing.T) {
	cases := []struct {
		name, header, env string
		want              bool
	}{
		{"no header is not a task", "", "", true},
		{"unparseable header", "x", "", true},
		{"first attempt, default max", "0", "", false},
		{"last attempt, default max", "4", "", true},
		{"past last attempt", "7", "", true},
		{"env max respected", "2", "3", true},
		{"env max not reached", "1", "3", false},
		{"bad env falls back to default", "3", "nope", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLOUD_TASKS_MAX_ATTEMPTS", c.env)
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if c.header != "" {
				r.Header.Set("X-CloudTasks-TaskRetryCount", c.header)
			}
			if got := FinalAttempt(r); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

const testToken = "xoxb-secret-test-token"

type posted struct {
	auth string
	body map[string]string
}

// fakeSlack points Post at a test server and a token file; it returns what was posted and the log buffer.
func fakeSlack(t *testing.T, respond func(w http.ResponseWriter)) (*[]posted, *bytes.Buffer) {
	t.Helper()
	var got []posted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, posted{auth: r.Header.Get("Authorization"), body: body})
		respond(w)
	}))
	t.Cleanup(srv.Close)
	oldURL := PostMessageURL
	PostMessageURL = srv.URL
	t.Cleanup(func() { PostMessageURL = oldURL })

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_BOT_TOKEN_FILE", tokenFile)
	return &got, &bytes.Buffer{}
}

func okSlack(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":true}`)) }
func refuseSlack(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
}

var thread = Thread{ChannelID: "C1", ThreadTS: "1.2", UserID: "U1"}

func post(logs *bytes.Buffer, th Thread, r Reply) {
	Post(context.Background(), slog.New(slog.NewJSONHandler(logs, nil)), "test_fn", th, r)
}

func assertNoToken(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	if strings.Contains(logs.String(), testToken) {
		t.Fatal("token leaked into logs")
	}
}

func TestPostRepliesInThread(t *testing.T) {
	got, logs := fakeSlack(t, okSlack)
	post(logs, thread, Reply{Outcome: Revoked, Op: "REVOKE", What: "`s` in `p`"})

	if len(*got) != 1 {
		t.Fatalf("posts = %d, want 1", len(*got))
	}
	p := (*got)[0]
	if p.auth != "Bearer "+testToken {
		t.Errorf("auth header = %q", p.auth)
	}
	if p.body["channel"] != "C1" || p.body["thread_ts"] != "1.2" {
		t.Errorf("posted to %q/%q", p.body["channel"], p.body["thread_ts"])
	}
	if p.body["text"] != ":no_entry: <@U1> Access revoked: `s` in `p`." {
		t.Errorf("text = %q", p.body["text"])
	}
	if !strings.Contains(logs.String(), `"msg":"test_fn.slack.posted"`) {
		t.Errorf("missing posted log: %s", logs)
	}
	assertNoToken(t, logs)
}

func TestPostWithoutThreadDoesNotPost(t *testing.T) {
	got, logs := fakeSlack(t, okSlack)
	post(logs, Thread{UserID: "U1"}, Reply{Outcome: Granted})

	if len(*got) != 0 {
		t.Fatalf("posts = %d, want 0", len(*got))
	}
	if !strings.Contains(logs.String(), `"level":"INFO","msg":"test_fn.slack.no_thread"`) {
		t.Errorf("want INFO no_thread log: %s", logs)
	}
}

func TestPostMissingTokenDoesNotPost(t *testing.T) {
	got, logs := fakeSlack(t, okSlack)
	t.Setenv("SLACK_BOT_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	post(logs, thread, Reply{Outcome: Granted})

	if len(*got) != 0 {
		t.Fatalf("posts = %d, want 0", len(*got))
	}
	if !strings.Contains(logs.String(), "test_fn.slack.token_unreadable") {
		t.Errorf("missing token_unreadable log: %s", logs)
	}
}

func TestPostEmptyTokenDoesNotPost(t *testing.T) {
	got, logs := fakeSlack(t, okSlack)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_BOT_TOKEN_FILE", empty)
	post(logs, thread, Reply{Outcome: Granted})

	if len(*got) != 0 {
		t.Fatalf("posts = %d, want 0", len(*got))
	}
}

func TestPostSlackRefusalIsLoggedAndSwallowed(t *testing.T) {
	_, logs := fakeSlack(t, refuseSlack)
	post(logs, thread, Reply{Outcome: Failed, Op: "GRANT", Err: errors.New("x")})

	if !strings.Contains(logs.String(), "test_fn.slack.post_rejected") || !strings.Contains(logs.String(), "not_in_channel") {
		t.Errorf("missing post_rejected log: %s", logs)
	}
	assertNoToken(t, logs)
}

func TestPostHTTPErrorIsSwallowed(t *testing.T) {
	_, logs := fakeSlack(t, okSlack)
	PostMessageURL = "http://127.0.0.1:1/unreachable"
	post(logs, thread, Reply{Outcome: Granted})

	if !strings.Contains(logs.String(), "test_fn.slack.post_error") {
		t.Errorf("missing post_error log: %s", logs)
	}
	assertNoToken(t, logs)
}

func TestPostNilLoggerUsesDefault(t *testing.T) {
	got, _ := fakeSlack(t, okSlack)
	Post(context.Background(), nil, "test_fn", thread, Reply{Outcome: Granted})
	if len(*got) != 1 {
		t.Fatalf("posts = %d, want 1", len(*got))
	}
}
