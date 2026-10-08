package guard

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	chatGuardScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_guard_score",
		Help:    "Worst injection probability per checked turn, observed before GUARD_THRESHOLD so the threshold can be tuned from traffic.",
		Buckets: []float64{.05, .1, .2, .3, .4, .5, .6, .7, .8, .9, .95, .99},
	})

	chatGuardChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_guard_checks_total",
		Help: "Guard checks by verdict. \"error\" turns proceed unguarded; client hang-ups are not counted.",
	}, []string{"verdict"})

	chatGuardDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_guard_duration_seconds",
		Help:    "Wall time of one guard check, all batches included. It runs alongside retrieval, so it only delays a turn when it is the slower of the two.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	})
)
