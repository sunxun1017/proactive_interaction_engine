package port

import (
	"context"

	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/event"
)

// AuditRecorder stores semantic, not raw-media, interaction records.
type AuditRecorder interface {
	RecordEvent(context.Context, event.SemanticEvent) error
	RecordDecision(context.Context, decision.Decision) error
	RecordPlan(context.Context, behavior.BehaviorPlan) error
	RecordActionStatus(context.Context, behavior.ActionStatus) error
}
