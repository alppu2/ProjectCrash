package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	chatStreamsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_streams_total",
		Help: "Total chat streams by terminal status.",
	}, []string{"status"})

	chatChunksSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chat_chunks_sent_total",
		Help: "Total ChatChunk text deltas sent to clients.",
	})

	chatStreamDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_stream_duration_seconds",
		Help:    "Wall time of a chat stream from request to terminal frame.",
		Buckets: prometheus.DefBuckets,
	})

	chatProviderErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_provider_errors_total",
		Help: "LLM provider failures by reason. chat_streams_total{status=\"error\"} counts the same failures; this breaks down the cause.",
	}, []string{"reason"})
)
