package budget

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// No visitor identity in any label: one series per address would grow without
// bound and put personal data in the metrics store.
var (
	budgetRefusalsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_budget_refusals_total",
		Help: "Turns refused before reaching the model, by reason.",
	}, []string{"reason"})

	budgetGlobalUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "chat_budget_global_used_tokens",
		Help: "Tokens charged site-wide since the last UTC midnight or restart.",
	})

	budgetVisitorTokens = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_budget_visitor_tokens",
		Help:    "A visitor's running daily total, observed at each debit. Tune BUDGET_TOKENS_PER_VISITOR from it.",
		Buckets: []float64{500, 1000, 2000, 5000, 10000, 15000, 20000, 30000, 50000},
	})
)
