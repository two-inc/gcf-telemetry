package slackthread

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
	pubsub "google.golang.org/api/pubsub/v1"
)

func TestNewEvent(t *testing.T) {
	until := time.Date(2026, 10, 8, 16, 2, 0, 0, time.FixedZone("CEST", 2*3600))
	e := NewEvent(
		Source{Function: "gcp-access-manager", RequestID: "req-1", TaskName: "projects/p/locations/l/queues/q/tasks/t"},
		thread,
		Reply{Outcome: Failed, Op: "GRANT", What: "`roles/x` on `p`", Who: "a@two.inc", Until: &until, Err: errors.New("boom")},
	)
	want := Event{
		RequestID: "req-1", Operation: "GRANT", Outcome: "failed", What: "`roles/x` on `p`", Who: "a@two.inc",
		Until: "2026-10-08T14:02:00Z", Error: "boom",
		SlackChannelID: "C1", SlackThreadTS: "1.2", SlackUserID: "U1",
		Function: "gcp-access-manager", TaskName: "projects/p/locations/l/queues/q/tasks/t",
	}
	if e != want {
		t.Errorf("got %+v\nwant %+v", e, want)
	}

	raw, _ := json.Marshal(NewEvent(Source{Function: "f"}, Thread{}, Reply{Outcome: Revoked}))
	if string(raw) != `{"outcome":"revoked","function":"f"}` {
		t.Errorf("empty fields not omitted: %s", raw)
	}
}

func TestTaskName(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if TaskName(r) != "" {
		t.Error("want empty without header")
	}
	r.Header.Set("X-CloudTasks-TaskName", "t1")
	if TaskName(r) != "t1" {
		t.Error("want header value")
	}
}

type published struct {
	path string
	req  pubsub.PublishRequest
}

// fakePubSub points Publish at a test server; status other than 200 makes it fail.
func fakePubSub(t *testing.T, status int) *[]published {
	t.Helper()
	var got []published
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pubsub.PublishRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, published{path: r.URL.Path, req: req})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"messageIds":["m1"]}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"denied"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	old := newPubSub
	newPubSub = func(ctx context.Context) (*pubsub.Service, error) {
		return pubsub.NewService(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	}
	t.Cleanup(func() { newPubSub = old })
	return &got
}

func publish(logs *bytes.Buffer, th Thread, r Reply) {
	Publish(context.Background(), slog.New(slog.NewJSONHandler(logs, nil)), "test_fn",
		Source{Function: "test-fn", RequestID: "req-1", TaskName: "t1"}, th, r)
}

func TestPublishSendsEvent(t *testing.T) {
	got := fakePubSub(t, http.StatusOK)
	logs := &bytes.Buffer{}
	publish(logs, thread, Reply{Outcome: Revoked, Op: "REVOKE", What: "`s` in `p`"})

	if len(*got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(*got))
	}
	p := (*got)[0]
	if !strings.HasSuffix(p.path, "/v1/"+DefaultTopic+":publish") {
		t.Errorf("path = %q", p.path)
	}
	m := p.req.Messages[0]
	if m.Attributes["function"] != "test-fn" || m.Attributes["outcome"] != "revoked" || m.Attributes["operation"] != "REVOKE" {
		t.Errorf("attributes = %v", m.Attributes)
	}
	raw, _ := base64.StdEncoding.DecodeString(m.Data)
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.RequestID != "req-1" || e.TaskName != "t1" || e.SlackThreadTS != "1.2" || e.What != "`s` in `p`" {
		t.Errorf("event = %+v", e)
	}
	if !strings.Contains(logs.String(), `"msg":"test_fn.result.published"`) {
		t.Errorf("missing published log: %s", logs)
	}
}

func TestPublishWithoutThreadStillPublishes(t *testing.T) {
	got := fakePubSub(t, http.StatusOK)
	publish(&bytes.Buffer{}, Thread{}, Reply{Outcome: Granted})
	if len(*got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(*got))
	}
}

func TestPublishHonoursTopicEnv(t *testing.T) {
	got := fakePubSub(t, http.StatusOK)
	t.Setenv("ACCESS_RESULTS_TOPIC", "projects/p/topics/other")
	publish(&bytes.Buffer{}, thread, Reply{Outcome: Granted})
	if !strings.HasSuffix((*got)[0].path, "/v1/projects/p/topics/other:publish") {
		t.Errorf("path = %q", (*got)[0].path)
	}
}

func TestPublishErrorIsLoggedAndSwallowed(t *testing.T) {
	fakePubSub(t, http.StatusForbidden)
	logs := &bytes.Buffer{}
	publish(logs, thread, Reply{Outcome: Failed, Op: "GRANT", Err: errors.New("x")})
	if !strings.Contains(logs.String(), `"level":"ERROR","msg":"test_fn.result.publish_error"`) {
		t.Errorf("missing publish_error log: %s", logs)
	}
}

func TestPublishNilLoggerUsesDefault(t *testing.T) {
	got := fakePubSub(t, http.StatusOK)
	Publish(context.Background(), nil, "test_fn", Source{Function: "f"}, thread, Reply{Outcome: Granted})
	if len(*got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(*got))
	}
}

func TestPublishRequestsOnlyCloudPlatformScope(t *testing.T) {
	if len(pubSubScopes) != 1 || pubSubScopes[0] != "https://www.googleapis.com/auth/cloud-platform" {
		t.Errorf("pubSubScopes = %v, want only cloud-platform", pubSubScopes)
	}
}

func TestPublishAlertSendsAlert(t *testing.T) {
	got := fakePubSub(t, http.StatusOK)
	logs := &bytes.Buffer{}
	PublishAlert(context.Background(), slog.New(slog.NewJSONHandler(logs, nil)), "test_fn",
		Source{Function: "ip-firewall-manager", TaskName: "t1"}, thread, "block",
		Alert{
			Title: "IP Block", Text: "IP address `1.2.3.4` blocked", Severity: "warning", TestMode: true,
			Fields: []AlertField{{Title: "IP Address", Value: "1.2.3.4"}, {Title: "Project", Value: "tillit-api"}},
			Button: &AlertButton{Text: "Unblock IP in tillit-api", ActionID: "unblock_ip_button", Value: "1.2.3.4;tillit-api"},
		})

	if len(*got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(*got))
	}
	m := (*got)[0].req.Messages[0]
	if m.Attributes["outcome"] != "block" || m.Attributes["function"] != "ip-firewall-manager" {
		t.Errorf("attributes = %v", m.Attributes)
	}
	raw, _ := base64.StdEncoding.DecodeString(m.Data)
	// The exact JSON slack-bot's firewall_alerts reads.
	var e map[string]any
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	alert, _ := e["alert"].(map[string]any)
	if alert == nil || alert["title"] != "IP Block" || alert["severity"] != "warning" || alert["test_mode"] != true {
		t.Fatalf("alert = %v", e["alert"])
	}
	if _, set := alert["warning"]; set {
		t.Errorf("warning=false should be omitted: %v", alert)
	}
	fields, _ := alert["fields"].([]any)
	if len(fields) != 2 || fields[0].(map[string]any)["title"] != "IP Address" || fields[1].(map[string]any)["value"] != "tillit-api" {
		t.Errorf("fields = %v", alert["fields"])
	}
	button, _ := alert["button"].(map[string]any)
	if button["action_id"] != "unblock_ip_button" || button["value"] != "1.2.3.4;tillit-api" || button["text"] != "Unblock IP in tillit-api" {
		t.Errorf("button = %v", alert["button"])
	}
	if e["slack_thread_ts"] != "1.2" || e["task_name"] != "t1" || e["outcome"] != "block" {
		t.Errorf("event = %v", e)
	}
}

func TestPublishLeavesAlertUnset(t *testing.T) {
	raw, _ := json.Marshal(NewEvent(Source{Function: "f"}, Thread{}, Reply{Outcome: Granted}))
	if strings.Contains(string(raw), "alert") {
		t.Errorf("non-firewall event carries an alert: %s", raw)
	}
}
