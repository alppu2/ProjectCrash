# Horizontal Scaling + Load Testing Design

## Goal

Scale `order-service` to multiple replicas behind Envoy, verify load distributes evenly across replicas via Grafana, fire load with ghz.

## Changes

### docker-compose.yml

- Remove `container_name: order-service` — Docker requires unique container names; fixed name blocks `--scale`.
- Mount Docker socket into Prometheus: `- /var/run/docker.sock:/var/run/docker.sock`

### envoy/envoy.yaml

Change cluster discovery type:

```yaml
type: STRICT_DNS  # was: logical_dns
```

`logical_dns` resolves DNS once on startup, caches one IP — talks to one replica forever.
`strict_dns` re-resolves on interval, discovers all replica IPs, applies `round_robin` across all.

### prometheus/prometheus.yml

Replace static `order-service:9091` target with Docker service discovery:

```yaml
- job_name: order-service
  docker_sd_configs:
    - host: unix:///var/run/docker.sock
  relabel_configs:
    - source_labels: [__meta_docker_container_name]
      regex: /order-service.*
      action: keep
    - source_labels: [__address__]
      regex: (.+):\d+
      replacement: $1:9091
      target_label: __address__
    - source_labels: [__meta_docker_container_name]
      target_label: instance
```

Prometheus auto-discovers all `order-service-*` containers. Sets `instance` label per container — used to split metrics per replica in Grafana.

> **Note:** Docker socket mount is a dev-only pattern. In Kubernetes (step 3), replace with `kubernetes_sd_configs` + RBAC.

### grafana/dashboards/overview.json

**Add panel 5 — Requests per Replica:**

```json
{
  "expr": "rate(packets_processed_total[1m])",
  "legendFormat": "{{instance}}"
}
```

Each replica becomes its own line. Shows load distribution live during ghz run.

**Fix panel 4 (Service Logs) Loki query:**

```
{container=~"order-service.*|inventory-service"}
```

Scaled containers are named `order-service-1`, `order-service-2`, etc. — old exact match misses them.

## Load Testing

Install ghz (single binary): https://github.com/bojand/ghz/releases

Scale up and run:

```bash
# Scale to 3 replicas
docker compose up --scale order-service=3 -d

# Fire load
ghz --insecure \
    --proto ./proto/service.proto \
    --call orders.OrderService/StressTest \
    --data '{"client_id":"loadtest","payload":"hello","count":10}' \
    --rps 50 \
    --duration 60s \
    localhost:8080
```

Expected: ~17 RPS per replica on Grafana "Requests per Replica" panel. ghz terminal shows p50/p95/p99 latency and total RPS.

## Success Criteria

- `docker compose up --scale order-service=3` starts without error
- Envoy distributes requests across all 3 replicas (visible in Grafana)
- Prometheus scrapes metrics from each replica separately (3 `instance` labels)
- ghz completes 60s run with <1% errors
