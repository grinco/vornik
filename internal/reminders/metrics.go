// Package reminders metrics. See https://docs.vornik.io
// §4.1/§7 for the task-kind fire path these counters observe.
package reminders

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricsNamespace = "vornik"
	metricsSubsystem = "reminders"
)

var (
	// metricTaskSpawned counts task-kind reminder fires that
	// successfully created a task (deliverTask, before re-arm).
	// Full name: vornik_reminders_task_spawned_total.
	metricTaskSpawned = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "task_spawned_total",
		Help:      "Task-kind reminder fires that spawned a task.",
	}, []string{"project"})

	// metricTaskDelivered counts task-kind outcomes delivered to a
	// channel (Task 6's ClaimDelivery/FinalizeDelivery path).
	// Full name: vornik_reminders_task_delivered_total.
	metricTaskDelivered = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "task_delivered_total",
		Help:      "Task-kind reminder outcomes delivered to a channel.",
	}, []string{"success"})

	// metricTaskSkipped counts task-kind fires skipped because the
	// prior run outran its slot (still awaiting_task at the next fire).
	// Full name: vornik_reminders_task_skipped_total.
	metricTaskSkipped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "task_skipped_total",
		Help:      "Task-kind fires skipped because the prior run outran its slot.",
	})

	// metricTaskDeliverErrors counts channel send failures at
	// task-kind delivery (Task 6).
	// Full name: vornik_reminders_task_deliver_errors_total.
	metricTaskDeliverErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "task_deliver_errors_total",
		Help:      "Channel send failures at task-kind delivery.",
	})

	// metricFiringReclaimed counts rows the stuck-firing reconciliation
	// sweep reclaims (Task 14, design §9 crash recovery) — a row that
	// sat in 'firing' past the grace window because a crash interrupted
	// spawn (LeaseDue -> MarkTaskSpawned), delivery (Send/ClaimDelivery
	// -> FinalizeDelivery), or the pre-existing text-kind failed-Send
	// case. Full name: vornik_reminders_firing_reclaimed_total.
	metricFiringReclaimed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "firing_reclaimed_total",
		Help:      "Rows in status=firing past the grace window reclaimed by the crash-recovery sweep.",
	})
)
