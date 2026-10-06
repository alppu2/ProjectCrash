package main

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logWithTrace returns a logger with trace_id and span_id fields from ctx.
// Returns base unchanged if no active span is in ctx.
func logWithTrace(ctx context.Context, base *slog.Logger) *slog.Logger {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return base
	}
	return base.With(
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String(),
	)
}
