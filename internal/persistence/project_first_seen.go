package persistence

import "context"

// ProjectFirstSeenRepository records the first time the daemon observed each
// project id, so a lifecycle counter fires once per project rather than once per
// registry load.
//
// WHY IT EXISTS. A project is creatable four ways and only three emitted
// `project_created`: `vornikctl init --template`, `vornikctl init`, and the
// API/UI template create. The fourth — writing `configs/projects/<id>.yaml` by
// hand, by config management, or by copying an existing project — emitted
// nothing, because `internal/registry` has no telemetry in it at all. That is
// not a small slice: it is the path an operator uses for every project after
// their first, and the one the docs describe for anything the templates do not
// cover. Worse, the error is CORRELATED WITH MATURITY — the more established a
// deployment, the larger the share of its projects the counter cannot see.
//
// THE HARD PART IS "CREATED", NOT THE EMIT. `LoadProjects` runs on every daemon
// start and every registry reload, and sees existing and new projects
// identically; emitting per load would turn a lifecycle counter into a restart
// counter. So the predicate is a persisted first-seen marker, and the emit is
// conditioned on the marker actually being NEW.
type ProjectFirstSeenRepository interface {
	// MarkSeen records projectID as observed and reports whether THIS call was
	// the one that recorded it.
	//
	// The insert and the answer are one statement on purpose. A read-then-write
	// would let two daemons — or a start racing a reload — both observe the
	// project absent and both emit, which turns the fix into a duplicate-count
	// bug one restart later.
	//
	// source describes how the project came to exist, for the event this gates.
	MarkSeen(ctx context.Context, projectID, source string) (firstTime bool, err error)
}
