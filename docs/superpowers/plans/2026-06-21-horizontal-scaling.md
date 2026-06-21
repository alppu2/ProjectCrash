# Horizontal Scaling + Load Testing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scale `order-service` to multiple replicas behind Envoy, verify even load distribution per-replica in Grafana, validate with ghz load tool.

**Architecture:** Four config-only changes unblock `--scale`: remove the fixed container name (Docker blocks duplicates), switch Envoy to `STRICT_DNS` (re-resolves DNS each interval, discovers all replica IPs), replace Prometheus static scrape with Docker socket service discovery (auto-scrapes every replica), and update the Grafana dashboard to show per-replica traffic and match scaled container names in Loki.

**Tech Stack:** Docker Compose `--scale`, Envoy `STRICT_DNS`, Prometheus `docker_sd_configs`, Grafana dashboard JSON, ghz load tester

**Spec:** `docs/superpowers/specs/2026-06-15-horizontal-scaling-design.md`

---

## File Map

| File | Action | What changes |
|------|--------|--------------|
| `docker-compose.yml` | Modify | Remove `container_name: order-service`; mount Docker socket into Prometheus |
| `envoy/envoy.yaml` | Modify | `type: logical_dns` → `type: STRICT_DNS` |
| `prometheus/prometheus.yml` | Modify | Replace static `order-service:9091` with `docker_sd_configs` |
| `grafana/dashboards/overview.json` | Modify | Add panel 5 (per-replica rate); fix panel 4 Loki regex |

---

## Task 1: Remove Fixed Container Name + Add Docker Socket to Prometheus

**Files:**
- Modify: `docker-compose.yml`

`container_name: order-service` makes every replica try to claim the same name — Docker rejects all but the first. Remove it. Prometheus also needs the Docker socket to discover dynamically-named containers.

- [ ] **Step 1: Remove `container_name` from `order-service` in `docker-compose.yml`**

Delete line 6:
```yaml
    container_name: order-service   # DELETE THIS LINE
```

The `order-service` block should look like:

```yaml
  order-service:
    build:
      context: .
      dockerfile: order-service/Dockerfile
    env_file:
      - order-service/.env
    networks:
      - micro-network
    depends_on:
      mongodb:
        condition: service_started
      rabbitmq:
        condition: service_healthy
    develop:
      watch:
        - action: rebuild
          path: ./order-service
        - action: rebuild
          path: ./orders
```

- [ ] **Step 2: Add Docker socket mount to Prometheus in `docker-compose.yml`**

The current `prometheus` block has only one volume. Add the socket mount:

```yaml
  prometheus:
    image: prom/prometheus:latest
    container_name: prometheus
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml
      - /var/run/docker.sock:/var/run/docker.sock
    ports:
      - "9090:9090"
    networks:
      - micro-network
```

- [ ] **Step 3: Verify compose parses correctly**

```bash
docker compose config --quiet
```

Expected: no output (no errors). If errors appear, check YAML indentation.

- [ ] **Step 4: Commit**

```bash
git add docker-compose.yml
git commit -m "feat(compose): remove order-service container_name to allow --scale; add docker socket to prometheus"
```

---

## Task 2: Switch Envoy to STRICT_DNS

**Files:**
- Modify: `envoy/envoy.yaml`

`logical_dns` resolves `order-service` once on startup, caches that single IP. With multiple replicas, all traffic hits one container. `STRICT_DNS` re-resolves on interval and builds an endpoint set from every IP in the DNS response — one per replica.

- [ ] **Step 1: Change cluster type in `envoy/envoy.yaml`**

Find line 48:
```yaml
      type: logical_dns
```

Replace with:
```yaml
      type: STRICT_DNS
```

Full cluster block for reference (only `type` changes):

```yaml
  clusters:
    - name: order_service_cluster
      connect_timeout: 0.25s
      type: STRICT_DNS
      http2_protocol_options: {}
      lb_policy: round_robin
      load_assignment:
        cluster_name: order_service_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: order-service
                      port_value: 50051
```

- [ ] **Step 2: Commit**

```bash
git add envoy/envoy.yaml
git commit -m "feat(envoy): switch cluster to STRICT_DNS for multi-replica discovery"
```

---

## Task 3: Prometheus Docker Service Discovery

**Files:**
- Modify: `prometheus/prometheus.yml`

Static config `order-service:9091` resolves to one container. With `--scale`, containers are named `projectcrash-order-service-1`, `projectcrash-order-service-2`, etc. Docker SD auto-discovers all of them.

- [ ] **Step 1: Replace static order-service scrape config**

Replace the entire `order-service` job in `prometheus/prometheus.yml`. Full file after change:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: order-service
    docker_sd_configs:
      - host: unix:///var/run/docker.sock
    relabel_configs:
      - source_labels: [__meta_docker_container_name]
        regex: /.*order-service.*
        action: keep
      - source_labels: [__address__]
        regex: (.+):\d+
        replacement: $1:9091
        target_label: __address__
      - source_labels: [__meta_docker_container_name]
        target_label: instance

  - job_name: inventory-service
    static_configs:
      - targets: ['inventory-service:9091']

  - job_name: rabbitmq
    static_configs:
      - targets: ['rabbitmq:15692']
```

- [ ] **Step 2: Commit**

```bash
git add prometheus/prometheus.yml
git commit -m "feat(prometheus): replace static order-service scrape with docker_sd_configs"
```

---

## Task 4: Update Grafana Dashboard

**Files:**
- Modify: `grafana/dashboards/overview.json`

Two changes:
1. Panel 4 (Service Logs) uses `container=~"order-service|inventory-service"` — exact match misses `order-service-1`, `order-service-2`. Fix to `order-service.*`.
2. Add panel 5 "Requests per Replica" — shows one line per replica so load distribution is visible during ghz run.

- [ ] **Step 1: Fix panel 4 Loki query**

In `grafana/dashboards/overview.json`, find panel with `"id": 4`. Change its `expr`:

```json
        {
          "expr": "{container=~\"order-service.*|inventory-service\"}",
          "refId": "A"
        }
```

- [ ] **Step 2: Add panel 5 — Requests per Replica**

In the `"panels"` array, after the closing `}` of panel 4, add:

```json
    ,
    {
      "id": 5,
      "title": "Requests per Replica",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 24, "x": 0, "y": 16},
      "datasource": {"type": "prometheus", "uid": "prometheus"},
      "targets": [
        {
          "expr": "rate(packets_processed_total[1m])",
          "legendFormat": "{{instance}}",
          "refId": "A"
        }
      ]
    }
```

- [ ] **Step 3: Bump dashboard version**

Change `"version": 1` to `"version": 2` so Grafana reloads the dashboard from disk.

- [ ] **Step 4: Commit**

```bash
git add grafana/dashboards/overview.json
git commit -m "feat(grafana): fix log query for scaled containers; add per-replica requests panel"
```

---

## Task 5: Scale Up and Verify

**No file changes** — this task runs and verifies everything works.

- [ ] **Step 1: Rebuild and start with 3 order-service replicas**

```bash
docker compose up --scale order-service=3 -d --build
```

Expected: Docker creates `projectcrash-order-service-1`, `projectcrash-order-service-2`, `projectcrash-order-service-3` (or similar naming). No errors.

- [ ] **Step 2: Confirm all 3 replicas running**

```bash
docker compose ps
```

Expected: 3 rows with `order-service` in the name, all `running`.

- [ ] **Step 3: Verify Prometheus discovers all replicas**

Open `http://localhost:9090/targets`

Expected: `order-service` job shows 3 targets, all State=UP. Each has a different `instance` label (the container name).

If targets show State=DOWN or only 1 target appears, check:
- Docker socket is mounted: `docker inspect prometheus | grep docker.sock`
- Container names match regex: run `docker ps --format '{{.Names}}'` and verify names contain `order-service`

- [ ] **Step 4: Restart Grafana to pick up dashboard changes**

```bash
docker compose restart grafana
```

Open `http://localhost:3000/dashboards` → ProjectCrash → ProjectCrash Overview.

Verify: Panel 5 "Requests per Replica" exists. Panel 4 "Service Logs" shows logs from all replicas (not just one).

- [ ] **Step 5: Fire load with ghz**

Install ghz from https://github.com/bojand/ghz/releases (single binary, put in PATH).

```bash
ghz --insecure \
    --proto ./proto/service.proto \
    --call orders.OrderService/StressTest \
    --data '{"client_id":"loadtest","payload":"hello","count":10}' \
    --rps 50 \
    --duration 60s \
    localhost:8080
```

Expected ghz terminal output: summary showing ~50 RPS total, p50/p95/p99 latencies, <1% errors.

- [ ] **Step 6: Verify load distribution in Grafana**

While ghz runs (or immediately after), open `http://localhost:3000/dashboards` → ProjectCrash Overview → Panel 5 "Requests per Replica".

Expected: 3 lines, each showing ~17 RPS (50 RPS / 3 replicas). If one line shows all traffic, Envoy `STRICT_DNS` change didn't take effect — restart Envoy: `docker compose restart envoy`.

- [ ] **Step 7: Scale back down**

```bash
docker compose up --scale order-service=1 -d
```

Confirm single replica running: `docker compose ps`

---

## Done

| Check | How |
|-------|-----|
| `--scale` works | `docker compose up --scale order-service=3 -d` → 3 containers start |
| Envoy distributes | Grafana panel 5 shows ~equal RPS per replica during ghz |
| Prometheus scrapes all | `http://localhost:9090/targets` shows 3 order-service targets |
| Logs from all replicas | Grafana panel 4 shows logs from all 3 containers |
| ghz <1% errors | ghz summary `Status code distribution` shows `OK: 100%` |
