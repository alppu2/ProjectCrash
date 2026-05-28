package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"

	pb "order-service/orders"
)

type server struct {
	pb.UnimplementedOrderServiceServer
	collection  *mongo.Collection
	amqpConn    *amqp.Connection
	amqpChannel *amqp.Channel
	mu          sync.Mutex
}

// dialRabbitMQ makes a single connection attempt with no retries.
func dialRabbitMQ() (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
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
		log.Printf("RabbitMQ not ready, retrying (%d/30)...", i+1)
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

	log.Println("RabbitMQ channel closed, reconnecting...")

	// Try reopening a channel on the existing connection — cheap, no new TCP handshake.
	if existingConn != nil && !existingConn.IsClosed() {
		channel, err := existingConn.Channel()
		if err == nil {
			_, err = channel.QueueDeclare("packets", false, false, false, false, nil)
			if err == nil {
				s.mu.Lock()
				s.amqpChannel = channel
				s.mu.Unlock()
				log.Println("RabbitMQ channel reopened")
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

	log.Println("RabbitMQ reconnected")
	return newChannel, nil
}

func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
	log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)

	_, err := s.collection.InsertOne(ctx, in)
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
		return &pb.Response{Message: "Packet persisted to MongoDB", Success: true}, nil
	}

	amqpChannel, err := s.ensureChannel()
	if err != nil {
		log.Printf("WARN: RabbitMQ unavailable: %v", err)
		return &pb.Response{Message: "Packet persisted to MongoDB", Success: true}, nil
	}

	if pubErr := amqpChannel.PublishWithContext(ctx, "", "packets", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
	}); pubErr != nil {
		log.Printf("WARN: failed to publish to RabbitMQ: %v", pubErr)
	}

	return &pb.Response{
		Message: "Packet persisted to MongoDB",
		Success: true,
	}, nil
}

func (s *server) StressTest(ctx context.Context, in *pb.StressTestRequest) (*pb.Response, error) {
	startTime := time.Now()

	amqpChannel, err := s.ensureChannel()
	if err != nil {
		log.Printf("WARN: RabbitMQ unavailable for stress test: %v", err)
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

		if amqpChannel != nil {
			body, err := json.Marshal(map[string]interface{}{
				"client_id":       packet.ClientId,
				"payload":         packet.Payload,
				"sequence_number": packet.SequenceNumber,
			})
			if err != nil {
				log.Printf("WARN: failed to marshal packet %d for RabbitMQ: %v", i, err)
			} else if pubErr := amqpChannel.PublishWithContext(ctx, "", "packets", false, false, amqp.Publishing{
				ContentType: "application/json",
				Body:        body,
			}); pubErr != nil {
				log.Printf("WARN: failed to publish packet %d to RabbitMQ: %v", i, pubErr)
			}
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
				Message: fmt.Sprintf("%d packets processed in %s", packetCount, time.Since(startTime)),
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

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://mongodb:27017"))
	if err != nil {
		log.Fatal(err)
	}
	collection := mongoClient.Database("order_db").Collection("packets")

	amqpConn, amqpChannel, err := connectRabbitMQ()
	if err != nil {
		log.Fatalf("RabbitMQ setup failed: %v", err)
	}

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterOrderServiceServer(grpcServer, &server{
		collection:  collection,
		amqpConn:    amqpConn,
		amqpChannel: amqpChannel,
	})

	log.Println("Order Service (gRPC) listening on :50051")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
