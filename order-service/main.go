package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	pb "order-service/orders"
)

var tracer trace.Tracer

type server struct {
	pb.UnimplementedOrderServiceServer
	collection  *mongo.Collection
	amqpConn    *amqp.Connection
	amqpChannel *amqp.Channel
	mu          sync.Mutex
}

// dialRabbitMQ makes a single connection attempt with no retries.
func dialRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(os.Getenv("RABBITMQ_URL"))
	if err != nil {
		return nil, nil, err
	}

	channel, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to open channel: %w", err)
	}

	_, err = channel.QueueDeclare("packets", false, false, false, false, nil)
	if err != nil {
		channel.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("failed to declare queue: %w", err)
	}

	return conn, channel, nil
}

// connectRabbitMQ retries dialRabbitMQ up to 30 times. Used at startup only.
func connectRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	for i := 0; i < 30; i++ {
		conn, channel, err := dialRabbitMQ()
		if err == nil {
			return conn, channel, nil
		}
		slog.Info("rabbitmq not ready, retrying", "attempt", i+1, "max", 30)
		time.Sleep(time.Second)
	}
	return nil, nil, fmt.Errorf("failed to connect to RabbitMQ after 30 attempts")
}

// ensureChannel returns a ready channel, reconnecting if needed.
// Callers must use the returned channel — do not re-read s.amqpChannel after this returns.
func (s *server) ensureChannel() (*amqp.Channel, error) {
	s.mu.Lock()
	if s.amqpChannel != nil && !s.amqpChannel.IsClosed() {
		channel := s.amqpChannel
		s.mu.Unlock()
		return channel, nil
	}
	existingConn := s.amqpConn
	s.mu.Unlock()

	slog.Warn("rabbitmq channel closed, reconnecting")

	// Try reopening a channel on the existing connection — cheap, no new TCP handshake.
	if existingConn != nil && !existingConn.IsClosed() {
		channel, err := existingConn.Channel()
		if err == nil {
			_, err = channel.QueueDeclare("packets", false, false, false, false, nil)
			if err == nil {
				s.mu.Lock()
				s.amqpChannel = channel
				s.mu.Unlock()
				slog.Info("rabbitmq channel reopened")
				return channel, nil
			}
			channel.Close()
		}
	}

	// Full reconnect — single attempt outside the lock so other goroutines are not blocked.
	newConn, newChannel, err := dialRabbitMQ()
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ reconnect failed: %w", err)
	}

	s.mu.Lock()
	if s.amqpConn != nil {
		s.amqpConn.Close()
	}
	s.amqpConn = newConn
	s.amqpChannel = newChannel
	s.mu.Unlock()

	slog.Info("rabbitmq reconnected")
	return newChannel, nil
}

func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
	log := logWithTrace(ctx, slog.Default())
	log.Info("received packet", "payload", in.Payload, "client_id", in.ClientId)

	ctx, mongoSpan := tracer.Start(ctx, "mongodb.insert_one",
		trace.WithAttributes(
			attribute.String("db.system", "mongodb"),
			attribute.String("db.operation", "insert_one"),
		),
	)
	timer := prometheus.NewTimer(mongodbOpDuration.WithLabelValues("insert_one"))
	_, err := s.collection.InsertOne(ctx, in)
	timer.ObserveDuration()
	if err != nil {
		mongoSpan.RecordError(err)
		mongoSpan.End()
		packetsFailedTotal.WithLabelValues("mongodb_error").Inc()
		return nil, err
	}
	mongoSpan.End()
	packetsProcessedTotal.WithLabelValues(in.ClientId).Inc()

	body, err := json.Marshal(map[string]interface{}{
		"client_id":       in.ClientId,
		"payload":         in.Payload,
		"sequence_number": in.SequenceNumber,
		"published_at":    float64(time.Now().UnixNano()) / float64(time.Second),
	})
	if err != nil {
		log.Warn("failed to marshal packet for rabbitmq", "error", err)
		return &pb.Response{Message: "Packet persisted to MongoDB", Success: true}, nil
	}

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
		log.Warn("rabbitmq unavailable", "error", err)
		publishSpan.RecordError(err)
		publishSpan.End()
		return &pb.Response{Message: "Packet persisted to MongoDB", Success: true}, nil
	}

	if pubErr := amqpChannel.PublishWithContext(publishCtx, "", "packets", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     headers,
	}); pubErr != nil {
		log.Warn("failed to publish to rabbitmq", "error", pubErr)
		publishSpan.RecordError(pubErr)
		rabbitmqPublishErrorsTotal.Inc()
	}
	publishSpan.End()

	return &pb.Response{
		Message: "Packet persisted to MongoDB",
		Success: true,
	}, nil
}

func (s *server) StressTest(ctx context.Context, in *pb.StressTestRequest) (*pb.Response, error) {
	ctx, span := tracer.Start(ctx, "stress_test",
		trace.WithAttributes(attribute.Int("stress_test.count", int(in.Count))),
	)
	defer span.End()

	log := logWithTrace(ctx, slog.Default())
	startTime := time.Now()

	amqpChannel, err := s.ensureChannel()
	if err != nil {
		log.Warn("rabbitmq unavailable for stress test", "error", err)
		amqpChannel = nil
	}

	for i := int32(0); i < in.Count; i++ {
		packet := &pb.DataPacket{
			ClientId:       in.ClientId,
			Payload:        in.Payload,
			SequenceNumber: i,
		}
		if _, err := s.collection.InsertOne(ctx, packet); err != nil {
			return nil, err
		}
		packetsProcessedTotal.WithLabelValues(in.ClientId).Inc()

		if amqpChannel != nil {
			body, err := json.Marshal(map[string]interface{}{
				"client_id":       packet.ClientId,
				"payload":         packet.Payload,
				"sequence_number": packet.SequenceNumber,
				"published_at":    float64(time.Now().UnixNano()) / float64(time.Second),
			})
			if err != nil {
				log.Warn("failed to marshal packet for rabbitmq", "sequence", i, "error", err)
			} else {
				headers := amqp.Table{}
				otel.GetTextMapPropagator().Inject(ctx, amqpHeaderCarrier(headers))
				if pubErr := amqpChannel.PublishWithContext(ctx, "", "packets", false, false, amqp.Publishing{
					ContentType: "application/json",
					Body:        body,
					Headers:     headers,
				}); pubErr != nil {
					log.Warn("failed to publish packet to rabbitmq", "sequence", i, "error", pubErr)
				}
			}
		}

		if i%100 == 0 {
			log.Info("stress test progress", "processed", i, "total", in.Count)
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
				Message: fmt.Sprintf("%d packets processed in %s", packetCount, time.Since(startTime)),
				Success: true,
			})
		}
		if err != nil {
			return err
		}

		packetCount++
		if packetCount%100 == 0 {
			slog.Info("stream disturbance progress", "received", packetCount)
		}
	}
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
	tracer = otel.Tracer("order-service")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(os.Getenv("MONGODB_URL")))
	if err != nil {
		slog.Error("failed to connect to mongodb", "error", err)
		os.Exit(1)
	}
	collection := mongoClient.Database("order_db").Collection("packets")

	amqpConn, amqpChannel, err := connectRabbitMQ()
	if err != nil {
		slog.Error("rabbitmq setup failed", "error", err)
		os.Exit(1)
	}

	grpcPort := os.Getenv("GRPC_PORT")
	lis, err := net.Listen("tcp", grpcPort)
	if err != nil {
		slog.Error("failed to listen", "error", err, "port", grpcPort)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)
	pb.RegisterOrderServiceServer(grpcServer, &server{
		collection:  collection,
		amqpConn:    amqpConn,
		amqpChannel: amqpChannel,
	})

	slog.Info("order service listening", "port", grpcPort)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc server failed", "error", err)
		os.Exit(1)
	}
}
