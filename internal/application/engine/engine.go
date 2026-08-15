package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/episode"
	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
	"proactive-interaction-engine/internal/domain/state"
)

// Engine has single-writer semantics. Process observations from one goroutine.
type Engine struct {
	config       Config
	ingress      *ingress
	compiler     *event.Compiler
	projector    *state.Projector
	episodes     *episode.Tracker
	pendingReply *pendingReplyContinuation
	replyWindow  replyAcceptanceWindowStore
	policy       decision.Policy
	planner      behavior.Planner
	driver       port.ActionDriver
	recorder     port.AuditRecorder
	clock        port.Clock
}

type Result struct {
	Events         []event.SemanticEvent
	Decisions      []decision.Decision
	Plans          []behavior.BehaviorPlan
	ActionStatuses []behavior.ActionStatus
	Outcomes       []episode.Outcome
	Warnings       []string
}

// Wakeup is an application-owned, business-opaque deadline token. Engine and
// its caller preserve the same single-writer ordering used by Process.
type Wakeup struct {
	Token    string
	Deadline time.Time
}

type pendingReplyContinuation struct {
	episodeID     string
	interactionID string
	subjectID     string
	traceID       string
	wakeup        Wakeup
	after         behavior.ActionPhase
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
		episodes:  episode.NewTracker(),
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
		if semanticEvent.Kind == event.UserReplied {
			statuses, err := e.acceptUserReply(ctx, &result, semanticEvent)
			result.ActionStatuses = append(result.ActionStatuses, statuses...)
			if err != nil {
				return result, err
			}
			continue
		}

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
		phases, err := behavior.CompileUserReplyPhases(plan)
		if err != nil {
			return result, fmt.Errorf("compile reply phases for plan %s: %w", plan.ID, err)
		}
		if e.pendingReply != nil {
			return result, fault.New(fault.PolicyBlocked, "start reply wait", errors.New("another reply continuation is pending"))
		}
		episodeID := "episode:" + outcome.InteractionID
		if err := e.episodes.Start(episode.Episode{
			ID:             episodeID,
			InteractionID:  outcome.InteractionID,
			SubjectID:      semanticEvent.SubjectID,
			DecisionID:     outcome.ID,
			TriggerEventID: semanticEvent.ID,
			TraceID:        outcome.TraceID,
			ConfigHash:     outcome.ConfigHash,
			StartedAt:      now,
		}); err != nil {
			return result, fmt.Errorf("start episode for decision %s: %w", outcome.ID, err)
		}
		result.Plans = append(result.Plans, plan)
		e.bestEffortAudit(&result, func(auditCtx context.Context) error {
			return e.recorder.RecordPlan(auditCtx, plan)
		})

		statuses, err := e.dispatchCommands(ctx, phases.Before.Commands(e.clock.Now()))
		result.ActionStatuses = append(result.ActionStatuses, statuses...)
		if err != nil {
			// A P0 cancellation deliberately leaves the Episode active so Runner can
			// join this work and then commit the correlated rejection Outcome.
			if ctx.Err() == nil {
				if _, abortErr := e.episodes.AbortUndelivered(episodeID); abortErr != nil {
					return result, errors.Join(err, fmt.Errorf("abort undelivered episode %s: %w", episodeID, abortErr))
				}
			}
			return result, err
		}
		openedAt := e.clock.Now()
		wakeup := Wakeup{
			Token:    "wakeup:" + episodeID + ":user.reply",
			Deadline: openedAt.Add(phases.WaitFor),
		}
		if err := e.episodes.OpenResponseWindow(episodeID, wakeup.Token, openedAt, phases.WaitFor); err != nil {
			windowErr := fmt.Errorf("open reply window for episode %s: %w", episodeID, err)
			if _, abortErr := e.episodes.AbortUndelivered(episodeID); abortErr != nil {
				return result, errors.Join(windowErr, fmt.Errorf("abort undelivered episode %s: %w", episodeID, abortErr))
			}
			return result, windowErr
		}
		e.pendingReply = &pendingReplyContinuation{
			episodeID:     episodeID,
			interactionID: outcome.InteractionID,
			subjectID:     semanticEvent.SubjectID,
			traceID:       outcome.TraceID,
			wakeup:        wakeup,
			after:         phases.After,
		}
		e.replyWindow.publish(ReplyAcceptanceWindow{
			SubjectID: semanticEvent.SubjectID,
			OpenedAt:  openedAt,
			Deadline:  wakeup.Deadline,
		})
	}
	return result, nil
}

// NextWakeup returns the current application deadline without exposing its
// reply-window semantics to the runtime scheduler.
func (e *Engine) NextWakeup() (Wakeup, bool) {
	if e.pendingReply == nil {
		return Wakeup{}, false
	}
	return e.pendingReply.wakeup, true
}

// AdvanceAt consumes the exact current wakeup after its deadline. It follows
// the same single-writer convention as Process and CommitUserRejection.
func (e *Engine) AdvanceAt(ctx context.Context, wakeup Wakeup) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, classifyContext("advance wakeup", err)
	}
	pending := e.pendingReply
	if pending == nil || pending.wakeup != wakeup || e.clock.Now().Before(wakeup.Deadline) {
		return Result{}, nil
	}

	expired, err := event.NewResponseWindowExpired(
		"evt:"+wakeup.Token+":"+string(event.ResponseWindowExpired),
		pending.episodeID,
		wakeup.Token,
		pending.subjectID,
		wakeup.Deadline,
		pending.traceID,
	)
	if err != nil {
		return Result{}, fmt.Errorf("construct response window expiration: %w", err)
	}
	// This is the expiration transaction's commit gate. Cancellation observed
	// here leaves the Episode, cooldown, and continuation untouched so a queued
	// P0 rejection can commit after Runner joins this work.
	if err := ctx.Err(); err != nil {
		return Result{}, classifyContext("advance wakeup", err)
	}
	outcome, applied, err := e.episodes.Expire(pending.episodeID, expired)
	if err != nil {
		return Result{}, fmt.Errorf("expire response window for episode %s: %w", pending.episodeID, err)
	}
	if !applied {
		return Result{}, nil
	}
	if outcome == nil {
		return Result{}, errors.New("expire response window returned no outcome")
	}
	if _, err := e.projector.ApplyNoResponse(expired, e.config.NoResponseCooldown); err != nil {
		return Result{}, fmt.Errorf("project response window expiration %s: %w", expired.ID, err)
	}
	e.pendingReply = nil
	e.replyWindow.clear()

	result := Result{
		Events:   []event.SemanticEvent{expired},
		Outcomes: []episode.Outcome{*outcome},
	}
	e.bestEffortAudit(&result, func(auditCtx context.Context) error {
		return e.recorder.RecordEvent(auditCtx, expired)
	})
	e.bestEffortAudit(&result, func(auditCtx context.Context) error {
		return e.recorder.RecordOutcome(auditCtx, *outcome)
	})
	statuses, err := e.dispatchCommands(ctx, pending.after.Commands(e.clock.Now()))
	result.ActionStatuses = append(result.ActionStatuses, statuses...)
	if err != nil {
		return result, fmt.Errorf("dispatch expired reply continuation %s: %w", pending.interactionID, err)
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

func (e *Engine) dispatchCommands(ctx context.Context, commands []behavior.ActionCommand) ([]behavior.ActionStatus, error) {
	var statuses []behavior.ActionStatus
	for _, command := range commands {
		commandCtx, cancel := context.WithTimeout(ctx, command.Action.Timeout)
		stream, err := e.driver.Execute(commandCtx, command)
		if err != nil {
			cancel()
			return statuses, fault.New(fault.AdapterRejected, "execute action "+command.ID, err)
		}
		if stream == nil {
			cancel()
			return statuses, fault.New(fault.Unavailable, "await action "+command.ID, errors.New("adapter returned a nil status stream"))
		}

		var terminal behavior.ActionState
		malformedAfterTerminal := false
		for {
			select {
			case <-commandCtx.Done():
				cancel()
				return statuses, classifyContext("await action "+command.ID, commandCtx.Err())
			case status, ok := <-stream:
				if !ok {
					cancel()
					switch {
					case terminal == "":
						return statuses, fault.New(fault.Unavailable, "await action "+command.ID, errors.New("adapter status stream closed without a terminal state"))
					case malformedAfterTerminal:
						return statuses, fault.New(fault.AdapterRejected, "await action "+command.ID, errors.New("adapter emitted status after a terminal state"))
					case terminal != behavior.ActionCompleted:
						return statuses, fault.New(fault.AdapterRejected, "await action "+command.ID, fmt.Errorf("adapter ended action with terminal state %s", terminal))
					default:
						goto nextCommand
					}
				}
				statuses = append(statuses, status)
				e.bestEffortAuditStatus(statuses, status)
				if terminal != "" {
					malformedAfterTerminal = true
					continue
				}
				if isTerminalActionState(status.State) {
					terminal = status.State
				}
			}
		}
	nextCommand:
	}
	return statuses, nil
}

func isTerminalActionState(state behavior.ActionState) bool {
	switch state {
	case behavior.ActionCompleted,
		behavior.ActionFailed,
		behavior.ActionRejected,
		behavior.ActionTimedOut,
		behavior.ActionCancelled:
		return true
	default:
		return false
	}
}

func (e *Engine) acceptUserReply(ctx context.Context, result *Result, input event.SemanticEvent) ([]behavior.ActionStatus, error) {
	pending := e.pendingReply
	if pending == nil || pending.subjectID != input.SubjectID {
		return nil, nil
	}
	outcome, _, err := e.episodes.Accept(pending.episodeID, input)
	if err != nil {
		return nil, fmt.Errorf("accept user reply event %s: %w", input.ID, err)
	}
	if outcome == nil {
		return nil, nil
	}
	e.pendingReply = nil
	e.replyWindow.clear()
	result.Outcomes = append(result.Outcomes, *outcome)
	e.bestEffortAudit(result, func(auditCtx context.Context) error {
		return e.recorder.RecordOutcome(auditCtx, *outcome)
	})
	statuses, err := e.dispatchCommands(ctx, pending.after.Commands(e.clock.Now()))
	if err != nil {
		return statuses, fmt.Errorf("dispatch accepted reply continuation %s: %w", pending.interactionID, err)
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
func (e *Engine) StopAll(ctx context.Context, command control.Command) error {
	if err := command.Validate(); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, e.config.ExternalCallTimeout)
	defer cancel()
	if err := e.driver.StopAll(callCtx, command.Reason); err != nil {
		return fault.New(fault.Unavailable, "stop all actions", err)
	}
	return nil
}

// CommitUserRejection records explicit user feedback after runtime cancellation
// has stopped and joined the active observation path. It is a single-writer
// operation and must not run concurrently with Process.
func (e *Engine) CommitUserRejection(ctx context.Context, command control.Command) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if command.Reason != control.ReasonUserRejected {
		return fault.New(fault.InvalidInput, "commit user rejection", fmt.Errorf("stop reason %q is not USER_REJECTED", command.Reason))
	}
	if command.SubjectID != e.config.SubjectID {
		return fault.New(
			fault.InvalidInput,
			"commit user rejection",
			fmt.Errorf("subject %s does not match engine subject %s", command.SubjectID, e.config.SubjectID),
		)
	}
	if err := ctx.Err(); err != nil {
		return classifyContext("commit user rejection", err)
	}

	feedback, err := event.NewUserRejected(
		fmt.Sprintf("evt:%s:%s", command.ID, event.UserRejected),
		command.SubjectID,
		command.OccurredAt,
		command.TraceID,
	)
	if err != nil {
		return fmt.Errorf("construct rejection event for control %s: %w", command.ID, err)
	}
	outcome, applied, err := e.episodes.Reject(feedback)
	if err != nil {
		return fmt.Errorf("apply rejection event %s: %w", feedback.ID, err)
	}
	if !applied {
		return nil
	}
	if _, err := e.projector.ApplyUserRejection(feedback, e.config.RejectionCooldown); err != nil {
		return fmt.Errorf("project rejection event %s: %w", feedback.ID, err)
	}
	if outcome != nil && e.pendingReply != nil && outcome.InteractionID == e.pendingReply.interactionID {
		e.pendingReply = nil
		e.replyWindow.clear()
	}
	e.bestEffortControlAudit(func(auditCtx context.Context) error {
		return e.recorder.RecordEvent(auditCtx, feedback)
	})
	if outcome != nil {
		e.bestEffortControlAudit(func(auditCtx context.Context) error {
			return e.recorder.RecordOutcome(auditCtx, *outcome)
		})
	}
	return nil
}

func (e *Engine) bestEffortControlAudit(record func(context.Context) error) {
	auditCtx, cancel := context.WithTimeout(context.Background(), e.config.ExternalCallTimeout)
	defer cancel()
	_ = record(auditCtx)
}
