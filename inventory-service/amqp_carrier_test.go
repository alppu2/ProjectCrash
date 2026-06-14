package main

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestAmqpHeaderCarrier_SetAndGet(t *testing.T) {
	table := amqp.Table{}
	carrier := amqpHeaderCarrier(table)

	carrier.Set("traceparent", "00-abc-def-01")

	got := carrier.Get("traceparent")
	if got != "00-abc-def-01" {
		t.Errorf("Get() = %q, want %q", got, "00-abc-def-01")
	}
}

func TestAmqpHeaderCarrier_GetMissing(t *testing.T) {
	carrier := amqpHeaderCarrier(amqp.Table{})
	if got := carrier.Get("missing"); got != "" {
		t.Errorf("Get() for missing key = %q, want empty string", got)
	}
}

func TestAmqpHeaderCarrier_Keys(t *testing.T) {
	table := amqp.Table{"a": "1", "b": "2"}
	carrier := amqpHeaderCarrier(table)
	keys := carrier.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() returned %d keys, want 2", len(keys))
	}
}
