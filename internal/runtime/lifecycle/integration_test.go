package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/episode"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestEngineRunnerAutomaticallyExpiresResponseWindow(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	baseClock := engineclock.NewFake(startedAt)
	clock := &timerSignalingClock{Fake: baseClock, armed: make(chan time.Duration, 2)}
	baseDriver := fakeembodiment.NewDriver(integrationCapabilities(), clock)
	driver := &returnIdleSignalingDriver{delegate: baseDriver, returnedIdle: make(chan struct{}, 1)}
	audit := &memorystorage.AuditRecorder{}
	core, err := application.New(application.Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome.v1",
		ConfigHash:             "config.integration.v1",
		RandomSeed:             1,
	}, driver, audit, clock)
	if err != nil {
		t.Fatalf("application.New() error = %v", err)
	}
	runner, err := New(DefaultConfig(), core, clock)
	if err != nil {
		t.Fatalf("lifecycle.New() error = %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()
	var (
		joinOnce sync.Once
		runErr   error
	)
	stopAndJoin := func() error {
		joinOnce.Do(func() {
			cancelRun()
			select {
			case runErr = <-runDone:
			case <-time.After(time.Second):
				runErr = errors.New("runner did not stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() {
		if err := stopAndJoin(); !errors.Is(err, context.Canceled) {
			t.Errorf("Run() cleanup error = %v, want context.Canceled", err)
		}
	})

	if _, err := runner.SubmitObservation(context.Background(), integrationPresence("left", 1, startedAt, false)); err != nil {
		t.Fatalf("SubmitObservation(left) error = %v", err)
	}
	baseClock.Advance(45 * time.Minute)
	if _, err := runner.SubmitObservation(context.Background(), integrationPresence("returned", 2, baseClock.Now(), true)); err != nil {
		t.Fatalf("SubmitObservation(returned) error = %v", err)
	}
	select {
	case duration := <-clock.armed:
		if duration != 8*time.Second {
			t.Fatalf("armed timer = %s, want 8s", duration)
		}
	case <-time.After(time.Second):
		t.Fatal("response-window timer was not armed")
	}

	baseClock.Advance(8 * time.Second)
	waitSignal(t, driver.returnedIdle, "RETURN_IDLE was not dispatched after expiration")
	if err := stopAndJoin(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	if got := audit.Snapshot(); len(got.Outcomes) != 1 || got.Outcomes[0].Kind != episode.NoResponse {
		t.Fatalf("Audit Outcomes = %#v, want NO_RESPONSE", got.Outcomes)
	}
	snapshot := core.Snapshot()
	deadline := startedAt.Add(45*time.Minute + 8*time.Second)
	if snapshot.NoResponseCooldownStartedAt != deadline || snapshot.NoResponseCooldownUntil != deadline.Add(5*time.Minute) {
		t.Fatalf("Snapshot = %#v, want 5m no-response cooldown", snapshot)
	}
	commands := baseDriver.Commands()
	if len(commands) != 4 || commands[3].Action.Type != behavior.ReturnIdle {
		t.Fatalf("Commands = %#v, want fourth RETURN_IDLE", commands)
	}
	if wakeup, ok := core.NextWakeup(); ok {
		t.Fatalf("NextWakeup() = %#v, want none", wakeup)
	}
}

type timerSignalingClock struct {
	*engineclock.Fake
	armed chan time.Duration
}

func (c *timerSignalingClock) NewTimer(duration time.Duration) port.Timer {
	timer := c.Fake.NewTimer(duration)
	c.armed <- duration
	return timer
}

type returnIdleSignalingDriver struct {
	delegate     *fakeembodiment.Driver
	returnedIdle chan struct{}
}

func (d *returnIdleSignalingDriver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	return d.delegate.Capabilities(ctx)
}

func (d *returnIdleSignalingDriver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	stream, err := d.delegate.Execute(ctx, command)
	if err == nil && command.Action.Type == behavior.ReturnIdle {
		d.returnedIdle <- struct{}{}
	}
	return stream, err
}

func (d *returnIdleSignalingDriver) StopAll(ctx context.Context, reason control.StopReason) error {
	return d.delegate.StopAll(ctx, reason)
}

func integrationCapabilities() behavior.Capabilities {
	return behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}
}

func integrationPresence(id string, sequence uint64, occurredAt time.Time, present bool) observation.Observation {
	payload := observation.PersonPresence{Present: present}
	return observation.Observation{
		ID:             id,
		SourceID:       "integration",
		SourceSeq:      sequence,
		OccurredAt:     occurredAt,
		TTL:            time.Minute,
		SubjectID:      "user-1",
		Confidence:     0.99,
		TraceID:        "trace-integration",
		PersonPresence: &payload,
	}
}
