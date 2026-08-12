package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/episode"
	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestExpirationAuditFailuresOnlyWarnAndDoNotRollback(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	driver := fakeembodiment.NewDriver(desktopTestCapabilities(), clock)
	audit := &expirationFailingRecorder{delegate: &memorystorage.AuditRecorder{}}
	core := newEngineWithDependencies(t, driver, audit, clock)
	wakeup := prepareExpiration(t, core, clock)

	result, err := core.AdvanceAt(context.Background(), wakeup)
	if err != nil {
		t.Fatalf("AdvanceAt() error = %v", err)
	}
	if len(result.Warnings) != 2 {
		t.Fatalf("Warnings = %#v, want event and outcome audit warnings", result.Warnings)
	}
	assertExpirationCommitted(t, core, wakeup, result)
	commands := driver.Commands()
	if len(commands) != 4 || commands[3].Action.Type != behavior.ReturnIdle {
		t.Fatalf("Commands = %#v, want RETURN_IDLE after committed expiration", commands)
	}
}

func TestReturnIdleFailureDoesNotRollbackExpiration(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	baseDriver := fakeembodiment.NewDriver(desktopTestCapabilities(), clock)
	driver := &returnIdleFailingDriver{delegate: baseDriver}
	audit := &memorystorage.AuditRecorder{}
	core := newEngineWithDependencies(t, driver, audit, clock)
	wakeup := prepareExpiration(t, core, clock)

	result, err := core.AdvanceAt(context.Background(), wakeup)
	if !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("AdvanceAt() error = %v, want AdapterRejected", err)
	}
	assertExpirationCommitted(t, core, wakeup, result)
	if got := audit.Snapshot(); len(got.Events) != 3 || got.Events[2].Kind != event.ResponseWindowExpired || len(got.Outcomes) != 1 || got.Outcomes[0].Kind != episode.NoResponse {
		t.Fatalf("Audit = %#v, want committed expiration and outcome", got)
	}
	if commands := baseDriver.Commands(); len(commands) != 3 {
		t.Fatalf("successful Commands = %#v, want only pre-wait actions", commands)
	}
}

func prepareExpiration(t *testing.T, core *Engine, clock *engineclock.Fake) Wakeup {
	t.Helper()
	processOK(t, core, presenceObservation("left-fault", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, core, presenceObservation("returned-fault", 2, clock.Now(), true))
	wakeup, ok := core.NextWakeup()
	if !ok {
		t.Fatal("NextWakeup() = none")
	}
	clock.Advance(wakeup.Deadline.Sub(clock.Now()))
	return wakeup
}

func assertExpirationCommitted(t *testing.T, core *Engine, wakeup Wakeup, result Result) {
	t.Helper()
	if len(result.Events) != 1 || result.Events[0].Kind != event.ResponseWindowExpired || len(result.Outcomes) != 1 || result.Outcomes[0].Kind != episode.NoResponse {
		t.Fatalf("Result = %#v, want expiration event and NO_RESPONSE", result)
	}
	snapshot := core.Snapshot()
	if snapshot.NoResponseCooldownStartedAt != wakeup.Deadline || snapshot.NoResponseCooldownUntil != wakeup.Deadline.Add(5*time.Minute) {
		t.Fatalf("Snapshot = %#v, want 5m no-response cooldown", snapshot)
	}
	if got, ok := core.NextWakeup(); ok {
		t.Fatalf("NextWakeup() = %#v, want none", got)
	}
}

func newEngineWithDependencies(t *testing.T, driver port.ActionDriver, recorder port.AuditRecorder, clock port.Clock) *Engine {
	t.Helper()
	core, err := New(validTestConfig(), driver, recorder, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return core
}

func desktopTestCapabilities() behavior.Capabilities {
	return behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}
}

type expirationFailingRecorder struct {
	delegate *memorystorage.AuditRecorder
}

func (r *expirationFailingRecorder) RecordEvent(ctx context.Context, value event.SemanticEvent) error {
	if value.Kind == event.ResponseWindowExpired {
		return errors.New("event audit unavailable")
	}
	return r.delegate.RecordEvent(ctx, value)
}

func (r *expirationFailingRecorder) RecordDecision(ctx context.Context, value decision.Decision) error {
	return r.delegate.RecordDecision(ctx, value)
}

func (r *expirationFailingRecorder) RecordPlan(ctx context.Context, value behavior.BehaviorPlan) error {
	return r.delegate.RecordPlan(ctx, value)
}

func (r *expirationFailingRecorder) RecordActionStatus(ctx context.Context, value behavior.ActionStatus) error {
	return r.delegate.RecordActionStatus(ctx, value)
}

func (r *expirationFailingRecorder) RecordOutcome(ctx context.Context, value episode.Outcome) error {
	if value.Kind == episode.NoResponse {
		return errors.New("outcome audit unavailable")
	}
	return r.delegate.RecordOutcome(ctx, value)
}

type returnIdleFailingDriver struct {
	delegate *fakeembodiment.Driver
}

func (d *returnIdleFailingDriver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	return d.delegate.Capabilities(ctx)
}

func (d *returnIdleFailingDriver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	if command.Action.Type == behavior.ReturnIdle {
		return nil, errors.New("return idle rejected")
	}
	return d.delegate.Execute(ctx, command)
}

func (d *returnIdleFailingDriver) StopAll(ctx context.Context, reason control.StopReason) error {
	return d.delegate.StopAll(ctx, reason)
}
