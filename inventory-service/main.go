package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
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
		conn, err = amqp.Dial(os.Getenv("RABBITMQ_URL"))
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
	conn, ch, err := connectRabbitMQ()
	if err != nil {
		log.Fatalf("RabbitMQ setup failed: %v", err)
	}
	defer conn.Close()
	defer ch.Close()

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
