package service

import (
	"context"

	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// a2aPushNotifier builds the A2A webhook push notifier (pushNotificationConfig
// follow-up). Returns nil when the config repo isn't wired — without stored
// configs there's nothing to push, and the agent card won't advertise push.
// The same instance implements both executor.CompletionNotifier (terminal
// states) and executor.SteeringNotifier (AWAITING_INPUT) so it slots into the
// existing notifier multiplexers.
func (c *Container) a2aPushNotifier() *a2a.PushNotifier {
	if c == nil || c.repos == nil || c.repos.A2APushConfigs == nil {
		return nil
	}
	return a2a.NewPushNotifier(c.repos.A2APushConfigs, c.Logger.With().Str("component", "a2a-push").Logger())
}

// steeringMux fans a steering notification out to every wired sink (the
// chat/DM notifier + the A2A push notifier). Each sink no-ops for tasks it
// doesn't own, so a fan-out is safe.
type steeringMux struct {
	sinks []executor.SteeringNotifier
}

func (m *steeringMux) NotifySteeringRequired(ctx context.Context, task *persistence.Task, state string) {
	if m == nil {
		return
	}
	for _, s := range m.sinks {
		s.NotifySteeringRequired(ctx, task, state)
	}
}

// combinedSteeringNotifier merges the chat/DM steering notifier with the A2A
// push notifier. Returns a single executor.SteeringNotifier (the lone sink
// directly when only one is wired, the mux when both are).
func (c *Container) combinedSteeringNotifier() executor.SteeringNotifier {
	sinks := []executor.SteeringNotifier{}
	if s := c.steeringNotifier(); s != nil {
		sinks = append(sinks, s)
	}
	if o := c.operatorAlertNotifier(); o != nil {
		sinks = append(sinks, o)
	}
	if p := c.a2aPushNotifier(); p != nil {
		sinks = append(sinks, p)
	}
	switch len(sinks) {
	case 0:
		return nil
	case 1:
		return sinks[0]
	default:
		return &steeringMux{sinks: sinks}
	}
}
