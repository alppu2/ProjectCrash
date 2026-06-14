package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestLogWithTrace_InjectsTraceID(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	defer span.End()

	var buf bytes.Buffer
	logger := logWithTrace(ctx, slog.New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("test message")

	var record map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, buf.String())
	}

	if _, ok := record["trace_id"]; !ok {
		t.Error("log record missing trace_id field")
	}
	if _, ok := record["span_id"]; !ok {
		t.Error("log record missing span_id field")
	}
}

func TestLogWithTrace_NoSpan_ReturnsDefault(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	logger := logWithTrace(context.Background(), base)
	logger.Info("no span")

	var record map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := record["trace_id"]; ok {
		t.Error("log record should not have trace_id when no active span")
	}
}
