package main

import amqp "github.com/rabbitmq/amqp091-go"

// amqpHeaderCarrier adapts amqp.Table to the OTel TextMapCarrier interface,
// enabling W3C trace context propagation over AMQP message headers.
type amqpHeaderCarrier amqp.Table

func (c amqpHeaderCarrier) Get(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c amqpHeaderCarrier) Set(key, val string) {
	c[key] = val
}

func (c amqpHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
