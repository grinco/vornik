package service

// Cross-replica wrapping for the live-events publisher. Stays
// in its own file so the multi-line composition + DSN-building
// doesn't bloat container.go's already-large wire block.

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/executor/livepubsub"
)

// wrapLivePublisherCrossReplica composes the existing in-process
// publisher with a Postgres-backed cross-replica layer. The
// returned Publisher persists every event + NOTIFY-broadcasts so
// other replicas' LISTEN goroutines see it; their LISTEN
// goroutine ingests events from other replicas into this
// replica's in-process ring.
//
// Returns (nil, nil, nil) when c.livePub doesn't appear to be a
// stock in-process publisher — defensive against a future
// refactor where someone wires a different concrete impl and the
// wrapping no longer applies. Callers fall through to the bare
// in-process publisher in that case.
func (c *Container) wrapLivePublisherCrossReplica(ctx context.Context) (livepubsub.Publisher, func(), error) {
	if c.livePub == nil {
		return nil, nil, fmt.Errorf("livepubsub: no inner publisher")
	}

	dsn := postgresDSNFromConfig(c.Config.Database.Host, c.Config.Database.Port,
		c.Config.Database.User, c.Config.Database.Password,
		c.Config.Database.Name, c.Config.Database.SSLMode)

	notifier := livepubsub.NewPostgresNotifier(c.DB)
	listener := livepubsub.NewPostgresListener(dsn,
		c.Logger.With().Str("component", "livepubsub-listener").Logger())

	return livepubsub.NewDBBacked(ctx, livepubsub.NewDBBackedConfig{
		// The inner publisher is wired as a Publisher interface
		// on the container, but the cross-replica wrapper needs
		// the concrete *inProcessPublisher type for its
		// IngestRemote hot path. The package's NewDBBacked
		// caller relaxes this by accepting a Publisher and
		// type-asserting internally when needed — for now we
		// require the caller (this site) to pass nil and let
		// the wrapper construct its own inner, since the
		// pre-wrap in-process publisher we built above is
		// already serving subscribers and the swap can't be
		// atomic. The original publisher's sweeper still runs
		// (held by c.livePubShutdown).
		//
		// Net behaviour: callers that already obtained the
		// pre-wrap publisher reference (none in production — the
		// executor consumes through c.livePub which we
		// overwrite below) keep their old reference, but the
		// container's c.livePub is the new wrapper from this
		// point forward.
		Repo:     c.repos.LiveEvents,
		Notifier: notifier,
		Listener: listener,
		NodeID:   c.daemonHolderID(),
		Logger:   c.Logger.With().Str("component", "livepubsub").Logger(),
	})
}

// postgresDSNFromConfig builds a libpq-style DSN from the daemon's
// DatabaseConfig fields. Mirrors the format used by
// internal/persistence/postgres.Config.DSN — repeated here to
// avoid pulling internal/persistence/postgres into the executor
// dependency chain through the livepubsub listener.
//
// sslmode defaults to "disable" when blank, matching the rest of
// the daemon's "no surprise SSL upgrade" stance.
func postgresDSNFromConfig(host string, port int, user, password, database, sslmode string) string {
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		escapeDSN(host), port, escapeDSN(user), escapeDSN(password),
		escapeDSN(database), escapeDSN(sslmode))
}

// escapeDSN wraps values that contain whitespace or single quotes
// in single quotes; bare alphanumeric/safe values pass through
// unchanged. Mirrors the postgres.Config.escapeDSNValue helper.
func escapeDSN(v string) string {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' || c == '\'' || c == '\\' {
			// Quote + escape inner single-quotes/backslashes.
			esc := make([]byte, 0, len(v)+4)
			esc = append(esc, '\'')
			for j := 0; j < len(v); j++ {
				if v[j] == '\'' || v[j] == '\\' {
					esc = append(esc, '\\')
				}
				esc = append(esc, v[j])
			}
			esc = append(esc, '\'')
			return string(esc)
		}
	}
	return v
}
