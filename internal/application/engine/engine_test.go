package engine

import (
	"context"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
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

func TestStopAllForwardsTypedReason(t *testing.T) {
	engine, _, driver := newTestEngine(t)
	command := control.Command{
		ID:         "control-1",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC),
		TraceID:    "trace-1",
	}
	if err := engine.StopAll(context.Background(), command); err != nil {
		t.Fatalf("StopAll() error = %v", err)
	}
	reasons := driver.StopReasons()
	if len(reasons) != 1 || reasons[0] != control.ReasonUserRejected {
		t.Fatalf("StopReasons() = %#v, want USER_REJECTED", reasons)
	}
}

func TestCommitUserRejectionRecordsOneOutcomeAndProjectsCooldown(t *testing.T) {
	engine, clock, _, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	start := clock.Now()
	processOK(t, engine, presenceObservation("left", 1, start, false))
	clock.Advance(45 * time.Minute)
	result := processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
	if len(result.Decisions) != 1 || result.Decisions[0].Kind != decision.GreetShort {
		t.Fatalf("Decisions = %#v, want active greeting", result.Decisions)
	}

	command := control.Command{
		ID:         "rejection-1",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: clock.Now(),
		TraceID:    "trace-rejection-1",
	}
	if err := engine.CommitUserRejection(context.Background(), command); err != nil {
		t.Fatalf("CommitUserRejection() error = %v", err)
	}
	if err := engine.CommitUserRejection(context.Background(), command); err != nil {
		t.Fatalf("CommitUserRejection(duplicate) error = %v", err)
	}

	auditSnapshot := audit.Snapshot()
	if len(auditSnapshot.Outcomes) != 1 {
		t.Fatalf("Outcomes = %#v, want one", auditSnapshot.Outcomes)
	}
	if got := engine.Snapshot().RejectionCooldownUntil; got != command.OccurredAt.Add(30*time.Minute) {
		t.Fatalf("RejectionCooldownUntil = %s", got)
	}
}

func TestCommitUserRejectionWithoutActiveEpisodeAuditsFactOnly(t *testing.T) {
	engine, clock, _, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	command := control.Command{
		ID:         "rejection-without-episode",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: clock.Now(),
		TraceID:    "trace-rejection-without-episode",
	}
	if err := engine.CommitUserRejection(context.Background(), command); err != nil {
		t.Fatalf("CommitUserRejection() error = %v", err)
	}

	snapshot := audit.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Events[0].Kind != "USER_REJECTED" {
		t.Fatalf("Events = %#v, want one USER_REJECTED", snapshot.Events)
	}
	if len(snapshot.Outcomes) != 0 {
		t.Fatalf("Outcomes = %#v, want none", snapshot.Outcomes)
	}
	if got := engine.Snapshot().RejectionCooldownUntil; got != command.OccurredAt.Add(30*time.Minute) {
		t.Fatalf("RejectionCooldownUntil = %s", got)
	}
}

func TestCommitUserRejectionRejectsMismatchedSubjectWithoutSideEffects(t *testing.T) {
	engine, clock, _, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	before := engine.Snapshot()
	err := engine.CommitUserRejection(context.Background(), control.Command{
		ID:         "rejection-other-user",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-2",
		OccurredAt: clock.Now(),
		TraceID:    "trace-other-user",
	})
	if !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("CommitUserRejection() error = %v, want InvalidInput", err)
	}
	if after := engine.Snapshot(); after != before {
		t.Fatalf("Snapshot changed: before=%#v after=%#v", before, after)
	}
	snapshot := audit.Snapshot()
	if len(snapshot.Events) != 0 || len(snapshot.Outcomes) != 0 {
		t.Fatalf("Audit changed: %#v", snapshot)
	}
}

func TestRejectionCooldownBlocksUntilExclusiveDeadline(t *testing.T) {
	engine, clock, _, _ := newTestEngineWithDurations(t, 30*time.Minute, time.Minute)
	start := clock.Now()
	processOK(t, engine, presenceObservation("left-1", 1, start, false))
	clock.Advance(2 * time.Minute)
	processOK(t, engine, presenceObservation("returned-1", 2, clock.Now(), true))
	rejectedAt := clock.Now()
	if err := engine.CommitUserRejection(context.Background(), control.Command{
		ID:         "rejection-1",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: rejectedAt,
		TraceID:    "trace-rejection-1",
	}); err != nil {
		t.Fatalf("CommitUserRejection() error = %v", err)
	}

	clock.Advance(time.Minute)
	processOK(t, engine, presenceObservation("left-2", 3, clock.Now(), false))
	clock.Advance(time.Minute)
	inside := processOK(t, engine, presenceObservation("returned-2", 4, clock.Now(), true))
	if len(inside.Decisions) != 1 || inside.Decisions[0].Kind != decision.Silent || inside.Decisions[0].ReasonCodes[0] != decision.ReasonRecentRejection {
		t.Fatalf("inside cooldown Decisions = %#v", inside.Decisions)
	}

	clock.Advance(27 * time.Minute)
	processOK(t, engine, presenceObservation("left-3", 5, clock.Now(), false))
	clock.Advance(time.Minute)
	atDeadline := processOK(t, engine, presenceObservation("returned-3", 6, clock.Now(), true))
	if len(atDeadline.Decisions) != 1 || atDeadline.Decisions[0].Kind != decision.GreetShort {
		t.Fatalf("deadline Decisions = %#v, want GREET_SHORT", atDeadline.Decisions)
	}
}

func newTestEngine(t *testing.T) (*Engine, *engineclock.Fake, *fakeembodiment.Driver) {
	t.Helper()
	engine, clock, driver, _ := newTestEngineWithCooldown(t, 30*time.Minute)
	return engine, clock, driver
}

func newTestEngineWithCooldown(t *testing.T, cooldown time.Duration) (*Engine, *engineclock.Fake, *fakeembodiment.Driver, *memorystorage.AuditRecorder) {
	t.Helper()
	return newTestEngineWithDurations(t, cooldown, 30*time.Minute)
}

func newTestEngineWithDurations(t *testing.T, cooldown, returnThreshold time.Duration) (*Engine, *engineclock.Fake, *fakeembodiment.Driver, *memorystorage.AuditRecorder) {
	t.Helper()
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	driver := fakeembodiment.NewDriver(behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}, clock)
	audit := &memorystorage.AuditRecorder{}
	engine, err := New(Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: returnThreshold,
		RejectionCooldown:      cooldown,
		ActionTimeout:          time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome.v1",
		ConfigHash:             "config.test.v1",
		RandomSeed:             1,
	}, driver, audit, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return engine, clock, driver, audit
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
