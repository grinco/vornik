package service

import (
	"os"

	"vornik.io/vornik/internal/dispatcher"
)

// mediaSight builds the receiver's inline-media configuration from daemon
// config, shared by every channel receiver so one channel cannot end up with
// different perception rules than another.
//
// Returns nil when the resolved dispatcher model has no declared or inferred
// vision capability — nil disables inline media entirely and every media
// attachment hands over to the vision role, which is exactly the intended
// behaviour for a text-only dispatcher and saves each receiver from
// re-deciding it.
//
// see LLD § https://docs.vornik.io §4.3
func (c *Container) mediaSight() *dispatcher.MediaSight {
	if c.Config == nil {
		return nil
	}
	declared, err := c.Config.Chat.DeclaredModalities()
	if err != nil {
		// Validate() already rejected this at load; nil-on-error degrades
		// toward handover rather than toward claiming sight.
		declared = nil
	}
	perImage, total, count := c.Config.Media.InlineLimits()

	// Roots a channel-supplied ChannelRef may be read from. Both inbound
	// channels that carry attachments today write into one of these: email
	// persists into the artifact store, Telegram downloads into the project
	// workspace uploads dir or the temp dir.
	roots := []string{os.TempDir()}
	if p := c.Config.Storage.ArtifactsPath; p != "" {
		roots = append(roots, p)
	}
	if p := c.Config.Runtime.ProjectWorkspacePath; p != "" {
		roots = append(roots, p)
	}

	return &dispatcher.MediaSight{
		Model:    c.Config.Chat.Model,
		Declared: declared,
		// Store stays nil: every channel that carries inbound attachments
		// today exposes a readable path via ChannelRef, so the path branch
		// serves them. The interface remains the seam for a channel that
		// persists bytes without a readable path.
		Store:            nil,
		AllowedRoots:     roots,
		MaxBytesPerImage: perImage,
		MaxBytesTotal:    total,
		MaxImages:        count,
		Metrics:          containerMediaObserver{c: c},
	}
}

// containerMediaObserver forwards media dispositions to the dispatcher's
// Prometheus metrics, resolved LAZILY at call time.
//
// The indirection exists because channel receivers are constructed before
// the metrics registry is wired, so capturing the *Metrics pointer here
// would capture nil and silently drop every observation for the process's
// lifetime. Reading the field per call costs nothing measurable and is
// nil-safe on both sides (Metrics' methods no-op on a nil receiver).
type containerMediaObserver struct{ c *Container }

func (o containerMediaObserver) MediaAttachment(kind, disposition string) {
	if o.c == nil {
		return
	}
	o.c.dispatcherMetrics.MediaAttachment(kind, disposition)
}

func (o containerMediaObserver) MediaHandover(kind, reason string) {
	if o.c == nil {
		return
	}
	o.c.dispatcherMetrics.MediaHandover(kind, reason)
}

// disclosureObserver returns the Art 50 disclosure observer for a channel
// receiver, resolved LAZILY for the same reason as containerMediaObserver:
// receivers are constructed before the metrics registry is wired, so
// capturing the *Metrics pointer here would capture nil and silently drop
// every conformity observation for the process's lifetime.
//
// Silently is the operative word. Art 50 is served deep inside the receiver
// and nothing user-visible changes if the counters never move, so a nil
// observer looks exactly like a conforming deployment with no traffic.
//
// see LLD § https://docs.vornik.io §4.1
func (c *Container) disclosureObserver() dispatcher.DisclosureObserver {
	return containerDisclosureObserver{c: c}
}

type containerDisclosureObserver struct{ c *Container }

func (o containerDisclosureObserver) DisclosureServed(channel, cadence string) {
	if o.c == nil {
		return
	}
	o.c.dispatcherMetrics.DisclosureServed(channel, cadence)
}

func (o containerDisclosureObserver) DisclosureFailed(channel, stage string) {
	if o.c == nil {
		return
	}
	o.c.dispatcherMetrics.DisclosureFailed(channel, stage)
}
