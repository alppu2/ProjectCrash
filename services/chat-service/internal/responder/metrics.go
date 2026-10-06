package responder

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collectors owned by the responder stack. The stream-level counters live in
// the root package, next to the handler that is allowed to touch them.
var (
	chatProviderErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_provider_errors_total",
		Help: "LLM provider failures by reason. chat_streams_total{status=\"error\"} counts the same failures; this breaks down the cause.",
	}, []string{"reason"})

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
		Name:    "chat_condense_duration_seconds",
		Help:    "Wall time of folding history into a standalone question. Lands ahead of the first token and would otherwise hide inside chat_time_to_first_token_seconds.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
	})
)
