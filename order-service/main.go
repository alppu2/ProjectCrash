package main

import (
	"context"
	"fmt"
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

func (s *server) SendPacket(ctx context.Context, in *pb.DataPacket) (*pb.Response, error) {
	log.Printf("Received packet: %s from %s", in.Payload, in.ClientId)

	_, err := s.db.InsertOne(ctx, in)
	if err != nil {
		return nil, err
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
		if packetCount % 100 == 0 {
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
