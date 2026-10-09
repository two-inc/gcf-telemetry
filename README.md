# gcf-telemetry

[![Test](https://github.com/two-inc/gcf-telemetry/actions/workflows/test.yaml/badge.svg)](https://github.com/two-inc/gcf-telemetry/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/two-inc/gcf-telemetry)](https://github.com/two-inc/gcf-telemetry/releases/latest)

OpenTelemetry + Cloud Logging glue for Go Cloud Functions.

Wires Cloud Logging with OTel trace correlation so every log line carries
`logging.googleapis.com/trace`, `spanId`, and `trace_sampled` pulled from the
OTel span context on each `*Context` logging call. Inbound HTTP handlers are
wrapped to open a server span fed by `X-Cloud-Trace-Context` or `traceparent`.

## Usage

```go
import "github.com/two-inc/gcf-telemetry"

func init() {
    logger := telemetry.New(context.Background(), "my-function", nil)
    // ...
}

func Handler(w http.ResponseWriter, r *http.Request) {
    // r.Context() now carries an OTel server span
}

var _ = telemetry.NewHTTPHandler(http.HandlerFunc(Handler), "my-function")
```

When no GCP project is discoverable from the environment, `New` falls back to
stdout JSON logging so the same code path runs off-cloud.

## Reporting results to slack-bot (`slackthread`)

`github.com/two-inc/gcf-telemetry/slackthread` lets the /two access backends
(gcp-, secret- and groups-access-manager, db-permission-manager,
ip-firewall-manager) report a result to slack-bot, so the fields, wording and
log events are the same in every backend. Results go to the `access-results`
Pub/Sub topic. slack-bot, the only holder of the bot token, renders each one,
replies in the request thread, updates its cards and records the real status.
A backend needs `pubsub.publisher` on that topic and no Slack token
(PLAT-2588).

```go
type AccessRequest struct {
    // ...
    slackthread.Thread // slack_channel_id, slack_thread_ts, slack_user_id
}

src := slackthread.Source{Function: "gcp-access-manager", RequestID: req.RequestID, TaskName: slackthread.TaskName(r)}
slackthread.Publish(ctx, logger, "gcp_access_manager", src, req.Thread, slackthread.Reply{
    Outcome: slackthread.Granted, Op: "GRANT", What: "`roles/viewer` on `proj`", Until: &expiry,
})

// failures: report only when Cloud Tasks will not retry again
if permanent || slackthread.FinalAttempt(r) {
    slackthread.Publish(ctx, logger, prefix, src, req.Thread, slackthread.Reply{Outcome: slackthread.Failed, Op: req.Operation, What: what, Err: err})
}
```

- `FinalAttempt` compares `X-CloudTasks-TaskRetryCount` with
  `CLOUD_TASKS_MAX_ATTEMPTS` (default 5; set it to the queue's
  `max_attempts`). A request without the header is not from Cloud Tasks and
  counts as final. **Requirement:** Cloud Tasks stops retrying only once both
  `max_attempts` and `max_retry_duration` are reached, so the queue must leave
  `max_retry_duration` unset or 0, `max_attempts` must be a finite positive
  number (not `-1`, unlimited), `CLOUD_TASKS_MAX_ATTEMPTS` must equal it, and
  tasks must not override the queue's retry settings.
  Otherwise a failure can be reported early, more than once, or (when
  `CLOUD_TASKS_MAX_ATTEMPTS` is higher than the queue's `max_attempts`) not
  at all.
- `Text` gives the reply wording: plain text, an emoji, a mention when
  `slack_user_id` is set, then one sentence. Times read `2026-10-08 14:02 UTC`.

### How backends use this

Cloud Tasks retries any non-2xx response, so status codes decide how often a
failure is reported. For operations delivered by Cloud Tasks (GRANT, REVOKE,
DOWNGRADE, firewall block/unblock):

| Result | Status | Published |
|---|---|---|
| Success | `200` | the matching outcome, once |
| Permanent failure (bad payload, validation, or a non-retryable GCP error: 400, 403 permission denied, 404 not found) | `299`, a 2xx, so Cloud Tasks stops | `Failed`, once, straight away |
| Transient failure (5xx, timeouts, and retryable 4xx: 408, 409 policy conflict, 429) | `503`, retried | `Failed` only when `FinalAttempt(r)` |
| Auth failure | `403` | none: the payload is not trusted |
| Undecodable body | `299` | `Failed` when its `request_id` still decoded, so slack-bot can match it; otherwise none |

Direct synchronous reads (groups `LIST_MANAGERS`, `LIST_USER_GROUPS`) keep
`400` for bad input and never publish.

Queue requirement: `max_retry_duration` unset or 0, a finite positive
`max_attempts`, and `CLOUD_TASKS_MAX_ATTEMPTS` set in terraform from the
queue's `max_attempts`, so the two cannot drift.

### `Publish` details

- The topic is `ACCESS_RESULTS_TOPIC`, default
  `projects/two-slack-bot/topics/access-results`. Credentials come from the
  function's own service account.
- The event carries facts, not wording: `request_id`, `operation`, `outcome`
  (the `Outcome` name), `what`, `who`, `until` (RFC 3339, UTC), `error`, the
  three Slack thread fields, `function` and `task_name`. Attributes repeat
  `function`, `outcome` and `operation`.
- It publishes even when the request has no thread, because slack-bot records
  the status regardless and posts ip-firewall-manager's alerts to a fallback
  channel.
- Best-effort: it never returns an error. Log events:
  `<prefix>.result.{published,publish_error}`.
- Status codes and `FinalAttempt` apply exactly as above. A task Cloud Tasks
  runs twice publishes two messages with different message ids, so a consumer
  that should drop the second must key on `task_name` and `outcome`, not on
  the message id alone.
- ip-firewall-manager uses `PublishAlert` instead: its event carries an
  `alert` object (`title`, `text`, `fields` as `{title, value}`, `severity`,
  `warning`, `test_mode`, `button` as `{text, action_id, value}`) that slack-bot
  renders and routes as the function's own alert, fallback channel included.
  Those names match slack-bot's `firewall_alerts`; keep the two in step. Its
  `outcome` names the alert kind (`block`, `unblock_failed`, ...), so two alerts
  from one task are not deduped into one.

## Releases

Every push to `main` runs tests and then auto-bumps a patch tag via
[github-tag-action](https://github.com/mathieudutour/github-tag-action), which
triggers [GoReleaser](https://goreleaser.com) to publish a GitHub release with
a generated changelog. Use conventional-commit prefixes in commit messages to
control the bump:

- `fix:` → patch bump
- `feat:` → minor bump
- `feat!:` or `BREAKING CHANGE:` → major bump
- `docs:`, `chore:`, `test:`, `ci:` → excluded from changelog, default patch
