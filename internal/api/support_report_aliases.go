package api

import "vornik.io/vornik/internal/supportbundle"

// The support bundle's collector moved to internal/supportbundle on 2026-09-04
// so the CLI can drive the SAME collector for Community-Edition local
// collection without importing the HTTP server (support-bundle-in-CE design
// §3). These aliases keep internal/api's wiring surface unchanged: the service
// container and the Server's options name these types, and a rename there
// would have been a second, unrelated change riding along with a move.
//
// Aliases, not new types: a value satisfying supportbundle.DoctorRunner IS an
// api.SupportDoctorRunner, so nothing has to be re-wrapped at the seam.
type (
	// SupportDoctorRunner runs the doctor for the bundle's doctor.json.
	SupportDoctorRunner = supportbundle.DoctorRunner
	// SupportHealthSource snapshots daemon health for health.json.
	SupportHealthSource = supportbundle.HealthSource
	// SupportMetricsSource renders the Prometheus text for metrics.txt.
	SupportMetricsSource = supportbundle.MetricsSource
	// SupportJudgeReader reads a task's judge verdicts.
	SupportJudgeReader = supportbundle.SupportJudgeReader
	// SupportPostMortemReader reads a task's post-mortem.
	SupportPostMortemReader = supportbundle.SupportPostMortemReader
)

// No alias for the trace service: api.BlackBoxTraceService (blackbox_iface.go)
// already has AssembleCached plus Compare, so it assigns straight into the
// builder's narrower supportbundle.TraceService field. An alias would have been
// a third name for one seam.
