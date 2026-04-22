package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestTraceAwareHandlerInjectsTraceFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newTraceAwareHandler(newJSONHandler(&buf, nil), "test-proj")).WithGroup("outer")

	traceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := trace.SpanIDFromHex("0123456789abcdef")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "demo", "k", "v")

	got := decodeJSONLine(t, buf.Bytes())
	wantTrace := "projects/test-proj/traces/" + traceID.String()
	if got["logging.googleapis.com/trace"] != wantTrace {
		t.Errorf(`trace = %v, want %q`, got["logging.googleapis.com/trace"], wantTrace)
	}
	if got["logging.googleapis.com/spanId"] != spanID.String() {
		t.Errorf(`spanId = %v, want %q`, got["logging.googleapis.com/spanId"], spanID.String())
	}
	if got["logging.googleapis.com/trace_sampled"] != true {
		t.Errorf(`trace_sampled = %v, want true`, got["logging.googleapis.com/trace_sampled"])
	}
	if got["event"] != "demo" {
		t.Errorf(`event = %v, want "demo"`, got["event"])
	}
	if got["severity"] != "INFO" {
		t.Errorf(`severity = %v, want "INFO"`, got["severity"])
	}
	group, ok := got["outer"].(map[string]any)
	if !ok {
		t.Fatalf("outer group type = %T, want map[string]any", got["outer"])
	}
	if group["k"] != "v" {
		t.Errorf(`outer["k"] = %v, want "v"`, group["k"])
	}
}

func TestTraceAwareHandlerWithoutTrace(t *testing.T) {
	var buf bytes.Buffer
	slog.New(newTraceAwareHandler(newJSONHandler(&buf, nil), "test-proj")).
		InfoContext(context.Background(), "no-trace")

	got := decodeJSONLine(t, buf.Bytes())
	if _, ok := got["logging.googleapis.com/trace"]; ok {
		t.Errorf("expected no trace field, got %v", got["logging.googleapis.com/trace"])
	}
}

func decodeJSONLine(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("unmarshal log line: %v\n%s", err, raw)
	}
	return got
}
