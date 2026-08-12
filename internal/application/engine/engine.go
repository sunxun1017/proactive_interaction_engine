package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
	"proactive-interaction-engine/internal/domain/state"
)

// Engine has single-writer semantics. Process observations from one goroutine.
type Engine struct {
	config    Config
	ingress   *ingress
	compiler  *event.Compiler
	projector *state.Projector
	policy    decision.Policy
	planner   behavior.Planner
	driver    port.ActionDriver
	recorder  port.AuditRecorder
	clock     port.Clock
}

type Result struct {
	Events         []event.SemanticEvent
	Decisions      []decision.Decision
	Plans          []behavior.BehaviorPlan
	ActionStatuses []behavior.ActionStatus
	Warnings       []string
}

func New(config Config, driver port.ActionDriver, recorder port.AuditRecorder, clock port.Clock) (*Engine, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate engine config: %w", err)
	}
	if driver == nil || recorder == nil || clock == nil {
		return nil, errors.New("driver, recorder, and clock are required")
	}
	return &Engine{
		config:    config,
		ingress:   newIngress(),
		compiler:  event.NewCompiler(config.ReturnAbsenceThreshold),
		projector: state.NewProjector(config.SubjectID),
		policy: decision.RulePolicy{
			Version:         config.PolicyVersion,
			BehaviorVersion: config.BehaviorVersion,
			RandomSeed:      config.RandomSeed,
		},
		planner:  behavior.Planner{ActionTimeout: config.ActionTimeout},
		driver:   driver,
		recorder: recorder,
		clock:    clock,
	}, nil
}

// Process runs one canonical observation through the local real-time path.
func (e *Engine) Process(ctx context.Context, input observation.Observation) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, classifyContext("process observation", err)
	}
	if input.SubjectID != e.config.SubjectID {
		return Result{}, fault.New(
			fault.InvalidInput,
			"process observation",
			fmt.Errorf("subject %s does not match engine subject %s", input.SubjectID, e.config.SubjectID),
		)
	}
	now := e.clock.Now()
	if err := e.ingress.accept(input, now); err != nil {
		return Result{}, fmt.Errorf("ingress observation %s: %w", input.ID, err)
	}

	result := Result{Events: e.compiler.Compile(input)}
	for _, semanticEvent := range result.Events {
		e.bestEffortAudit(&result, func(auditCtx context.Context) error {
			return e.recorder.RecordEvent(auditCtx, semanticEvent)
		})

		snapshot := e.projector.Apply(semanticEvent)
		outcome, err := decision.Evaluate(e.policy, snapshot, semanticEvent, e.config.ConfigHash)
		if err != nil {
			return result, fmt.Errorf("evaluate event %s: %w", semanticEvent.ID, err)
		}
		if outcome == nil {
			continue
		}
		result.Decisions = append(result.Decisions, *outcome)
		e.bestEffortAudit(&result, func(auditCtx context.Context) error {
			return e.recorder.RecordDecision(auditCtx, *outcome)
		})

		if outcome.Kind == decision.Silent {
			continue
		}

		capabilities, err := e.capabilities(ctx)
		if err != nil {
			return result, err
		}
		plan, err := e.planner.Plan(*outcome, capabilities, now)
		if err != nil {
			return result, fmt.Errorf("plan decision %s: %w", outcome.ID, err)
		}
		result.Plans = append(result.Plans, plan)
		e.bestEffortAudit(&result, func(auditCtx context.Context) error {
			return e.recorder.RecordPlan(auditCtx, plan)
		})

		statuses, err := e.dispatch(ctx, plan, now)
		result.ActionStatuses = append(result.ActionStatuses, statuses...)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (e *Engine) capabilities(ctx context.Context) (behavior.Capabilities, error) {
	callCtx, cancel := context.WithTimeout(ctx, e.config.ExternalCallTimeout)
	defer cancel()
	capabilities, err := e.driver.Capabilities(callCtx)
	if err != nil {
		return nil, fault.New(fault.Unavailable, "read embodiment capabilities", err)
	}
	return capabilities, nil
}

func (e *Engine) dispatch(ctx context.Context, plan behavior.BehaviorPlan, now time.Time) ([]behavior.ActionStatus, error) {
	var statuses []behavior.ActionStatus
	for _, command := range plan.Commands(now) {
		commandCtx, cancel := context.WithTimeout(ctx, command.Action.Timeout)
		stream, err := e.driver.Execute(commandCtx, command)
		if err != nil {
			cancel()
			return statuses, fault.New(fault.AdapterRejected, "execute action "+command.ID, err)
		}

		for {
			select {
			case <-commandCtx.Done():
				cancel()
				return statuses, classifyContext("await action "+command.ID, commandCtx.Err())
			case status, ok := <-stream:
				if !ok {
					cancel()
					goto nextCommand
				}
				statuses = append(statuses, status)
				e.bestEffortAuditStatus(statuses, status)
			}
		}
	nextCommand:
	}
	return statuses, nil
}

func (e *Engine) bestEffortAudit(result *Result, record func(context.Context) error) {
	auditCtx, cancel := context.WithTimeout(context.Background(), e.config.ExternalCallTimeout)
	defer cancel()
	if err := record(auditCtx); err != nil {
		result.Warnings = append(result.Warnings, "audit unavailable: "+err.Error())
	}
}

func (e *Engine) bestEffortAuditStatus(_ []behavior.ActionStatus, status behavior.ActionStatus) {
	auditCtx, cancel := context.WithTimeout(context.Background(), e.config.ExternalCallTimeout)
	defer cancel()
	_ = e.recorder.RecordActionStatus(auditCtx, status)
}

func classifyContext(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

// Snapshot exposes a value copy for queries without allowing mutation.
func (e *Engine) Snapshot() state.WorldSnapshot { return e.projector.Snapshot() }

// StopAll requests cancellation of interruptible software actions. It is not a
// physical emergency stop.
func (e *Engine) StopAll(ctx context.Context, reason string) error {
	callCtx, cancel := context.WithTimeout(ctx, e.config.ExternalCallTimeout)
	defer cancel()
	if err := e.driver.StopAll(callCtx, reason); err != nil {
		return fault.New(fault.Unavailable, "stop all actions", err)
	}
	return nil
}
