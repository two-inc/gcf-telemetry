package slackthread

import (
	"errors"
	"net/http"
	"net/http/httptest"
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

var thread = Thread{ChannelID: "C1", ThreadTS: "1.2", UserID: "U1"}
