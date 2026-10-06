package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	packetsProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "packets_processed_total",
		Help: "Total packets successfully persisted to MongoDB.",
	}, []string{"client_id"})

	packetsFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "packets_failed_total",
		Help: "Total packets that failed processing.",
	}, []string{"reason"})

	rabbitmqPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rabbitmq_publish_errors_total",
		Help: "Total failed RabbitMQ publish attempts.",
	})

	mongodbOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mongodb_op_duration_seconds",
		Help:    "MongoDB operation latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})
)
