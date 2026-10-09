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

## Replying in the request thread (`slackthread`)

`github.com/two-inc/gcf-telemetry/slackthread` lets the /two access backends
(gcp-, secret- and groups-access-manager, db-permission-manager) report a grant
or revoke result as a reply in the Slack thread the request came from, so the
fields, wording and log events are the same in every backend.

```go
type AccessRequest struct {
    // ...
    slackthread.Thread // slack_channel_id, slack_thread_ts, slack_user_id
}

slackthread.Post(ctx, logger, "gcp_access_manager", req.Thread, slackthread.Reply{
    Outcome: slackthread.Granted, Op: "GRANT", What: "`roles/viewer` on `proj`", Until: &expiry,
})

// failures: report only when Cloud Tasks will not retry again
if permanent || slackthread.FinalAttempt(r) {
    slackthread.Post(ctx, logger, prefix, req.Thread, slackthread.Reply{Outcome: slackthread.Failed, Op: req.Operation, What: what, Err: err})
}
```

- `Post` is best-effort: it never returns an error. With no thread it only
  logs `<prefix>.slack.no_thread` at INFO. Other events:
  `<prefix>.slack.{posted,token_unreadable,post_error,post_rejected}`.
- The bot token is read on every call from `SLACK_BOT_TOKEN_FILE` (default
  `/etc/secrets/slack-bot/SLACK_BOT_TOKEN`), so a rotation needs no redeploy.
  It is never logged.
- `FinalAttempt` compares `X-CloudTasks-TaskRetryCount` with
  `CLOUD_TASKS_MAX_ATTEMPTS` (default 5; set it to the queue's
  `max_attempts`). A request without the header is not from Cloud Tasks and
  counts as final. **Requirement:** Cloud Tasks stops retrying only once both
  `max_attempts` and `max_retry_duration` are reached, so the queue must leave
  `max_retry_duration` unset or 0, `max_attempts` must be a finite positive
  number (not `-1`, unlimited), `CLOUD_TASKS_MAX_ATTEMPTS` must equal it, and
  tasks must not override the queue's retry settings.
  Otherwise a failure can be posted early, more than once, or (when
  `CLOUD_TASKS_MAX_ATTEMPTS` is higher than the queue's `max_attempts`) not
  at all.
- Every message is plain text: an emoji, a mention when `slack_user_id` is
  set, then one sentence. Times read `2026-10-08 14:02 UTC`.

### How backends use this

Cloud Tasks retries any non-2xx response, so status codes decide how often a
failure is reported. For operations delivered by Cloud Tasks (GRANT, REVOKE,
DOWNGRADE, firewall block/unblock):

| Result | Status | Reply |
|---|---|---|
| Success | `200` | the matching outcome, once |
| Permanent failure (bad payload, validation, or a non-retryable GCP error: 400, 403 permission denied, 404 not found) | `299`, a 2xx, so Cloud Tasks stops | `Failed`, once, straight away |
| Transient failure (5xx, timeouts, and retryable 4xx: 408, 409 policy conflict, 429) | `503`, retried | `Failed` only when `FinalAttempt(r)` |
| Auth failure | `403` | none: the payload is not trusted |
| Undecodable body | `299` | none: there is no thread to reply to |

Direct synchronous reads (groups `LIST_MANAGERS`, `LIST_USER_GROUPS`) keep
`400` for bad input and never post.

Queue requirement: `max_retry_duration` unset or 0, a finite positive
`max_attempts`, and `CLOUD_TASKS_MAX_ATTEMPTS` set in terraform from the
queue's `max_attempts`, so the two cannot drift.

Duplicates: Cloud Tasks may rarely run a task twice, so a reply can appear
twice. This is accepted, because deduping would need a shared store keyed by
task name.

### Publishing results for slack-bot (`Publish`)

`Publish` is replacing `Post` (PLAT-2588). It sends the same result as an
event to the `access-results` Pub/Sub topic, and slack-bot, the only holder of
the bot token, renders it, replies in the thread, updates its cards and records
the real status. A backend then needs `pubsub.publisher` on that topic and no
Slack token.

```go
slackthread.Publish(ctx, logger, "gcp_access_manager",
    slackthread.Source{Function: "gcp-access-manager", RequestID: req.RequestID, TaskName: slackthread.TaskName(r)},
    req.Thread, slackthread.Reply{Outcome: slackthread.Granted, Op: "GRANT", What: what, Until: &expiry})
```

- The topic is `ACCESS_RESULTS_TOPIC`, default
  `projects/two-slack-bot/topics/access-results`. Credentials come from the
  function's own service account.
- The event carries facts, not wording: `request_id`, `operation`, `outcome`
  (the `Outcome` name), `what`, `who`, `until` (RFC 3339, UTC), `error`, the
  three Slack thread fields, `function` and `task_name`. Attributes repeat
  `function`, `outcome` and `operation`.
- It publishes even when the request has no thread, because slack-bot records
  the status regardless and ip-firewall-manager posts to a fallback channel.
- Best-effort like `Post`: it never returns an error. Log events:
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
