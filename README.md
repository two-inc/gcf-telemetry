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
