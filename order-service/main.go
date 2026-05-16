package main

import (
	"context"
	"io"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	
	pb "order-service/orders"
)

type server struct {
	pb.UnimplementedOrderServiceServer
	db *mongo.Collection
}

// SendPacket handles a single incoming packet
func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
	log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)

	// Persist to MongoDB
	_, err := s.db.InsertOne(ctx, in)
	if err != nil {
		return nil, err
	}

	return &pb.Response{
		Message: "Packet persisted to MongoDB",
		Success: true,
	}, nil
}

// StreamDisturbance handles high-volume incoming streams (Stress Test)
func (s *server) StreamDisturbance(stream pb.OrderService_StreamDisturbanceServer) error {
	var packetCount int32
	startTime := time.Now()

	for {
		// Read from the stream
		_, err := stream.Recv()
		if err == io.EOF {
			// Stream finished
			return stream.SendAndClose(&pb.Response{
				Message: string(packetCount) + " packets processed in " + time.Since(startTime).String(),
				Success: true,
			})
		}
		if err != nil {
			return err
		}

		packetCount++
		// Tech Lead Note: In a real scenario, we might process these in batches 
		// to avoid overloading the DB.
		if packetCount % 100 == 0 {
			log.Printf("Stress test: Received %d packets so far...", packetCount)
		}
	}
}

func main() {
	// 1. Connect to MongoDB
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://mongodb:27017"))
	if err != nil {
		log.Fatal(err)
	}
	collection := client.Database("order_db").Collection("packets")

	// 2. Setup gRPC Server
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterOrderServiceServer(s, &server{db: collection})

	log.Println("Order Service (gRPC) listening on :50051")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}