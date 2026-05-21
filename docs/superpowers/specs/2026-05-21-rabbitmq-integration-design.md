# RabbitMQ Integration Design

**Date:** 2026-05-21  
**Status:** Approved

## Summary

Wire RabbitMQ into the existing microservices stack. `order-service` publishes a message to a direct queue on every `SendPacket` call. A new `inventory-service` consumes from that queue and logs received packets. Demo/learning scope — proves the async messaging pipeline end to end.

## Architecture

```
Frontend (React)
    │  gRPC-web (Connect)
    ▼
Envoy proxy :8080
    │  gRPC
    ▼
order-service :50051
    │  MongoDB insert (existing)
    │
    │  AMQP publish → queue: "packets"
    ▼
RabbitMQ :5672
    │
    ▼
inventory-service
    │  log: received packet {client_id, payload, sequence_number}
```

## Components

### order-service (modified)

- Connect to RabbitMQ (`amqp://rabbitmq:5672`) at startup
- Declare queue `packets` (durable: false, for demo simplicity)
- Inject `*amqp.Channel` into `server` struct alongside existing `*mongo.Collection`
- In `SendPacket`: after successful MongoDB insert, marshal `DataPacket` to JSON and publish to queue `packets`
- Publish failure logs `WARN` but does **not** fail the RPC — MongoDB write already succeeded
- New dependency: `github.com/rabbitmq/amqp091-go`

### inventory-service (new)

- `inventory-service/main.go`: standalone Go binary
- Connect to RabbitMQ at startup with retry loop (max ~30s, ~1s intervals)
- Declare queue `packets` (same parameters as publisher)
- Start consumer goroutine: for each message, unmarshal JSON to `DataPacket`, log fields, ack
- Consumer decode error: log `ERROR`, ack anyway (prevent infinite requeue of corrupt messages)
- No gRPC, no MongoDB
- Own `Dockerfile`, added to `docker-compose.yml`

### docker-compose.yml (modified)

- Add `healthcheck` to `rabbitmq` service so dependent services wait for broker readiness
- Uncomment and configure `inventory-service` block with `depends_on: rabbitmq`

## Data Flow (SendPacket)

1. Frontend sends gRPC `SendPacket(DataPacket)`
2. `order-service` inserts packet into MongoDB
3. `order-service` publishes `DataPacket` as JSON to queue `packets`
4. `order-service` returns `Response{success: true}` to caller
5. `inventory-service` consumer goroutine receives delivery, logs packet, acks

Steps 3 and 4 are independent — publish failure does not block the response.

## Error Handling

| Scenario | Behavior |
|---|---|
| RabbitMQ unavailable at startup | Retry with 1s sleep, up to 30 attempts; container restart handles beyond that |
| Publish failure in order-service | Log WARN, return gRPC success (MongoDB write is source of truth) |
| Consumer decode error | Log ERROR, ack message (no infinite requeue) |
| RabbitMQ connection drop mid-run | Out of scope — Docker restart policy handles recovery |

## Shared Types

`inventory-service` re-declares `DataPacket` as a local struct for JSON unmarshalling — no shared proto dependency needed for demo scope.

```go
type DataPacket struct {
    ClientId       string `json:"client_id"`
    Payload        string `json:"payload"`
    SequenceNumber int32  `json:"sequence_number"`
}
```

## Verification

1. `docker compose up`
2. Send packet from frontend
3. Check `inventory-service` container logs — expect line with received `client_id`, `payload`, `sequence_number`
4. Optional: RabbitMQ management UI at `localhost:15672` (guest/guest) — confirm queue `packets` exists and message was delivered

## Out of Scope

- StressTest publishing to RabbitMQ
- Persistent/durable queues
- Dead-letter queues
- inventory-service gRPC API or database
- Connection reconnect logic
