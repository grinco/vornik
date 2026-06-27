package autonomy

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the autonomy manager.
type Metrics struct {
	EvaluationsTotal *prometheus.CounterVec
	TasksCreated     *prometheus.CounterVec
	NoActionTotal    *prometheus.CounterVec
	ErrorsTotal      *prometheus.CounterVec
	EvalDuration     *prometheus.HistogramVec
}

// NewMetrics creates and registers autonomy metrics.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		EvaluationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "evaluations_total",
			Help:      "Total autonomous evaluations run.",
		}, []string{"project_id"}),
		TasksCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "tasks_created_total",
			Help:      "Total tasks created by autonomous lead.",
		}, []string{"project_id"}),
		NoActionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "no_action_total",
			Help:      "Total evaluations where the lead decided no action was needed.",
		}, []string{"project_id"}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "errors_total",
			Help:      "Total autonomous evaluation errors.",
		}, []string{"project_id"}),
		EvalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "evaluation_duration_seconds",
			Help:      "Duration of each autonomous evaluation.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		}, []string{"project_id"}),
	}
	reg.MustRegister(m.EvaluationsTotal, m.TasksCreated, m.NoActionTotal, m.ErrorsTotal, m.EvalDuration)
	return m
}
