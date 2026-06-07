# Observability Design — Grafana LGTM Stack

## Goal

Add full observability (logs, metrics, traces) to `order-service` and `inventory-service` using the Grafana LGTM stack. Single Grafana UI for all three pillars with cross-pillar correlation (trace → logs).

## Stack

| Pillar | Instrumentation | Backend | UI |
|--------|----------------|---------|-----|
| Logs | `slog` (JSON to stdout) → Promtail | Loki | Grafana |
| Metrics | `prometheus/client_golang` `/metrics` endpoint | Prometheus | Grafana |
| Traces | OpenTelemetry SDK (OTLP exporter) | Tempo | Grafana |

RabbitMQ metrics scraped directly via built-in `rabbitmq-prometheus` plugin (port 15692) — no separate exporter container.

## Architecture

```
order-service   ──┐
                  ├─→ :9091/metrics ──→ Prometheus
inventory-service─┘

order-service   ──┐
                  ├─→ OTLP (gRPC :4317) ──→ Tempo
inventory-service─┘

Docker stdout   ──→ Promtail ──→ Loki

RabbitMQ :15692 ──→ Prometheus

Prometheus ──┐
Loki       ──┼──→ Grafana :3000
Tempo      ──┘
```

## Logging

### Library
`log/slog` (Go stdlib, 1.21+). No new dependency.

### Initialization
Both services initialize a JSON handler at startup:

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))
```

### Migration
All `log.Printf` / `log.Println` / `log.Fatal` calls replaced with structured `slog` equivalents:

```go
// before
log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)

// after
slog.Info("received packet", "payload", in.Payload, "client_id", in.ClientId)
```

### Trace-Log Correlation
A context helper extracts the OTel trace ID from `context.Context` and injects it into the slog record. Every log line during a traced request carries `trace_id` and `span_id` fields. Grafana uses these to link from a Tempo trace span to the corresponding Loki log lines.

### Loki Pipeline
Promtail runs as a Docker container. It mounts `/var/lib/docker/containers`, scrapes stdout from containers labelled `order-service` and `inventory-service`, and ships JSON log lines to Loki. No application code change needed for collection.

## Metrics

### Prometheus Endpoint
Both services expose an HTTP `/metrics` endpoint on port `9091` (separate from gRPC) using `prometheus/client_golang`. Go runtime metrics (GC, goroutines, memory) are registered automatically on import. Both services use port `9091` internally — no conflict since each container has its own network identity in the Docker bridge network.

### order-service custom metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `packets_processed_total` | Counter | `client_id` | Packets successfully persisted to MongoDB |
| `packets_failed_total` | Counter | `reason` | Packets that failed processing |
| `rabbitmq_publish_errors_total` | Counter | — | Failed RabbitMQ publish attempts |
| `mongodb_op_duration_seconds` | Histogram | `operation` | MongoDB operation latency |
| `grpc_request_duration_seconds` | Histogram | `method`, `status` | gRPC handler latency (via interceptor) |

### inventory-service custom metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `packets_consumed_total` | Counter | — | Messages successfully consumed |
| `packets_decode_errors_total` | Counter | — | Messages that failed JSON decode |
| `rabbitmq_consume_lag_seconds` | Histogram | — | Time between publish and consume (requires `published_at` Unix timestamp added to message body by order-service) |

### RabbitMQ metrics
Scraped via RabbitMQ's built-in Prometheus plugin. The plugin ships with `rabbitmq:3-management-alpine` but is not enabled by default — enable it via `RABBITMQ_ENABLED_PLUGINS` env var in compose:

```yaml
environment:
  RABBITMQ_ENABLED_PLUGINS: "rabbitmq_management,rabbitmq_prometheus"
```

Expose port `15692` for Prometheus scraping. Key metrics: `rabbitmq_queue_messages`, `rabbitmq_queue_messages_ready`, message rates, connection counts.

## Tracing

### SDK Setup
Both services initialize an OTel OTLP/gRPC exporter pointing at Tempo on startup. Tracer provider registered as global.

### gRPC (automatic)
OTel gRPC server interceptors added to `grpc.NewServer()`. Every RPC (`SendPacket`, `StressTest`, `StreamDisturbance`) gets a span automatically with method name, status code, and duration.

### RabbitMQ (manual spans)
No official Go OTel AMQP library exists — spans are created manually.

**order-service publish:**
```go
ctx, span := tracer.Start(ctx, "rabbitmq.publish", trace.WithAttributes(
    attribute.String("messaging.system", "rabbitmq"),
    attribute.String("messaging.destination", "packets"),
))
defer span.End()

// Propagate trace context into message headers
headers := amqp.Table{}
otel.GetTextMapPropagator().Inject(ctx, amqpHeaderCarrier(headers))
```

**inventory-service consume:**
```go
// Extract trace context from message headers to continue the same trace
ctx := otel.GetTextMapPropagator().Extract(context.Background(), amqpHeaderCarrier(d.Headers))
ctx, span := tracer.Start(ctx, "rabbitmq.consume", trace.WithAttributes(
    attribute.String("messaging.system", "rabbitmq"),
    attribute.String("messaging.destination", "packets"),
))
defer span.End()
```

This produces a single trace spanning: gRPC call → MongoDB insert → RabbitMQ publish → RabbitMQ consume.

### MongoDB (manual spans)
Spans wrapped around `InsertOne` calls to capture MongoDB op latency in traces.

### What is automatic vs manual

| Instrumentation | Mode | Reason |
|----------------|------|--------|
| gRPC request spans | Automatic | OTel gRPC interceptor library |
| Go runtime metrics | Automatic | `client_golang` registers on import |
| Log capture to Loki | Automatic | Promtail reads Docker stdout |
| Trace context on gRPC | Automatic | Interceptor handles W3C `traceparent` |
| RabbitMQ spans | Manual | No official Go OTel AMQP library |
| MongoDB spans | Manual | Driver has no built-in OTel support |
| Trace ID → slog | Manual | Context middleware needed |
| Custom business metrics | Manual | Domain-specific — can't be auto-inferred |

## Grafana Provisioning

Auto-configured via mounted YAML files. No manual click-config on first boot.

### Directory structure
```
grafana/
  provisioning/
    datasources/
      datasources.yaml    ← Prometheus, Loki, Tempo datasources
    dashboards/
      dashboards.yaml     ← dashboard provider config
  dashboards/
    overview.json         ← starter dashboard
```

### Cross-pillar correlation
- **Tempo → Loki:** `traceToLogs` config on Tempo datasource — clicking a trace span jumps to Loki filtered by `trace_id`
- **Loki → Tempo:** derived field on Loki datasource extracts `trace_id` from JSON log lines and links to Tempo

### Starter dashboard panels
- RabbitMQ queue depth (`rabbitmq_queue_messages`)
- gRPC request rate and p99 latency per method
- `packets_processed_total` rate
- `rabbitmq_publish_errors_total`
- Loki log stream (order-service + inventory-service)

## New Docker Compose Services

```yaml
prometheus:   scrapes order-service, inventory-service, rabbitmq
loki:         receives logs from promtail
tempo:        receives OTLP traces, port 4317
promtail:     scrapes docker container stdout → loki
grafana:      port 3000, auto-provisioned
```

RabbitMQ: add port `15692` to existing service for Prometheus scraping.

## File Changes Summary

| File | Change |
|------|--------|
| `docker-compose.yml` | Add prometheus, loki, tempo, promtail, grafana; expose rabbitmq :15692 |
| `order-service/main.go` | slog init, OTel init, Prometheus endpoint, gRPC interceptors, manual spans |
| `inventory-service/main.go` | slog init, OTel init, Prometheus endpoint, manual consume span |
| `prometheus/prometheus.yml` | Scrape configs |
| `loki/loki-config.yml` | Loki config |
| `tempo/tempo-config.yml` | Tempo config |
| `promtail/promtail-config.yml` | Promtail scrape config |
| `grafana/provisioning/datasources/datasources.yaml` | Datasource auto-provision |
| `grafana/provisioning/dashboards/dashboards.yaml` | Dashboard provider |
| `grafana/dashboards/overview.json` | Starter dashboard |
| `order-service/go.mod` | Add OTel, Prometheus deps |
| `inventory-service/go.mod` | Add OTel, Prometheus deps |
