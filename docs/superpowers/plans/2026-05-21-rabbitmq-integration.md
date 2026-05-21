# RabbitMQ Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire `order-service` to publish `DataPacket` events to RabbitMQ on every `SendPacket` call, and create a new `inventory-service` that consumes and logs those events.

**Architecture:** `order-service` connects to RabbitMQ at startup, declares a direct queue named `packets`, and publishes JSON-encoded `DataPacket` after each successful MongoDB insert. `inventory-service` is a new standalone Go binary that connects to the same queue, consumes deliveries, and logs each packet's fields. Both services retry the RabbitMQ connection for up to 30 seconds at startup.

**Tech Stack:** Go 1.26, `github.com/rabbitmq/amqp091-go`, RabbitMQ 3 (already in Docker Compose), Docker multi-stage builds.

> **Note:** No unit tests — demo/learning scope. Verification is done via `docker compose up` and inspecting logs.

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Modify | `order-service/main.go` | Add RabbitMQ connection, inject channel into server, publish on SendPacket |
| Modify | `order-service/go.mod` | Add `github.com/rabbitmq/amqp091-go` dependency |
| Create | `inventory-service/go.mod` | Go module definition for new service |
| Create | `inventory-service/main.go` | AMQP consumer — connect, consume queue `packets`, log messages |
| Create | `inventory-service/Dockerfile` | Multi-stage build for inventory-service container |
| Modify | `docker-compose.yml` | Add healthcheck to rabbitmq, add inventory-service, update order-service depends_on |

---

## Task 1: Add amqp091-go dependency to order-service

**Files:**
- Modify: `order-service/go.mod`

- [ ] **Step 1: Add the dependency**

Run from the project root:
```bash
cd order-service && go get github.com/rabbitmq/amqp091-go
```

Expected output: something like `go: added github.com/rabbitmq/amqp091-go v1.10.0`

- [ ] **Step 2: Verify go.mod updated**

`order-service/go.mod` should now contain a `require` block with `github.com/rabbitmq/amqp091-go`. A `go.sum` file will also be created or updated in `order-service/`.

- [ ] **Step 3: Commit**

```bash
cd ..
git add order-service/go.mod order-service/go.sum
git commit -m "chore(order-service): add amqp091-go dependency"
```

---

## Task 2: Update order-service to publish packets to RabbitMQ

**Files:**
- Modify: `order-service/main.go`

- [ ] **Step 1: Replace order-service/main.go with the following**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"

	pb "order-service/orders"
)

type server struct {
	pb.UnimplementedOrderServiceServer
	db *mongo.Collection
	ch *amqp.Channel
}

func connectRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	var conn *amqp.Connection
	var err error
	for i := 0; i < 30; i++ {
		conn, err = amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
		if err == nil {
			break
		}
		log.Printf("RabbitMQ not ready, retrying (%d/30)...", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to RabbitMQ after 30 attempts: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to open channel: %w", err)
	}

	_, err = ch.QueueDeclare("packets", false, false, false, false, nil)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("failed to declare queue: %w", err)
	}

	return conn, ch, nil
}

func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
	log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)

	_, err := s.db.InsertOne(ctx, in)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]interface{}{
		"client_id":       in.ClientId,
		"payload":         in.Payload,
		"sequence_number": in.SequenceNumber,
	})
	if err != nil {
		log.Printf("WARN: failed to marshal packet for RabbitMQ: %v", err)
	} else {
		if pubErr := s.ch.PublishWithContext(ctx, "", "packets", false, false, amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		}); pubErr != nil {
			log.Printf("WARN: failed to publish to RabbitMQ: %v", pubErr)
		}
	}

	return &pb.Response{
		Message: "Packet persisted to MongoDB",
		Success: true,
	}, nil
}

func (s *server) StressTest(ctx context.Context, in *pb.StressTestRequest) (*pb.Response, error) {
	startTime := time.Now()
	for i := int32(0); i < in.Count; i++ {
		packet := &pb.DataPacket{
			ClientId:       in.ClientId,
			Payload:        in.Payload,
			SequenceNumber: i,
		}
		if _, err := s.db.InsertOne(ctx, packet); err != nil {
			return nil, err
		}
		if i%100 == 0 {
			log.Printf("Stress test: processed %d / %d packets", i, in.Count)
		}
	}
	return &pb.Response{
		Message: fmt.Sprintf("%d packets processed in %s", in.Count, time.Since(startTime)),
		Success: true,
	}, nil
}

func (s *server) StreamDisturbance(stream pb.OrderService_StreamDisturbanceServer) error {
	var packetCount int32
	startTime := time.Now()

	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.Response{
				Message: string(packetCount) + " packets processed in " + time.Since(startTime).String(),
				Success: true,
			})
		}
		if err != nil {
			return err
		}

		packetCount++
		if packetCount%100 == 0 {
			log.Printf("Stress test: Received %d packets so far...", packetCount)
		}
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://mongodb:27017"))
	if err != nil {
		log.Fatal(err)
	}
	collection := client.Database("order_db").Collection("packets")

	_, ch, err := connectRabbitMQ()
	if err != nil {
		log.Fatalf("RabbitMQ setup failed: %v", err)
	}

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterOrderServiceServer(s, &server{db: collection, ch: ch})

	log.Println("Order Service (gRPC) listening on :50051")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
```

- [ ] **Step 2: Commit**

```bash
git add order-service/main.go
git commit -m "feat(order-service): publish DataPacket to RabbitMQ queue on SendPacket"
```

---

## Task 3: Create inventory-service Go module and main.go

**Files:**
- Create: `inventory-service/go.mod`
- Create: `inventory-service/main.go`

- [ ] **Step 1: Initialize the Go module**

```bash
mkdir inventory-service
cd inventory-service
go mod init inventory-service
go get github.com/rabbitmq/amqp091-go
cd ..
```

Expected: `inventory-service/go.mod` and `inventory-service/go.sum` created.

- [ ] **Step 2: Create inventory-service/main.go**

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type DataPacket struct {
	ClientId       string `json:"client_id"`
	Payload        string `json:"payload"`
	SequenceNumber int32  `json:"sequence_number"`
}

func connectRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	var conn *amqp.Connection
	var err error
	for i := 0; i < 30; i++ {
		conn, err = amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
		if err == nil {
			break
		}
		log.Printf("RabbitMQ not ready, retrying (%d/30)...", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect after 30 attempts: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to open channel: %w", err)
	}

	_, err = ch.QueueDeclare("packets", false, false, false, false, nil)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("failed to declare queue: %w", err)
	}

	return conn, ch, nil
}

func main() {
	_, ch, err := connectRabbitMQ()
	if err != nil {
		log.Fatalf("RabbitMQ setup failed: %v", err)
	}

	msgs, err := ch.Consume("packets", "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("failed to register consumer: %v", err)
	}

	log.Println("Inventory Service listening for packets...")

	for d := range msgs {
		var packet DataPacket
		if err := json.Unmarshal(d.Body, &packet); err != nil {
			log.Printf("ERROR: failed to decode message: %v", err)
			d.Ack(false)
			continue
		}
		log.Printf("Received packet — client_id: %s, payload: %s, sequence_number: %d",
			packet.ClientId, packet.Payload, packet.SequenceNumber)
		d.Ack(false)
	}
}
```

- [ ] **Step 3: Commit**

```bash
git add inventory-service/go.mod inventory-service/go.sum inventory-service/main.go
git commit -m "feat(inventory-service): add RabbitMQ consumer service"
```

---

## Task 4: Create inventory-service Dockerfile

**Files:**
- Create: `inventory-service/Dockerfile`

- [ ] **Step 1: Create inventory-service/Dockerfile**

```dockerfile
FROM golang:1.26 AS builder
WORKDIR /app

COPY inventory-service/go.mod .
COPY inventory-service/main.go .

RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o /inventory-service .

FROM alpine:latest
COPY --from=builder /inventory-service /inventory-service
CMD ["/inventory-service"]
```

Note: build context is the project root (`.`) — same as `order-service`. The `docker-compose.yml` sets this via `context: .`.

- [ ] **Step 2: Commit**

```bash
git add inventory-service/Dockerfile
git commit -m "chore(inventory-service): add Dockerfile"
```

---

## Task 5: Update docker-compose.yml

**Files:**
- Modify: `docker-compose.yml`

- [ ] **Step 1: Replace docker-compose.yml with the following**

```yaml
services:
  order-service:
    build:
      context: .
      dockerfile: order-service/Dockerfile
    container_name: order-service
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

  inventory-service:
    build:
      context: .
      dockerfile: inventory-service/Dockerfile
    container_name: inventory-service
    networks:
      - micro-network
    depends_on:
      rabbitmq:
        condition: service_healthy

  mongodb:
    image: mongo:latest
    container_name: mongodb
    ports:
      - "27017:27017"
    networks:
      - micro-network
    volumes:
      - mongo-data:/data/db

  rabbitmq:
    image: rabbitmq:3-management-alpine
    container_name: rabbitmq
    ports:
      - "5672:5672"
      - "15672:15672"
    networks:
      - micro-network
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5

  envoy:
    image: envoyproxy/envoy:v1.25-latest
    container_name: envoy-proxy
    volumes:
      - ./envoy/envoy.yaml:/etc/envoy/envoy.yaml
    ports:
      - "8080:8080"
    networks:
      - micro-network
    depends_on:
      - order-service

networks:
  micro-network:
    driver: bridge

volumes:
  mongo-data:
```

Key changes from original:
- `rabbitmq` gets a `healthcheck` using `rabbitmq-diagnostics ping`
- `order-service` `depends_on` upgraded to use `condition: service_healthy` for rabbitmq
- `inventory-service` block added (was commented out and references wrong service)

- [ ] **Step 2: Commit**

```bash
git add docker-compose.yml
git commit -m "feat: add inventory-service to compose, add rabbitmq healthcheck"
```

---

## Task 6: End-to-end verification

No automated tests. Verify the pipeline manually.

- [ ] **Step 1: Build and start all services**

```bash
docker compose up --build
```

Wait until you see all of:
- `order-service` → `Order Service (gRPC) listening on :50051`
- `inventory-service` → `Inventory Service listening for packets...`
- `rabbitmq` passes healthcheck (no more "retrying" logs from other services)

- [ ] **Step 2: Send a packet from the frontend**

Open `http://localhost:8080` in a browser. Fill in Client ID, Payload, Sequence Number and click Send Packet.

- [ ] **Step 3: Verify inventory-service received the message**

In the terminal running `docker compose up`, look for a line from `inventory-service` like:

```
inventory-service  | Received packet — client_id: client-001, payload: hello world, sequence_number: 0
```

- [ ] **Step 4: (Optional) Verify via RabbitMQ management UI**

Open `http://localhost:15672` (user: `guest`, password: `guest`). Navigate to Queues — queue `packets` should exist and show message delivery stats.

- [ ] **Step 5: Commit verification note (optional)**

If everything works, no additional code changes needed. Pipeline is complete.
