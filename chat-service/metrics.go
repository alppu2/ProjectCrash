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
		// DefBuckets stop at 10s, which lands every model-backed stream in
		// +Inf. The low buckets stay for the echo stub, which finishes sooner.
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300},
	})

	chatProviderErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_provider_errors_total",
		Help: "LLM provider failures by reason. chat_streams_total{status=\"error\"} counts the same failures; this breaks down the cause.",
	}, []string{"reason"})

	chatTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_tokens_total",
		Help: "Tokens reported by the responder, by direction.",
	}, []string{"direction"})

	chatTimeToFirstTokenSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_time_to_first_token_seconds",
		Help: "Latency from the provider request to the first streamed delta.",
		// DefBuckets stop at 10s, dumping every cold VRAM load (~33s on a GTX
		// 1060) into +Inf alongside nothing else.
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	})
)

// recordTokens accounts one turn's usage. Zeros are skipped so a responder
// that reports no tokens (the echo stub, or a provider ignoring
// stream_options.include_usage) leaves the series alone.
func recordTokens(u Usage) {
	if u.InputTokens > 0 {
		chatTokensTotal.WithLabelValues("input").Add(float64(u.InputTokens))
	}
	if u.OutputTokens > 0 {
		chatTokensTotal.WithLabelValues("output").Add(float64(u.OutputTokens))
	}
}
