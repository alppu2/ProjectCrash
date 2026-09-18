package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"chat-service/internal/responder"
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
		// DefBuckets stop at 10s, which lands every model-backed stream in
		// +Inf. The low buckets stay for the echo stub, which finishes sooner.
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300},
	})

	chatTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_tokens_total",
		Help: "Tokens reported by the responder, by direction.",
	}, []string{"direction"})
)

// recordTokens accounts one turn's usage. Zeros are skipped so a responder
// that reports no tokens (the echo stub, or a provider ignoring
// stream_options.include_usage) leaves the series alone.
func recordTokens(u responder.Usage) {
	if u.InputTokens > 0 {
		chatTokensTotal.WithLabelValues("input").Add(float64(u.InputTokens))
	}
	if u.OutputTokens > 0 {
		chatTokensTotal.WithLabelValues("output").Add(float64(u.OutputTokens))
	}
}
