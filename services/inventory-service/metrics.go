package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	packetsConsumedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "packets_consumed_total",
		Help: "Total messages successfully consumed from RabbitMQ.",
	})

	packetsDecodeErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "packets_decode_errors_total",
		Help: "Total messages that failed JSON decode.",
	})

	rabbitmqConsumeLag = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "rabbitmq_consume_lag_seconds",
		Help:    "Time between message publish and consume in seconds.",
		Buckets: prometheus.DefBuckets,
	})
)
