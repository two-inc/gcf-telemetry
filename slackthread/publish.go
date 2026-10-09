package slackthread

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/option"
	pubsub "google.golang.org/api/pubsub/v1"
)

// DefaultTopic receives every access result; slack-bot pulls it and does the
// Slack work, so a backend needs only pubsub.publisher on it and no token.
const DefaultTopic = "projects/two-slack-bot/topics/access-results"

// Source says which function reported the result and for which request.
type Source struct {
	Function  string // e.g. "gcp-access-manager"
	RequestID string
	TaskName  string // TaskName(r) for a Cloud Tasks delivery, else empty
}

// TaskName is the Cloud Tasks task that delivered r, or "" when r did not come
// from Cloud Tasks.
func TaskName(r *http.Request) string {
	return r.Header.Get("X-CloudTasks-TaskName")
}

// Event is the message published for one result. slack-bot renders it, so the
// fields carry facts rather than wording.
type Event struct {
	RequestID      string `json:"request_id,omitempty"`
	Operation      string `json:"operation,omitempty"`
	Outcome        string `json:"outcome"`
	What           string `json:"what,omitempty"`
	Who            string `json:"who,omitempty"`
	Until          string `json:"until,omitempty"` // RFC 3339, UTC
	Error          string `json:"error,omitempty"`
	SlackChannelID string `json:"slack_channel_id,omitempty"`
	SlackThreadTS  string `json:"slack_thread_ts,omitempty"`
	SlackUserID    string `json:"slack_user_id,omitempty"`
	Function       string `json:"function"`
	TaskName       string `json:"task_name,omitempty"`
	// Alert is set only by ip-firewall-manager: slack-bot renders and routes
	// it as the function's own alert (slack_bot/firewall_alerts.py).
	Alert *Alert `json:"alert,omitempty"`
}

// Alert is an ip-firewall-manager result as data. The JSON names are the ones
// slack-bot's firewall_alerts reads; keep the two in step.
type Alert struct {
	Title    string       `json:"title,omitempty"`
	Text     string       `json:"text,omitempty"`
	Fields   []AlertField `json:"fields,omitempty"`
	Severity string       `json:"severity,omitempty"` // "info", "warning" or "error"
	Warning  bool         `json:"warning,omitempty"`  // routes a top-level post to the warnings channel
	TestMode bool         `json:"test_mode,omitempty"`
	Button   *AlertButton `json:"button,omitempty"`
}

// AlertField is one title/value pair in the alert's field grid.
type AlertField struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

// AlertButton is the alert's single action button.
type AlertButton struct {
	Text     string `json:"text"`
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
}

// NewEvent builds the event for a reply.
func NewEvent(src Source, t Thread, r Reply) Event {
	e := Event{
		RequestID:      src.RequestID,
		Operation:      r.Op,
		Outcome:        r.Outcome.String(),
		What:           r.What,
		Who:            r.Who,
		SlackChannelID: t.ChannelID,
		SlackThreadTS:  t.ThreadTS,
		SlackUserID:    t.UserID,
		Function:       src.Function,
		TaskName:       src.TaskName,
	}
	if r.Until != nil {
		e.Until = r.Until.UTC().Format(time.RFC3339)
	}
	if r.Err != nil {
		e.Error = r.Err.Error()
	}
	return e
}

// cloud-platform only: Cloud Run's metadata server refuses the Pub/Sub API's
// default scopes, and the refusal breaks every later token fetch on the instance
// (see cloudPlatformScope in the telemetry package).
var pubSubScopes = []string{pubsub.CloudPlatformScope}

// Overridable in tests.
var newPubSub = func(ctx context.Context) (*pubsub.Service, error) {
	return pubsub.NewService(ctx, option.WithScopes(pubSubScopes...))
}

// Publish reports a result to ACCESS_RESULTS_TOPIC (default DefaultTopic). It is
// best-effort: it never returns an error, only logs. It publishes even without
// a thread, because slack-bot also records the status and ip-firewall-manager's
// results go to a fallback channel.
func Publish(ctx context.Context, log *slog.Logger, prefix string, src Source, t Thread, r Reply) {
	publishEvent(ctx, log, prefix, NewEvent(src, t, r))
}

// PublishAlert reports an ip-firewall-manager alert. outcome names the kind of
// alert ("block", "unblock", "block_failed", ...): slack-bot dedupes on task
// name plus outcome, so two alerts from one task need different outcomes.
func PublishAlert(ctx context.Context, log *slog.Logger, prefix string, src Source, t Thread, outcome string, a Alert) {
	publishEvent(ctx, log, prefix, Event{
		RequestID:      src.RequestID,
		Outcome:        outcome,
		SlackChannelID: t.ChannelID,
		SlackThreadTS:  t.ThreadTS,
		SlackUserID:    t.UserID,
		Function:       src.Function,
		TaskName:       src.TaskName,
		Alert:          &a,
	})
}

func publishEvent(ctx context.Context, log *slog.Logger, prefix string, e Event) {
	if log == nil {
		log = slog.Default()
	}
	attrs := []any{
		"slack_channel_id", e.SlackChannelID,
		"slack_thread_ts", e.SlackThreadTS,
		"slack_user_id", e.SlackUserID,
		"outcome", e.Outcome,
		"op", e.Operation,
		"request_id", e.RequestID,
		"task_name", e.TaskName,
	}
	logWith := func(level slog.Level, event, message string, extra ...any) {
		log.Log(ctx, level, prefix+".result."+event, append(append([]any{"message", message}, extra...), attrs...)...)
	}

	topic := os.Getenv("ACCESS_RESULTS_TOPIC")
	if topic == "" {
		topic = DefaultTopic
	}
	data, err := json.Marshal(e)
	if err != nil {
		logWith(slog.LevelError, "publish_error", err.Error())
		return
	}

	// Keep the trace, not the caller's cancellation: the operation already happened.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	svc, err := newPubSub(pubCtx)
	if err != nil {
		logWith(slog.LevelError, "publish_error", "Cannot create Pub/Sub client: "+err.Error())
		return
	}
	resp, err := svc.Projects.Topics.Publish(topic, &pubsub.PublishRequest{
		Messages: []*pubsub.PubsubMessage{{
			Data: base64.StdEncoding.EncodeToString(data),
			Attributes: map[string]string{
				"function":  e.Function,
				"outcome":   e.Outcome,
				"operation": e.Operation,
			},
		}},
	}).Context(pubCtx).Do()
	if err != nil {
		logWith(slog.LevelError, "publish_error", "Result not published: "+err.Error(), "topic", topic)
		return
	}

	logWith(slog.LevelInfo, "published", "Result published for slack-bot", "topic", topic, "message_ids", resp.MessageIds)
}
