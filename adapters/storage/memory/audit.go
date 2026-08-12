package memory

import (
	"context"
	"sync"

	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/event"
)

type AuditRecorder struct {
	mu        sync.Mutex
	events    []event.SemanticEvent
	decisions []decision.Decision
	plans     []behavior.BehaviorPlan
	statuses  []behavior.ActionStatus
}

func (r *AuditRecorder) RecordEvent(ctx context.Context, value event.SemanticEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, value)
	return nil
}

func (r *AuditRecorder) RecordDecision(ctx context.Context, value decision.Decision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, value)
	return nil
}

func (r *AuditRecorder) RecordPlan(ctx context.Context, value behavior.BehaviorPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = append(r.plans, value)
	return nil
}

func (r *AuditRecorder) RecordActionStatus(ctx context.Context, value behavior.ActionStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, value)
	return nil
}

type AuditSnapshot struct {
	Events    []event.SemanticEvent
	Decisions []decision.Decision
	Plans     []behavior.BehaviorPlan
	Statuses  []behavior.ActionStatus
}

func (r *AuditRecorder) Snapshot() AuditSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return AuditSnapshot{
		Events:    append([]event.SemanticEvent(nil), r.events...),
		Decisions: append([]decision.Decision(nil), r.decisions...),
		Plans:     append([]behavior.BehaviorPlan(nil), r.plans...),
		Statuses:  append([]behavior.ActionStatus(nil), r.statuses...),
	}
}
