package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer

type DataPacket struct {
	ClientId       string  `json:"client_id"`
	Payload        string  `json:"payload"`
	SequenceNumber int32   `json:"sequence_number"`
	PublishedAt    float64 `json:"published_at"`
}

func connectRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	var conn *amqp.Connection
	var err error
	for i := 0; i < 30; i++ {
		conn, err = amqp.Dial(os.Getenv("RABBITMQ_URL"))
		if err == nil {
			break
		}
		slog.Info("rabbitmq not ready, retrying", "attempt", i+1, "max", 30)
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9091", nil); err != nil {
			slog.Error("metrics server failed", "error", err)
			os.Exit(1)
		}
	}()

	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	shutdown, err := initTracer(otelCtx)
	if err != nil {
		slog.Warn("failed to init tracer, continuing without tracing", "error", err)
	} else {
		defer shutdown()
	}
	tracer = otel.Tracer("inventory-service")

	conn, ch, err := connectRabbitMQ()
	if err != nil {
		slog.Error("rabbitmq setup failed", "error", err)
		os.Exit(1)
	}
	defer conn.Close()
	defer ch.Close()

	msgs, err := ch.Consume("packets", "", false, false, false, false, nil)
	if err != nil {
		slog.Error("failed to register consumer", "error", err)
		os.Exit(1)
	}

	slog.Info("inventory service listening for packets")

	for d := range msgs {
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
		log := logWithTrace(ctx, slog.Default())

		var packet DataPacket
		if err := json.Unmarshal(d.Body, &packet); err != nil {
			log.Error("failed to decode message", "error", err)
			packetsDecodeErrorsTotal.Inc()
			span.RecordError(err)
			span.End()
			d.Nack(false, false)
			continue
		}

		if packet.PublishedAt > 0 {
			lag := time.Since(time.Unix(0, int64(packet.PublishedAt*float64(time.Second))))
			rabbitmqConsumeLag.Observe(lag.Seconds())
		}
		packetsConsumedTotal.Inc()

		log.Info("received packet",
			"client_id", packet.ClientId,
			"payload", packet.Payload,
			"sequence_number", packet.SequenceNumber)
		span.End()
		d.Ack(false)
	}
}
