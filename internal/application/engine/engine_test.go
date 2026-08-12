package engine

import (
	"context"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestReturnedUserProducesGreetingPlan(t *testing.T) {
	engine, clock, driver := newTestEngine(t)
	start := clock.Now()
	processOK(t, engine, presenceObservation("left", 1, start, false))
	clock.Advance(45 * time.Minute)

	result := processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
	if len(result.Decisions) != 1 || result.Decisions[0].Kind != decision.GreetShort {
		t.Fatalf("Decisions = %#v, want GREET_SHORT", result.Decisions)
	}
	if len(result.Plans) != 1 {
		t.Fatalf("Plans = %#v, want one plan", result.Plans)
	}
	if len(driver.Commands()) == 0 {
		t.Fatal("driver received no commands")
	}
}

func TestReturnedUserOnCallRemainsSilent(t *testing.T) {
	engine, clock, driver := newTestEngine(t)
	start := clock.Now()
	processOK(t, engine, presenceObservation("left", 1, start, false))
	clock.Advance(44 * time.Minute)
	processOK(t, engine, busyObservation("call", 2, clock.Now()))
	clock.Advance(time.Minute)

	result := processOK(t, engine, presenceObservation("returned", 3, clock.Now(), true))
	if len(result.Decisions) != 1 || result.Decisions[0].Kind != decision.Silent {
		t.Fatalf("Decisions = %#v, want SILENT", result.Decisions)
	}
	if len(result.Plans) != 0 || len(driver.Commands()) != 0 {
		t.Fatalf("silent decision emitted plan or commands: plans=%d commands=%d", len(result.Plans), len(driver.Commands()))
	}
}

func TestIngressRejectsDuplicateObservation(t *testing.T) {
	engine, clock, _ := newTestEngine(t)
	input := presenceObservation("same", 1, clock.Now(), false)
	processOK(t, engine, input)

	_, err := engine.Process(context.Background(), input)
	if !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Process(duplicate) error = %v, want StaleInput", err)
	}
}

func newTestEngine(t *testing.T) (*Engine, *engineclock.Fake, *fakeembodiment.Driver) {
	t.Helper()
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	driver := fakeembodiment.NewDriver(behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}, clock)
	engine, err := New(Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		ActionTimeout:          time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome.v1",
		ConfigHash:             "config.test.v1",
		RandomSeed:             1,
	}, driver, &memorystorage.AuditRecorder{}, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return engine, clock, driver
}

func processOK(t *testing.T, engine *Engine, input observation.Observation) Result {
	t.Helper()
	result, err := engine.Process(context.Background(), input)
	if err != nil {
		t.Fatalf("Process(%s) error = %v", input.ID, err)
	}
	return result
}

func presenceObservation(id string, seq uint64, at time.Time, present bool) observation.Observation {
	payload := observation.PersonPresence{Present: present}
	return observation.Observation{
		ID:             id,
		SourceID:       "test",
		SourceSeq:      seq,
		OccurredAt:     at,
		TTL:            time.Minute,
		SubjectID:      "user-1",
		Confidence:     1,
		TraceID:        "trace-test-1",
		PersonPresence: &payload,
	}
}

func busyObservation(id string, seq uint64, at time.Time) observation.Observation {
	payload := observation.UserBusy{Busy: true, Reason: observation.BusyOnCall}
	return observation.Observation{
		ID:         id,
		SourceID:   "test",
		SourceSeq:  seq,
		OccurredAt: at,
		TTL:        time.Minute,
		SubjectID:  "user-1",
		Confidence: 1,
		TraceID:    "trace-test-1",
		UserBusy:   &payload,
	}
}
