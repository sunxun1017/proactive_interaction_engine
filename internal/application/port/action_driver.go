package port

import (
	"context"

	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
)

// ActionDriver executes abstract commands through an embodiment adapter.
type ActionDriver interface {
	Capabilities(context.Context) (behavior.Capabilities, error)
	Execute(context.Context, behavior.ActionCommand) (<-chan behavior.ActionStatus, error)
	StopAll(context.Context, control.StopReason) error
}
