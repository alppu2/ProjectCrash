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

	chatRetrievalDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_retrieval_duration_seconds",
		Help:    "Wall time of the quota'd Qdrant search for one turn.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	})

	chatRetrievalChunks = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_retrieval_chunks",
		Help:    "Chunks surviving RETRIEVAL_MIN_SCORE. Zero means the reply was ungrounded by design.",
		Buckets: []float64{0, 1, 2, 3, 4, 5, 6, 8, 10},
	})

	chatRetrievalTopScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_retrieval_top_score",
		Help: "Best cosine score per turn. RETRIEVAL_MIN_SCORE is tuned from this, not argued about in a constant.",
		// Cosine similarity over normalised embeddings; below 0.2 is noise.
		Buckets: []float64{.2, .3, .4, .45, .5, .55, .6, .7, .8, .9},
	})

	chatRetrievalErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_retrieval_errors_total",
		Help: "Retrieval failures by reason. These degrade the reply rather than failing the stream, so chat_streams_total stays \"ok\".",
	}, []string{"reason"})

	chatEmbedDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_embed_duration_seconds",
		Help:    "Wall time of embedding one query.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	})

	chatCondenseDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_condense_duration_seconds",
		Help: "Wall time of folding history into a standalone question. Lands ahead of the first token and would otherwise hide inside chat_time_to_first_token_seconds.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
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
