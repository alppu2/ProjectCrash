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
