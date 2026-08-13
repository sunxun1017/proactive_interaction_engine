package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestPrePhaseFailureTerminalsStopDispatchAndDoNotOpenReplyWindow(t *testing.T) {
	for _, terminal := range []behavior.ActionState{
		behavior.ActionFailed,
		behavior.ActionRejected,
		behavior.ActionTimedOut,
		behavior.ActionCancelled,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			states := []behavior.ActionState{
				behavior.ActionDispatched,
				behavior.ActionAccepted,
				behavior.ActionStarted,
				terminal,
			}
			core, clock, driver, audit := newActionTerminalTestEngine(t, states)
			processOK(t, core, presenceObservation("left-terminal-"+string(terminal), 1, clock.Now(), false))
			clock.Advance(45 * time.Minute)

			result, err := core.Process(context.Background(), presenceObservation("returned-terminal-"+string(terminal), 2, clock.Now(), true))
			if !fault.IsCode(err, fault.AdapterRejected) {
				t.Fatalf("Process() error = %v, want AdapterRejected", err)
			}
			assertTerminalFailureState(t, core, driver, audit, result, states)
		})
	}
}

func TestPrePhaseStreamClosedWithoutTerminalIsUnavailable(t *testing.T) {
	states := []behavior.ActionState{
		behavior.ActionDispatched,
		behavior.ActionAccepted,
		behavior.ActionStarted,
	}
	core, clock, driver, audit := newActionTerminalTestEngine(t, states)
	processOK(t, core, presenceObservation("left-missing-terminal", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)

	result, err := core.Process(context.Background(), presenceObservation("returned-missing-terminal", 2, clock.Now(), true))
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Process() error = %v, want Unavailable", err)
	}
	assertTerminalFailureState(t, core, driver, audit, result, states)
}

func TestPrePhaseStreamRejectsStateAfterTerminal(t *testing.T) {
	for name, states := range map[string][]behavior.ActionState{
		"nonterminal after terminal": {
			behavior.ActionDispatched,
			behavior.ActionAccepted,
			behavior.ActionStarted,
			behavior.ActionCompleted,
			behavior.ActionDispatched,
		},
		"second terminal": {
			behavior.ActionDispatched,
			behavior.ActionAccepted,
			behavior.ActionStarted,
			behavior.ActionCompleted,
			behavior.ActionFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			core, clock, driver, audit := newActionTerminalTestEngine(t, states)
			processOK(t, core, presenceObservation("left-malformed-"+name, 1, clock.Now(), false))
			clock.Advance(45 * time.Minute)

			result, err := core.Process(context.Background(), presenceObservation("returned-malformed-"+name, 2, clock.Now(), true))
			if !fault.IsCode(err, fault.AdapterRejected) {
				t.Fatalf("Process() error = %v, want AdapterRejected", err)
			}
			assertTerminalFailureState(t, core, driver, audit, result, states)
		})
	}
}

func TestCompletedPrePhaseStreamsStillOpenReplyWindow(t *testing.T) {
	states := []behavior.ActionState{
		behavior.ActionDispatched,
		behavior.ActionAccepted,
		behavior.ActionStarted,
		behavior.ActionCompleted,
	}
	core, clock, driver, audit := newActionTerminalTestEngine(t, states)
	processOK(t, core, presenceObservation("left-completed-terminal", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)

	result, err := core.Process(context.Background(), presenceObservation("returned-completed-terminal", 2, clock.Now(), true))
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(driver.commands) != 3 {
		t.Fatalf("Execute() commands = %#v, want all three pre-phase commands", driver.commands)
	}
	if len(result.ActionStatuses) != 12 || !reflect.DeepEqual(result.ActionStatuses, audit.Snapshot().Statuses) {
		t.Fatalf("result statuses = %#v, audit statuses = %#v", result.ActionStatuses, audit.Snapshot().Statuses)
	}
	if _, open := core.CurrentReplyAcceptanceWindow(); !open {
		t.Fatal("CurrentReplyAcceptanceWindow() = none after completed pre-phase")
	}
	if _, ok := core.NextWakeup(); !ok {
		t.Fatal("NextWakeup() = none after completed pre-phase")
	}
}

func assertTerminalFailureState(
	t *testing.T,
	core *Engine,
	driver *scriptedStatusDriver,
	audit *memorystorage.AuditRecorder,
	result Result,
	wantStates []behavior.ActionState,
) {
	t.Helper()
	if len(driver.commands) != 1 {
		t.Fatalf("Execute() commands = %#v, want only failed first command", driver.commands)
	}
	wantStatuses := statusesForCommand(driver.commands[0], wantStates, driver.clock.Now())
	if !reflect.DeepEqual(result.ActionStatuses, wantStatuses) {
		t.Fatalf("Result.ActionStatuses = %#v, want %#v", result.ActionStatuses, wantStatuses)
	}
	if got := audit.Snapshot().Statuses; !reflect.DeepEqual(got, wantStatuses) {
		t.Fatalf("audited statuses = %#v, want %#v", got, wantStatuses)
	}
	if window, open := core.CurrentReplyAcceptanceWindow(); open {
		t.Fatalf("CurrentReplyAcceptanceWindow() = %#v, want none", window)
	}
	if wakeup, ok := core.NextWakeup(); ok {
		t.Fatalf("NextWakeup() = %#v, want none", wakeup)
	}
}

func newActionTerminalTestEngine(
	t *testing.T,
	firstStates []behavior.ActionState,
) (*Engine, *engineclock.Fake, *scriptedStatusDriver, *memorystorage.AuditRecorder) {
	t.Helper()
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	driver := &scriptedStatusDriver{clock: clock, firstStates: append([]behavior.ActionState(nil), firstStates...)}
	audit := &memorystorage.AuditRecorder{}
	core, err := New(validTestConfig(), driver, audit, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return core, clock, driver, audit
}

type scriptedStatusDriver struct {
	clock       *engineclock.Fake
	firstStates []behavior.ActionState
	commands    []behavior.ActionCommand
}

func (d *scriptedStatusDriver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return desktopTestCapabilities(), nil
}

func (d *scriptedStatusDriver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.commands = append(d.commands, command)
	states := d.firstStates
	if len(d.commands) > 1 {
		states = []behavior.ActionState{
			behavior.ActionDispatched,
			behavior.ActionAccepted,
			behavior.ActionStarted,
			behavior.ActionCompleted,
		}
	}
	statuses := statusesForCommand(command, states, d.clock.Now())
	stream := make(chan behavior.ActionStatus, len(statuses))
	for _, status := range statuses {
		stream <- status
	}
	close(stream)
	return stream, nil
}

func (d *scriptedStatusDriver) StopAll(context.Context, control.StopReason) error { return nil }

func statusesForCommand(command behavior.ActionCommand, states []behavior.ActionState, occurredAt time.Time) []behavior.ActionStatus {
	statuses := make([]behavior.ActionStatus, 0, len(states))
	for _, state := range states {
		statuses = append(statuses, behavior.ActionStatus{
			ActionID:      command.ID,
			InteractionID: command.InteractionID,
			TraceID:       command.TraceID,
			State:         state,
			OccurredAt:    occurredAt,
			Reason:        "scripted-" + string(state),
		})
	}
	return statuses
}
