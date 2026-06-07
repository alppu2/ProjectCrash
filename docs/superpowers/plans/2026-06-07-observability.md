# Observability (Grafana LGTM Stack) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add full observability (structured logs, Prometheus metrics, distributed traces) to `order-service` and `inventory-service`, with Grafana as the single UI for all three pillars.

**Architecture:** slog writes JSON to stdout; Promtail ships it to Loki. Both services expose `/metrics` on port 9091; Prometheus scrapes them plus RabbitMQ's built-in plugin on 15692. OTel SDK exports OTLP traces to Tempo. gRPC spans are automatic via `otelgrpc`; RabbitMQ and MongoDB spans are manual. Trace IDs are injected into slog records for Grafana trace↔log correlation.

**Tech Stack:** Go `log/slog`, `github.com/prometheus/client_golang`, `go.opentelemetry.io/otel`, `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`, Prometheus, Loki, Tempo, Promtail, Grafana

**Spec:** `docs/superpowers/specs/2026-06-07-observability-design.md`

---

## File Map

| File | Action | Responsibility |
|------|--------|---------------|
| `prometheus/prometheus.yml` | Create | Scrape targets for both services + RabbitMQ |
| `loki/loki-config.yml` | Create | Loki storage + ingester config |
| `tempo/tempo-config.yml` | Create | Tempo OTLP receiver + local storage |
| `promtail/promtail-config.yml` | Create | Docker socket scrape → Loki |
| `grafana/provisioning/datasources/datasources.yaml` | Create | Auto-provision Prometheus, Loki, Tempo with correlation config |
| `grafana/provisioning/dashboards/dashboards.yaml` | Create | Dashboard provider config |
| `grafana/dashboards/overview.json` | Create | Starter dashboard (queue depth, error rate, logs) |
| `docker-compose.yml` | Modify | Add 5 infra services; update rabbitmq |
| `order-service/telemetry.go` | Create | OTel tracer init and shutdown |
| `order-service/metrics.go` | Create | Prometheus metric variable definitions |
| `order-service/amqp_carrier.go` | Create | `amqpHeaderCarrier` type for W3C propagation over AMQP |
| `order-service/log_trace.go` | Create | `logWithTrace(ctx)` helper — injects trace_id/span_id into slog |
| `order-service/main.go` | Modify | slog init, OTel init, metrics HTTP server, gRPC stats handler, spans |
| `inventory-service/telemetry.go` | Create | OTel tracer init and shutdown |
| `inventory-service/metrics.go` | Create | Prometheus metric variable definitions |
| `inventory-service/amqp_carrier.go` | Create | Same `amqpHeaderCarrier` type |
| `inventory-service/log_trace.go` | Create | Same `logWithTrace(ctx)` helper |
| `inventory-service/main.go` | Modify | slog init, OTel init, metrics HTTP server, consume span |

---

## Task 1: Infra Config Files + Docker Compose

**Files:**
- Create: `prometheus/prometheus.yml`
- Create: `loki/loki-config.yml`
- Create: `tempo/tempo-config.yml`
- Create: `promtail/promtail-config.yml`
- Modify: `docker-compose.yml`

- [ ] **Step 1: Create `prometheus/prometheus.yml`**

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: order-service
    static_configs:
      - targets: ['order-service:9091']

  - job_name: inventory-service
    static_configs:
      - targets: ['inventory-service:9091']

  - job_name: rabbitmq
    static_configs:
      - targets: ['rabbitmq:15692']
```

- [ ] **Step 2: Create `loki/loki-config.yml`**

```yaml
auth_enabled: false

server:
  http_listen_port: 3100
  grpc_listen_port: 9096

common:
  instance_addr: 127.0.0.1
  path_prefix: /tmp/loki
  storage:
    filesystem:
      chunks_directory: /tmp/loki/chunks
      rules_directory: /tmp/loki/rules
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory

schema_config:
  configs:
    - from: 2020-10-24
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h
```

- [ ] **Step 3: Create `tempo/tempo-config.yml`**

```yaml
server:
  http_listen_port: 3200

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317

ingester:
  max_block_duration: 5m

compactor:
  compaction:
    block_retention: 1h

storage:
  trace:
    backend: local
    local:
      path: /tmp/tempo/blocks
    wal:
      path: /tmp/tempo/wal
```

- [ ] **Step 4: Create `promtail/promtail-config.yml`**

```yaml
server:
  http_listen_port: 9080
  grpc_listen_port: 0

positions:
  filename: /tmp/positions.yaml

clients:
  - url: http://loki:3100/loki/api/v1/push

scrape_configs:
  - job_name: docker
    docker_sd_configs:
      - host: unix:///var/run/docker.sock
        refresh_interval: 5s
    relabel_configs:
      - source_labels: [__meta_docker_container_name]
        regex: /(order-service|inventory-service)
        action: keep
      - source_labels: [__meta_docker_container_name]
        regex: /(.*)
        target_label: container
        replacement: $1
```

- [ ] **Step 5: Update `docker-compose.yml`**

Add these services before the `networks:` key:

```yaml
  prometheus:
    image: prom/prometheus:latest
    container_name: prometheus
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml
    ports:
      - "9090:9090"
    networks:
      - micro-network

  loki:
    image: grafana/loki:latest
    container_name: loki
    volumes:
      - ./loki/loki-config.yml:/etc/loki/local-config.yaml
    ports:
      - "3100:3100"
    command: -config.file=/etc/loki/local-config.yaml
    networks:
      - micro-network

  tempo:
    image: grafana/tempo:latest
    container_name: tempo
    volumes:
      - ./tempo/tempo-config.yml:/etc/tempo.yaml
    ports:
      - "3200:3200"
      - "4317:4317"
    command: -config.file=/etc/tempo.yaml
    networks:
      - micro-network

  promtail:
    image: grafana/promtail:latest
    container_name: promtail
    volumes:
      - ./promtail/promtail-config.yml:/etc/promtail/config.yml
      - /var/run/docker.sock:/var/run/docker.sock
    command: -config.file=/etc/promtail/config.yml
    networks:
      - micro-network
    depends_on:
      - loki

  grafana:
    image: grafana/grafana:latest
    container_name: grafana
    ports:
      - "3000:3000"
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning
      - ./grafana/dashboards:/var/lib/grafana/dashboards
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
    networks:
      - micro-network
    depends_on:
      - prometheus
      - loki
      - tempo
```

Replace the existing `rabbitmq:` service block with:

```yaml
  rabbitmq:
    image: rabbitmq:3-management-alpine
    container_name: rabbitmq
    ports:
      - "5672:5672"
      - "15672:15672"
      - "15692:15692"
    environment:
      RABBITMQ_ENABLED_PLUGINS: "rabbitmq_management,rabbitmq_prometheus"
    networks:
      - micro-network
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
```

- [ ] **Step 6: Verify infra starts**

```bash
docker compose up prometheus loki tempo promtail grafana rabbitmq -d
```

Expected: all containers reach `healthy` or `running`. Then:
- `curl http://localhost:9090/-/ready` → `Prometheus Server is Ready.`
- `curl http://localhost:3100/ready` → `ready`
- `curl http://localhost:3200/ready` → `ready`
- Open `http://localhost:3000` in browser → Grafana login page (anonymous access, no password needed)

- [ ] **Step 7: Commit**

```bash
git add prometheus/ loki/ tempo/ promtail/ docker-compose.yml
git commit -m "feat(infra): add prometheus, loki, tempo, promtail, grafana to compose"
```

---

## Task 2: Grafana Provisioning

**Files:**
- Create: `grafana/provisioning/datasources/datasources.yaml`
- Create: `grafana/provisioning/dashboards/dashboards.yaml`
- Create: `grafana/dashboards/overview.json`

- [ ] **Step 1: Create `grafana/provisioning/datasources/datasources.yaml`**

```yaml
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    uid: prometheus
    url: http://prometheus:9090
    isDefault: true
    jsonData:
      httpMethod: POST

  - name: Loki
    type: loki
    uid: loki
    url: http://loki:3100
    jsonData:
      derivedFields:
        - name: TraceID
          matcherRegex: '"trace_id":"(\w+)"'
          url: "${__value.raw}"
          datasourceUid: tempo

  - name: Tempo
    type: tempo
    uid: tempo
    url: http://tempo:3200
    jsonData:
      tracesToLogsV2:
        datasourceUid: loki
        filterByTraceID: true
        customQuery: true
        query: '{container=~"order-service|inventory-service"} | json | trace_id = "${__trace.traceId}"'
      serviceMap:
        datasourceUid: prometheus
```

- [ ] **Step 2: Create `grafana/provisioning/dashboards/dashboards.yaml`**

```yaml
apiVersion: 1

providers:
  - name: default
    folder: ProjectCrash
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 3: Create `grafana/dashboards/overview.json`**

```json
{
  "title": "ProjectCrash Overview",
  "uid": "projectcrash-overview",
  "version": 1,
  "schemaVersion": 36,
  "refresh": "10s",
  "time": {"from": "now-1h", "to": "now"},
  "tags": ["projectcrash"],
  "panels": [
    {
      "id": 1,
      "title": "RabbitMQ Queue Depth (packets)",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
      "datasource": {"type": "prometheus", "uid": "prometheus"},
      "targets": [
        {
          "expr": "rabbitmq_queue_messages{queue=\"packets\"}",
          "legendFormat": "queue depth",
          "refId": "A"
        }
      ]
    },
    {
      "id": 2,
      "title": "Packets Processed Rate (per second)",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 0},
      "datasource": {"type": "prometheus", "uid": "prometheus"},
      "targets": [
        {
          "expr": "rate(packets_processed_total[1m])",
          "legendFormat": "{{client_id}}",
          "refId": "A"
        }
      ]
    },
    {
      "id": 3,
      "title": "RabbitMQ Publish Errors (per second)",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
      "datasource": {"type": "prometheus", "uid": "prometheus"},
      "targets": [
        {
          "expr": "rate(rabbitmq_publish_errors_total[1m])",
          "legendFormat": "publish errors/s",
          "refId": "A"
        }
      ]
    },
    {
      "id": 4,
      "title": "Service Logs",
      "type": "logs",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
      "datasource": {"type": "loki", "uid": "loki"},
      "options": {
        "dedupStrategy": "none",
        "enableLogDetails": true,
        "prettifyLogMessage": false,
        "showTime": true,
        "sortOrder": "Descending"
      },
      "targets": [
        {
          "expr": "{container=~\"order-service|inventory-service\"}",
          "refId": "A"
        }
      ]
    }
  ],
  "templating": {"list": []},
  "annotations": {"list": []},
  "links": []
}
```

- [ ] **Step 4: Restart Grafana to pick up provisioning**

```bash
docker compose restart grafana
```

Open `http://localhost:3000/datasources` — verify Prometheus, Loki, Tempo all listed with green "Data source connected" status.

Open `http://localhost:3000/dashboards` → ProjectCrash folder → "ProjectCrash Overview" dashboard exists.

- [ ] **Step 5: Commit**

```bash
git add grafana/
git commit -m "feat(grafana): provision Prometheus/Loki/Tempo datasources and starter dashboard"
```

---

## Task 3: Structured Logging in order-service

**Files:**
- Modify: `order-service/main.go`

`slog` is in stdlib (`log/slog`) — no new dependency. Replace all `log.*` calls. Do NOT add OTel or metrics yet (those come in Tasks 5 and 7).

- [ ] **Step 1: Add slog init to `main()` in `order-service/main.go`**

Add this as the very first line of `main()`, before any other code:

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))
```

Update imports — remove `"log"`, add `"log/slog"`. `"os"` is already imported.

- [ ] **Step 2: Replace all `log.*` calls in `connectRabbitMQ`**

```go
// Replace:
log.Printf("RabbitMQ not ready, retrying (%d/30)...", i+1)
// With:
slog.Info("rabbitmq not ready, retrying", "attempt", i+1, "max", 30)
```

- [ ] **Step 3: Replace all `log.*` calls in `ensureChannel`**

```go
// Replace:
log.Println("RabbitMQ channel closed, reconnecting...")
// With:
slog.Warn("rabbitmq channel closed, reconnecting")

// Replace:
log.Println("RabbitMQ channel reopened")
// With:
slog.Info("rabbitmq channel reopened")

// Replace:
log.Println("RabbitMQ reconnected")
// With:
slog.Info("rabbitmq reconnected")
```

- [ ] **Step 4: Replace all `log.*` calls in `SendPacket`**

```go
// Replace:
log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)
// With:
slog.Info("received packet", "payload", in.Payload, "client_id", in.ClientId)

// Replace:
log.Printf("WARN: failed to marshal packet for RabbitMQ: %v", err)
// With:
slog.Warn("failed to marshal packet for rabbitmq", "error", err)

// Replace:
log.Printf("WARN: RabbitMQ unavailable: %v", err)
// With:
slog.Warn("rabbitmq unavailable", "error", err)

// Replace:
log.Printf("WARN: failed to publish to RabbitMQ: %v", pubErr)
// With:
slog.Warn("failed to publish to rabbitmq", "error", pubErr)
```

- [ ] **Step 5: Replace all `log.*` calls in `StressTest`**

```go
// Replace:
log.Printf("WARN: RabbitMQ unavailable for stress test: %v", err)
// With:
slog.Warn("rabbitmq unavailable for stress test", "error", err)

// Replace:
log.Printf("WARN: failed to marshal packet %d for RabbitMQ: %v", i, err)
// With:
slog.Warn("failed to marshal packet for rabbitmq", "sequence", i, "error", err)

// Replace (the publish error inside the loop):
log.Printf("WARN: failed to publish packet %d to RabbitMQ: %v", i, pubErr)
// With:
slog.Warn("failed to publish packet to rabbitmq", "sequence", i, "error", pubErr)

// Replace:
log.Printf("Stress test: processed %d / %d packets", i, in.Count)
// With:
slog.Info("stress test progress", "processed", i, "total", in.Count)
```

- [ ] **Step 6: Replace all `log.*` calls in `StreamDisturbance`**

```go
// Replace:
log.Printf("Stress test: Received %d packets so far...", packetCount)
// With:
slog.Info("stream disturbance progress", "received", packetCount)
```

- [ ] **Step 7: Replace `log.Fatal` calls in `main()`**

`slog` has no `Fatal` — use `slog.Error` + `os.Exit(1)`:

```go
// Replace:
log.Fatal(err)
// With:
slog.Error("failed to connect to mongodb", "error", err)
os.Exit(1)

// Replace:
log.Fatalf("RabbitMQ setup failed: %v", err)
// With:
slog.Error("rabbitmq setup failed", "error", err)
os.Exit(1)

// Replace:
log.Fatalf("failed to listen: %v", err)
// With:
slog.Error("failed to listen", "error", err, "port", grpcPort)
os.Exit(1)

// Replace:
log.Printf("Order Service (gRPC) listening on %s", grpcPort)
// With:
slog.Info("order service listening", "port", grpcPort)

// Replace:
log.Fatalf("failed to serve: %v", err)
// With:
slog.Error("grpc server failed", "error", err)
os.Exit(1)
```

- [ ] **Step 8: Build and verify JSON output**

```bash
cd order-service && go build ./...
```

Expected: no errors. Then run compose and check logs:

```bash
docker compose up order-service -d --build && docker logs order-service
```

Expected: JSON lines like `{"time":"...","level":"INFO","msg":"order service listening","port":":50051"}`

- [ ] **Step 9: Commit**

```bash
git add order-service/main.go
git commit -m "feat(order-service): replace log.Printf with structured slog"
```

---

## Task 4: Structured Logging in inventory-service

**Files:**
- Modify: `inventory-service/main.go`

Same pattern as Task 3. `"os"` is already imported. Remove `"log"`, add `"log/slog"`.

- [ ] **Step 1: Add slog init to `main()` in `inventory-service/main.go`**

Add as the very first line of `main()`:

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))
```

- [ ] **Step 2: Replace all `log.*` calls in `connectRabbitMQ`**

```go
// Replace:
log.Printf("RabbitMQ not ready, retrying (%d/30)...", i+1)
// With:
slog.Info("rabbitmq not ready, retrying", "attempt", i+1, "max", 30)
```

- [ ] **Step 3: Replace all `log.*` calls in `main()`**

```go
// Replace:
log.Fatalf("RabbitMQ setup failed: %v", err)
// With:
slog.Error("rabbitmq setup failed", "error", err)
os.Exit(1)

// Replace:
log.Fatalf("failed to register consumer: %v", err)
// With:
slog.Error("failed to register consumer", "error", err)
os.Exit(1)

// Replace:
log.Println("Inventory Service listening for packets...")
// With:
slog.Info("inventory service listening for packets")
```

- [ ] **Step 4: Replace log calls in the consume loop**

```go
// Replace:
log.Printf("ERROR: failed to decode message: %v", err)
// With:
slog.Error("failed to decode message", "error", err)

// Replace:
log.Printf("Received packet — client_id: %s, payload: %s, sequence_number: %d",
    packet.ClientId, packet.Payload, packet.SequenceNumber)
// With:
slog.Info("received packet",
    "client_id", packet.ClientId,
    "payload", packet.Payload,
    "sequence_number", packet.SequenceNumber)
```

- [ ] **Step 5: Build and verify**

```bash
cd inventory-service && go build ./...
docker compose up inventory-service -d --build && docker logs inventory-service
```

Expected: JSON log line `{"time":"...","level":"INFO","msg":"inventory service listening for packets"}`

- [ ] **Step 6: Commit**

```bash
git add inventory-service/main.go
git commit -m "feat(inventory-service): replace log.Printf with structured slog"
```

---

## Task 5: Prometheus Metrics in order-service

**Files:**
- Create: `order-service/metrics.go`
- Modify: `order-service/main.go`

- [ ] **Step 1: Add Prometheus dependency**

```bash
cd order-service && go get github.com/prometheus/client_golang/prometheus
cd order-service && go get github.com/prometheus/client_golang/prometheus/promauto
cd order-service && go get github.com/prometheus/client_golang/prometheus/promhttp
```

- [ ] **Step 2: Create `order-service/metrics.go`**

```go
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	packetsProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "packets_processed_total",
		Help: "Total packets successfully persisted to MongoDB.",
	}, []string{"client_id"})

	packetsFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "packets_failed_total",
		Help: "Total packets that failed processing.",
	}, []string{"reason"})

	rabbitmqPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rabbitmq_publish_errors_total",
		Help: "Total failed RabbitMQ publish attempts.",
	})

	mongodbOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mongodb_op_duration_seconds",
		Help:    "MongoDB operation latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})
)

// Note: grpc_request_duration_seconds from the spec is intentionally omitted here.
// gRPC latency is captured by OTel (Task 7) and visible in Tempo — a separate
// Prometheus histogram would be redundant and require an additional dependency.
```

- [ ] **Step 3: Start `/metrics` HTTP server in `main()` in `order-service/main.go`**

Add this import: `"net/http"` and `"github.com/prometheus/client_golang/prometheus/promhttp"`.

Add this goroutine at the start of `main()`, after the slog init:

```go
go func() {
    http.Handle("/metrics", promhttp.Handler())
    if err := http.ListenAndServe(":9091", nil); err != nil {
        slog.Error("metrics server failed", "error", err)
        os.Exit(1)
    }
}()
```

- [ ] **Step 4: Instrument `SendPacket` with metrics**

In `SendPacket`, wrap the `InsertOne` call and increment counters:

```go
func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
    slog.Info("received packet", "payload", in.Payload, "client_id", in.ClientId)

    timer := prometheus.NewTimer(mongodbOpDuration.WithLabelValues("insert_one"))
    _, err := s.collection.InsertOne(ctx, in)
    timer.ObserveDuration()
    if err != nil {
        packetsFailedTotal.WithLabelValues("mongodb_error").Inc()
        return nil, err
    }
    packetsProcessedTotal.WithLabelValues(in.ClientId).Inc()

    // ... rest of function unchanged (marshal + rabbitmq publish) ...
```

Also increment `rabbitmqPublishErrorsTotal` where publish errors are logged:

```go
// Where pubErr is logged in SendPacket:
if pubErr := amqpChannel.PublishWithContext(...); pubErr != nil {
    slog.Warn("failed to publish to rabbitmq", "error", pubErr)
    rabbitmqPublishErrorsTotal.Inc()
}
```

- [ ] **Step 5: Build, start, verify metrics endpoint**

```bash
cd order-service && go build ./...
docker compose up order-service -d --build
curl http://localhost:9091/metrics | grep packets
```

Expected output includes:
```
# HELP packets_processed_total Total packets successfully persisted to MongoDB.
# TYPE packets_processed_total counter
# HELP packets_failed_total Total packets that failed processing.
```

Send a test packet via the frontend or grpcurl, then:
```bash
curl -s http://localhost:9091/metrics | grep packets_processed_total
```
Expected: counter value increments.

- [ ] **Step 6: Verify Prometheus scrapes order-service**

Open `http://localhost:9090/targets` — `order-service` target should show State=UP.

- [ ] **Step 7: Commit**

```bash
git add order-service/metrics.go order-service/main.go order-service/go.mod order-service/go.sum
git commit -m "feat(order-service): add Prometheus metrics endpoint and business counters"
```

---

## Task 6: Prometheus Metrics in inventory-service

**Files:**
- Create: `inventory-service/metrics.go`
- Modify: `inventory-service/main.go`

- [ ] **Step 1: Add Prometheus dependency**

```bash
cd inventory-service && go get github.com/prometheus/client_golang/prometheus
cd inventory-service && go get github.com/prometheus/client_golang/prometheus/promauto
cd inventory-service && go get github.com/prometheus/client_golang/prometheus/promhttp
```

- [ ] **Step 2: Create `inventory-service/metrics.go`**

```go
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	packetsConsumedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "packets_consumed_total",
		Help: "Total messages successfully consumed from RabbitMQ.",
	})

	packetsDecodeErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "packets_decode_errors_total",
		Help: "Total messages that failed JSON decode.",
	})

	rabbitmqConsumeLag = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "rabbitmq_consume_lag_seconds",
		Help:    "Time between message publish and consume in seconds.",
		Buckets: prometheus.DefBuckets,
	})
)
```

- [ ] **Step 3: Start `/metrics` HTTP server in `main()` in `inventory-service/main.go`**

Add imports: `"net/http"`, `"github.com/prometheus/client_golang/prometheus/promhttp"`.

Add this goroutine at the start of `main()`, after the slog init:

```go
go func() {
    http.Handle("/metrics", promhttp.Handler())
    if err := http.ListenAndServe(":9091", nil); err != nil {
        slog.Error("metrics server failed", "error", err)
        os.Exit(1)
    }
}()
```

- [ ] **Step 4: Instrument the consume loop**

The consume lag metric requires a `published_at` timestamp in the message body. Update `DataPacket` struct and the consume loop:

```go
type DataPacket struct {
    ClientId       string  `json:"client_id"`
    Payload        string  `json:"payload"`
    SequenceNumber int32   `json:"sequence_number"`
    PublishedAt    float64 `json:"published_at"` // Unix seconds with fractional part
}
```

In the consume loop, after successful decode:

```go
for d := range msgs {
    var packet DataPacket
    if err := json.Unmarshal(d.Body, &packet); err != nil {
        slog.Error("failed to decode message", "error", err)
        packetsDecodeErrorsTotal.Inc()
        d.Ack(false)
        continue
    }

    if packet.PublishedAt > 0 {
        lag := time.Since(time.Unix(0, int64(packet.PublishedAt*float64(time.Second))))
        rabbitmqConsumeLag.Observe(lag.Seconds())
    }
    packetsConsumedTotal.Inc()

    slog.Info("received packet",
        "client_id", packet.ClientId,
        "payload", packet.Payload,
        "sequence_number", packet.SequenceNumber)
    d.Ack(false)
}
```

- [ ] **Step 5: Update order-service to include `published_at` in messages**

In `order-service/main.go`, update the JSON marshal in `SendPacket` to include the publish timestamp:

```go
body, err := json.Marshal(map[string]interface{}{
    "client_id":       in.ClientId,
    "payload":         in.Payload,
    "sequence_number": in.SequenceNumber,
    "published_at":    float64(time.Now().UnixNano()) / float64(time.Second),
})
```

Apply the same change in `StressTest` where the message is marshalled.

- [ ] **Step 6: Build and verify**

```bash
cd inventory-service && go build ./...
cd order-service && go build ./...
docker compose up inventory-service order-service -d --build
curl http://localhost:9092/metrics | grep packets
```

Note: inventory-service maps to a different host port. Update compose to expose inventory-service metrics:

Add to `inventory-service` in `docker-compose.yml`:
```yaml
    ports:
      - "9092:9091"
```

Then:
```bash
docker compose up inventory-service -d --build
curl http://localhost:9092/metrics | grep packets_consumed_total
```

Open `http://localhost:9090/targets` — both `order-service` and `inventory-service` targets show State=UP.

- [ ] **Step 7: Commit**

```bash
git add inventory-service/metrics.go inventory-service/main.go inventory-service/go.mod inventory-service/go.sum
git add order-service/main.go docker-compose.yml
git commit -m "feat(inventory-service): add Prometheus metrics; add published_at to messages"
```

---

## Task 7: OTel Tracer + gRPC Interceptors in order-service

**Files:**
- Create: `order-service/telemetry.go`
- Modify: `order-service/main.go`

- [ ] **Step 1: Add OTel dependencies**

```bash
cd order-service
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/trace
go get go.opentelemetry.io/otel/sdk/trace
go get go.opentelemetry.io/otel/sdk/resource
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
go get go.opentelemetry.io/otel/propagation
go get go.opentelemetry.io/otel/semconv/v1.21.0
go get go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc
```

- [ ] **Step 2: Create `order-service/telemetry.go`**

```go
package main

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

func initTracer(ctx context.Context) (func(), error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "tempo:4317"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("order-service"),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() { tp.Shutdown(context.Background()) }, nil
}
```

- [ ] **Step 3: Initialize tracer in `main()` and add gRPC stats handler**

In `order-service/main.go`, add a package-level tracer variable at the top of the file (outside `main`):

```go
import "go.opentelemetry.io/otel/trace"

var tracer trace.Tracer
```

In `main()`, after the slog init and before connecting MongoDB:

```go
otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
defer otelCancel()
shutdown, err := initTracer(otelCtx)
if err != nil {
    slog.Warn("failed to init tracer, continuing without tracing", "error", err)
} else {
    defer shutdown()
}
tracer = otel.Tracer("order-service")
```

Add import: `"go.opentelemetry.io/otel"` and `"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"`.

Replace the `grpc.NewServer()` call:

```go
// Replace:
grpcServer := grpc.NewServer()
// With:
grpcServer := grpc.NewServer(
    grpc.StatsHandler(otelgrpc.NewServerHandler()),
)
```

- [ ] **Step 4: Build and verify**

```bash
cd order-service && go build ./...
docker compose up order-service -d --build
```

Send a packet via the frontend or:
```bash
# if grpcurl is available:
grpcurl -plaintext -d '{"client_id":"test","payload":"hello","sequence_number":1}' localhost:8080 orders.OrderService/SendPacket
```

Open `http://localhost:3000/explore` in Grafana, select Tempo datasource, click "Search" — a trace for `/orders.OrderService/SendPacket` should appear.

- [ ] **Step 5: Add MongoDB span in `SendPacket`**

Add imports: `"go.opentelemetry.io/otel/attribute"` and `"go.opentelemetry.io/otel/trace"` (the API package, not SDK).

In `SendPacket`, wrap the `InsertOne` call:

```go
ctx, mongoSpan := tracer.Start(ctx, "mongodb.insert_one",
    trace.WithAttributes(
        attribute.String("db.system", "mongodb"),
        attribute.String("db.operation", "insert_one"),
    ),
)
timer := prometheus.NewTimer(mongodbOpDuration.WithLabelValues("insert_one"))
_, err := s.collection.InsertOne(ctx, in)
timer.ObserveDuration()
mongoSpan.End()
if err != nil {
    mongoSpan.RecordError(err)
    packetsFailedTotal.WithLabelValues("mongodb_error").Inc()
    return nil, err
}
```

- [ ] **Step 6: Build and verify MongoDB span appears in trace**

```bash
cd order-service && go build ./...
docker compose up order-service -d --build
```

Send a packet, then open Grafana → Explore → Tempo → find the trace. It should now show two spans: the gRPC span and `mongodb.insert_one` as a child.

- [ ] **Step 7: Add `OTEL_EXPORTER_OTLP_ENDPOINT` to order-service .env**

Open `order-service/.env` and add:
```
OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4317
```

- [ ] **Step 8: Commit**

```bash
git add order-service/telemetry.go order-service/main.go order-service/.env order-service/go.mod order-service/go.sum
git commit -m "feat(order-service): add OTel tracing with gRPC interceptor and MongoDB span"
```

---

## Task 8: RabbitMQ Publish Span in order-service

**Files:**
- Create: `order-service/amqp_carrier.go`
- Modify: `order-service/main.go`

- [ ] **Step 1: Write the failing test for `amqpHeaderCarrier`**

Create `order-service/amqp_carrier_test.go`:

```go
package main

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestAmqpHeaderCarrier_SetAndGet(t *testing.T) {
	table := amqp.Table{}
	carrier := amqpHeaderCarrier(table)

	carrier.Set("traceparent", "00-abc-def-01")

	got := carrier.Get("traceparent")
	if got != "00-abc-def-01" {
		t.Errorf("Get() = %q, want %q", got, "00-abc-def-01")
	}
}

func TestAmqpHeaderCarrier_GetMissing(t *testing.T) {
	carrier := amqpHeaderCarrier(amqp.Table{})
	if got := carrier.Get("missing"); got != "" {
		t.Errorf("Get() for missing key = %q, want empty string", got)
	}
}

func TestAmqpHeaderCarrier_Keys(t *testing.T) {
	table := amqp.Table{"a": "1", "b": "2"}
	carrier := amqpHeaderCarrier(table)
	keys := carrier.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() returned %d keys, want 2", len(keys))
	}
}
```

- [ ] **Step 2: Run the test — verify it fails**

```bash
cd order-service && go test ./... -run TestAmqpHeaderCarrier -v
```

Expected: `FAIL` — `amqpHeaderCarrier` undefined.

- [ ] **Step 3: Create `order-service/amqp_carrier.go`**

```go
package main

import amqp "github.com/rabbitmq/amqp091-go"

// amqpHeaderCarrier adapts amqp.Table to the OTel TextMapCarrier interface,
// enabling W3C trace context propagation over AMQP message headers.
type amqpHeaderCarrier amqp.Table

func (c amqpHeaderCarrier) Get(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c amqpHeaderCarrier) Set(key, val string) {
	c[key] = val
}

func (c amqpHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
```

- [ ] **Step 4: Run the test — verify it passes**

```bash
cd order-service && go test ./... -run TestAmqpHeaderCarrier -v
```

Expected: `PASS`

- [ ] **Step 5: Add RabbitMQ publish span in `SendPacket`**

In `order-service/main.go`, replace the publish block in `SendPacket`:

```go
// After InsertOne succeeds, before publishing:
headers := amqp.Table{}
publishCtx, publishSpan := tracer.Start(ctx, "rabbitmq.publish",
    trace.WithAttributes(
        attribute.String("messaging.system", "rabbitmq"),
        attribute.String("messaging.destination", "packets"),
        attribute.String("messaging.destination_kind", "queue"),
    ),
)
otel.GetTextMapPropagator().Inject(publishCtx, amqpHeaderCarrier(headers))

amqpChannel, err := s.ensureChannel()
if err != nil {
    slog.Warn("rabbitmq unavailable", "error", err)
    publishSpan.RecordError(err)
    publishSpan.End()
    return &pb.Response{Message: "Packet persisted to MongoDB", Success: true}, nil
}

if pubErr := amqpChannel.PublishWithContext(publishCtx, "", "packets", false, false, amqp.Publishing{
    ContentType: "application/json",
    Body:        body,
    Headers:     headers,
}); pubErr != nil {
    slog.Warn("failed to publish to rabbitmq", "error", pubErr)
    publishSpan.RecordError(pubErr)
    rabbitmqPublishErrorsTotal.Inc()
}
publishSpan.End()
```

- [ ] **Step 6: Add a single parent span for `StressTest`**

`StressTest` sends N messages. Adding a span per message is too noisy — wrap the whole call instead:

```go
func (s *server) StressTest(ctx context.Context, in *pb.StressTestRequest) (*pb.Response, error) {
    ctx, span := tracer.Start(ctx, "stress_test",
        trace.WithAttributes(attribute.Int("stress_test.count", int(in.Count))),
    )
    defer span.End()
    // ... rest of function unchanged ...
```

- [ ] **Step 7: Build and verify full trace includes RabbitMQ span**

```bash
cd order-service && go build ./...
docker compose up order-service -d --build
```

Send a packet. In Grafana → Explore → Tempo, find the trace — it should now show 3 spans: gRPC → `mongodb.insert_one` → `rabbitmq.publish`.

- [ ] **Step 8: Commit**

```bash
git add order-service/amqp_carrier.go order-service/amqp_carrier_test.go order-service/main.go
git commit -m "feat(order-service): add RabbitMQ publish span with W3C context propagation"
```

---

## Task 9: OTel Tracer + RabbitMQ Consume Span in inventory-service

**Files:**
- Create: `inventory-service/telemetry.go`
- Create: `inventory-service/amqp_carrier.go`
- Modify: `inventory-service/main.go`

- [ ] **Step 1: Add OTel dependencies**

```bash
cd inventory-service
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/trace
go get go.opentelemetry.io/otel/sdk/trace
go get go.opentelemetry.io/otel/sdk/resource
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
go get go.opentelemetry.io/otel/propagation
go get go.opentelemetry.io/otel/semconv/v1.21.0
```

- [ ] **Step 2: Write the failing test for `amqpHeaderCarrier`**

Create `inventory-service/amqp_carrier_test.go`:

```go
package main

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestAmqpHeaderCarrier_SetAndGet(t *testing.T) {
	table := amqp.Table{}
	carrier := amqpHeaderCarrier(table)

	carrier.Set("traceparent", "00-abc-def-01")

	got := carrier.Get("traceparent")
	if got != "00-abc-def-01" {
		t.Errorf("Get() = %q, want %q", got, "00-abc-def-01")
	}
}

func TestAmqpHeaderCarrier_GetMissing(t *testing.T) {
	carrier := amqpHeaderCarrier(amqp.Table{})
	if got := carrier.Get("missing"); got != "" {
		t.Errorf("Get() for missing key = %q, want empty string", got)
	}
}

func TestAmqpHeaderCarrier_Keys(t *testing.T) {
	table := amqp.Table{"a": "1", "b": "2"}
	carrier := amqpHeaderCarrier(table)
	keys := carrier.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() returned %d keys, want 2", len(keys))
	}
}
```

- [ ] **Step 3: Run — verify fails**

```bash
cd inventory-service && go test ./... -run TestAmqpHeaderCarrier -v
```

Expected: `FAIL` — `amqpHeaderCarrier` undefined.

- [ ] **Step 4: Create `inventory-service/telemetry.go`**

Same as `order-service/telemetry.go` but with service name `"inventory-service"`:

```go
package main

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

func initTracer(ctx context.Context) (func(), error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "tempo:4317"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("inventory-service"),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() { tp.Shutdown(context.Background()) }, nil
}
```

- [ ] **Step 5: Create `inventory-service/amqp_carrier.go`**

```go
package main

import amqp "github.com/rabbitmq/amqp091-go"

// amqpHeaderCarrier adapts amqp.Table to the OTel TextMapCarrier interface,
// enabling W3C trace context propagation over AMQP message headers.
type amqpHeaderCarrier amqp.Table

func (c amqpHeaderCarrier) Get(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c amqpHeaderCarrier) Set(key, val string) {
	c[key] = val
}

func (c amqpHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
```

- [ ] **Step 6: Run — verify test passes**

```bash
cd inventory-service && go test ./... -run TestAmqpHeaderCarrier -v
```

Expected: `PASS`

- [ ] **Step 7: Wire OTel and consume span in `inventory-service/main.go`**

Add package-level tracer variable:

```go
import "go.opentelemetry.io/otel/trace"

var tracer trace.Tracer
```

In `main()`, after slog init:

```go
otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
defer otelCancel()
shutdown, err := initTracer(otelCtx)
if err != nil {
    slog.Warn("failed to init tracer, continuing without tracing", "error", err)
} else {
    defer shutdown()
}
tracer = otel.Tracer("inventory-service")
```

Add imports: `"go.opentelemetry.io/otel"`, `"go.opentelemetry.io/otel/attribute"`, `"go.opentelemetry.io/otel/trace"` (API), `"context"`.

Replace the consume loop body:

```go
for d := range msgs {
    // Extract trace context from message headers to continue the same trace
    ctx := otel.GetTextMapPropagator().Extract(
        context.Background(),
        amqpHeaderCarrier(d.Headers),
    )
    ctx, span := tracer.Start(ctx, "rabbitmq.consume",
        trace.WithAttributes(
            attribute.String("messaging.system", "rabbitmq"),
            attribute.String("messaging.destination", "packets"),
            attribute.String("messaging.destination_kind", "queue"),
        ),
    )

    var packet DataPacket
    if err := json.Unmarshal(d.Body, &packet); err != nil {
        slog.Error("failed to decode message", "error", err)
        packetsDecodeErrorsTotal.Inc()
        span.RecordError(err)
        span.End()
        d.Ack(false)
        continue
    }

    if packet.PublishedAt > 0 {
        lag := time.Since(time.Unix(0, int64(packet.PublishedAt*float64(time.Second))))
        rabbitmqConsumeLag.Observe(lag.Seconds())
    }
    packetsConsumedTotal.Inc()

    slog.Info("received packet",
        "client_id", packet.ClientId,
        "payload", packet.Payload,
        "sequence_number", packet.SequenceNumber)
    span.End()
    d.Ack(false)
}
```

Add `OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4317` to `inventory-service/.env`.

- [ ] **Step 8: Build and verify end-to-end trace**

```bash
cd inventory-service && go build ./...
docker compose up inventory-service order-service -d --build
```

Send a packet. In Grafana → Explore → Tempo, find the trace — it should now show 4 spans across 2 services:
1. `orders.OrderService/SendPacket` (order-service)
2. `mongodb.insert_one` (order-service)
3. `rabbitmq.publish` (order-service)
4. `rabbitmq.consume` (inventory-service)

- [ ] **Step 9: Commit**

```bash
git add inventory-service/telemetry.go inventory-service/amqp_carrier.go inventory-service/amqp_carrier_test.go
git add inventory-service/main.go inventory-service/.env inventory-service/go.mod inventory-service/go.sum
git commit -m "feat(inventory-service): add OTel tracing with RabbitMQ consume span"
```

---

## Task 10: Trace-Log Correlation

**Files:**
- Create: `order-service/log_trace.go`
- Create: `inventory-service/log_trace.go`
- Modify: `order-service/main.go`
- Modify: `inventory-service/main.go`

This injects `trace_id` and `span_id` into slog records so Grafana can link a Tempo trace span directly to the correlated log lines.

- [ ] **Step 1: Write failing test for `logWithTrace` in order-service**

Create `order-service/log_trace_test.go`:

```go
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
```

- [ ] **Step 2: Run — verify fails**

```bash
cd order-service && go test ./... -run TestLogWithTrace -v
```

Expected: `FAIL` — `logWithTrace` undefined.

- [ ] **Step 3: Create `order-service/log_trace.go`**

```go
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
```

- [ ] **Step 4: Run — verify passes**

```bash
cd order-service && go test ./... -run TestLogWithTrace -v
```

Expected: `PASS`

- [ ] **Step 5: Update `SendPacket` in `order-service/main.go` to use `logWithTrace`**

Replace the slog calls that have a `ctx` parameter:

```go
func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
    log := logWithTrace(ctx, slog.Default())
    log.Info("received packet", "payload", in.Payload, "client_id", in.ClientId)
    // ... use log.Warn, log.Error for all subsequent calls in this function ...
```

Do the same in `StressTest` and `StreamDisturbance` — replace `slog.Info(...)` / `slog.Warn(...)` with `log.Info(...)` / `log.Warn(...)` where `log := logWithTrace(ctx, slog.Default())`.

- [ ] **Step 6: Create `inventory-service/log_trace_test.go`**

Identical to `order-service/log_trace_test.go` — same package, same test code.

- [ ] **Step 7: Run — verify fails**

```bash
cd inventory-service && go test ./... -run TestLogWithTrace -v
```

Expected: `FAIL` — `logWithTrace` undefined.

- [ ] **Step 8: Create `inventory-service/log_trace.go`**

Identical content to `order-service/log_trace.go`.

- [ ] **Step 9: Run — verify passes**

```bash
cd inventory-service && go test ./... -run TestLogWithTrace -v
```

Expected: `PASS`

- [ ] **Step 10: Update consume loop in `inventory-service/main.go` to use `logWithTrace`**

```go
// In the consume loop, after span is started:
log := logWithTrace(ctx, slog.Default())
// ...
log.Error("failed to decode message", "error", err)
// ...
log.Info("received packet",
    "client_id", packet.ClientId,
    "payload", packet.Payload,
    "sequence_number", packet.SequenceNumber)
```

- [ ] **Step 11: Build both services**

```bash
cd order-service && go build ./...
cd inventory-service && go build ./...
```

Expected: no errors.

- [ ] **Step 12: Deploy and verify trace↔log correlation in Grafana**

```bash
docker compose up order-service inventory-service -d --build
```

1. Send a packet to trigger a trace.
2. Open Grafana → Explore → Tempo → find the trace.
3. Click the `rabbitmq.publish` span → click "Logs for this span" (or the Loki link).
4. Grafana should open the Loki explorer filtered to log lines matching that `trace_id`.
5. Verify the log lines from order-service appear with the `trace_id` field matching the Tempo trace ID.

- [ ] **Step 13: Run all tests**

```bash
cd order-service && go test ./... -v
cd inventory-service && go test ./... -v
```

Expected: all pass.

- [ ] **Step 14: Commit**

```bash
git add order-service/log_trace.go order-service/log_trace_test.go order-service/main.go
git add inventory-service/log_trace.go inventory-service/log_trace_test.go inventory-service/main.go
git commit -m "feat: inject trace_id/span_id into slog for Grafana trace-log correlation"
```

---

## Done

Full observability stack running. Verify end state:

| Check | How |
|-------|-----|
| Logs in Loki | Grafana → Explore → Loki → `{container=~"order-service\|inventory-service"}` |
| Metrics in Prometheus | `http://localhost:9090/targets` — all 3 targets UP |
| Traces in Tempo | Grafana → Explore → Tempo → Search → find gRPC traces |
| Cross-pillar correlation | Click trace span → "Logs for this span" → see matching log lines |
| Business metrics | `http://localhost:9090/graph?g0.expr=rate(packets_processed_total[1m])` |
| Starter dashboard | Grafana → Dashboards → ProjectCrash → ProjectCrash Overview |
