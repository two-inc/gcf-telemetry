// Package telemetry wires up Cloud Logging + OpenTelemetry trace correlation
// for Go Cloud Functions.
//
// Use New to build an *slog.Logger that emits to Cloud Logging with the
// logging.googleapis.com/trace|spanId|trace_sampled fields populated from the
// OTel span context carried on each logging call's context.Context. Use
// NewHTTPHandler to wrap an http.Handler so an OTel server span is opened per
// request and X-Cloud-Trace-Context / traceparent headers are honoured.
//
// When no GCP project is discoverable from the environment (e.g. local dev,
// unit tests) New falls back to stdout JSON logging so the same code path
// runs off-cloud.
package telemetry

import (
	"context"
	"fmt"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	gcppropagator "github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	setupOnce   sync.Once
	sharedState struct {
		tp *sdktrace.TracerProvider
	}
)

// New returns a slog.Logger. When running in a GCP environment (project ID
// discoverable from env), the logger writes to Cloud Logging and decorates
// each entry with trace/span fields pulled from the OTel span context on
// ctx. Otherwise it returns a stdout JSON logger matching the historical
// Cloud Functions format (event/severity field names, WARN→WARNING rename).
//
// New is safe to call more than once per process; the global OTel provider
// and Cloud Logging client are initialised once and reused. level is
// optional — pass nil to accept the default slog level.
func New(_ context.Context, logName string, level *slog.LevelVar) *slog.Logger {
	projectID := discoverProjectID()
	if projectID == "" {
		return stdoutFallback(level)
	}

	var setupErr error
	setupOnce.Do(func() {
		exp, err := texporter.New(texporter.WithProjectID(projectID))
		if err != nil {
			setupErr = fmt.Errorf("cloud trace exporter: %w", err)
			return
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			gcppropagator.CloudTraceOneWayPropagator{},
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		sharedState.tp = tp
	})

	fallback := stdoutFallback(level)
	if setupErr != nil || sharedState.tp == nil {
		if setupErr != nil {
			fallback.Error("telemetry.init_failed", "error", setupErr)
		}
		return fallback
	}

	return slog.New(newTraceAwareHandler(newJSONHandler(os.Stdout, level), projectID))
}

// NewHTTPHandler wraps h so each request opens an OTel server span fed by the
// inbound X-Cloud-Trace-Context / traceparent headers, and subsequent
// handlers can reach that span via r.Context(). On request completion it
// force-flushes the span batch to Cloud Trace so Cloud Functions gen2's pause-
// between-invocations behaviour doesn't silently drop spans.
func NewHTTPHandler(h http.Handler, operation string) http.Handler {
	wrapped := otelhttp.NewHandler(h, operation, otelhttp.WithPropagators(otel.GetTextMapPropagator()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped.ServeHTTP(w, r)
		if sharedState.tp != nil {
			flushCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			_ = sharedState.tp.ForceFlush(flushCtx)
			cancel()
		}
	})
}

// Shutdown flushes pending trace spans and log entries. Safe to call when
// telemetry was never initialised. Cloud Functions gen2 does not surface
// shutdown hooks, so this is primarily for tests and cmd/local binaries.
func Shutdown(ctx context.Context) {
	if sharedState.tp != nil {
		_ = sharedState.tp.Shutdown(ctx)
	}
}

// discoverProjectID tries the env vars Cloud Functions / Cloud Run / App
// Engine set. GOOGLE_CLOUD_PROJECT is set by Cloud Functions gen2.
func discoverProjectID() string {
	for _, k := range []string{"GCP_PROJECT", "GOOGLE_CLOUD_PROJECT", "GOOGLE_PROJECT_ID"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// stdoutFallback mirrors the JSON handler shape used by Cloud Functions
// historically: message key renamed to `event`, level key renamed to
// `severity` with `WARNING` instead of `WARN` so Cloud Logging's severity
// filter works even on the fallback path.
func stdoutFallback(level *slog.LevelVar) *slog.Logger {
	return slog.New(newJSONHandler(os.Stdout, level))
}

func newJSONHandler(w io.Writer, level *slog.LevelVar) slog.Handler {
	leveler := slog.Leveler(slog.LevelInfo)
	if level != nil {
		leveler = level
	}
	opts := &slog.HandlerOptions{
		Level: leveler,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.MessageKey:
				a.Key = "event"
			case slog.LevelKey:
				a.Key = "severity"
				if a.Value.String() == "WARN" {
					a.Value = slog.StringValue("WARNING")
				}
			}
			return a
		},
	}
	return slog.NewJSONHandler(w, opts)
}

type traceAwareHandler struct {
	next      slog.Handler
	projectID string
	attrs     []slog.Attr
	groups    []string
}

func newTraceAwareHandler(next slog.Handler, projectID string) slog.Handler {
	return &traceAwareHandler{
		next:      next,
		projectID: projectID,
	}
}

func (h *traceAwareHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *traceAwareHandler) Handle(ctx context.Context, r slog.Record) error {
	record := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(
			slog.String("logging.googleapis.com/trace", fmt.Sprintf("projects/%s/traces/%s", h.projectID, sc.TraceID())),
			slog.String("logging.googleapis.com/spanId", sc.SpanID().String()),
			slog.Bool("logging.googleapis.com/trace_sampled", sc.IsSampled()),
		)
	}

	attrs := append([]slog.Attr{}, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	if len(h.groups) > 0 {
		attrs = wrapGroupedAttrs(h.groups, attrs)
	}
	record.AddAttrs(attrs...)
	return h.next.Handle(ctx, record)
}

func (h *traceAwareHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceAwareHandler{
		next:      h.next,
		projectID: h.projectID,
		attrs:     append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups:    append([]string{}, h.groups...),
	}
}

func (h *traceAwareHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &traceAwareHandler{
		next:      h.next,
		projectID: h.projectID,
		attrs:     append([]slog.Attr{}, h.attrs...),
		groups:    append(append([]string{}, h.groups...), name),
	}
}

func wrapGroupedAttrs(groups []string, attrs []slog.Attr) []slog.Attr {
	wrapped := append([]slog.Attr{}, attrs...)
	for i := len(groups) - 1; i >= 0; i-- {
		wrapped = []slog.Attr{{
			Key:   groups[i],
			Value: slog.GroupValue(wrapped...),
		}}
	}
	return wrapped
}
