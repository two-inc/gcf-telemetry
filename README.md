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
  counts as final.
- Every message is plain text: an emoji, a mention when `slack_user_id` is
  set, then one sentence. Times read `2026-10-08 14:02 UTC`.

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
