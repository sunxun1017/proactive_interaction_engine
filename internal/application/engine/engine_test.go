package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/decision"
	"proactive-interaction-engine/internal/domain/episode"
	"proactive-interaction-engine/internal/domain/event"
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
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
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
	clock.Advance(time.Second)
	lateReply := processOK(t, engine, replyObservation("reply-after-rejection", 3, clock.Now()))
	if len(lateReply.Outcomes) != 0 || len(driver.Commands()) != 3 {
		t.Fatalf("reply after rejection outcomes=%#v commands=%#v", lateReply.Outcomes, driver.Commands())
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

func TestUserReplyDispatchesPostWaitPhaseAndRecordsAcceptedOutcome(t *testing.T) {
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	processOK(t, engine, presenceObservation("left", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))

	commands := driver.Commands()
	if got := commandActionTypes(commands); !equalCommandActionTypes(got, []behavior.ActionType{behavior.AttendUser, behavior.Acknowledge, behavior.Speak}) {
		t.Fatalf("commands before reply = %#v", got)
	}

	clock.Advance(4 * time.Second)
	result := processOK(t, engine, replyObservation("reply", 3, clock.Now()))
	if len(result.Events) != 1 || result.Events[0].Kind != event.UserReplied {
		t.Fatalf("Events = %#v, want USER_REPLIED", result.Events)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Kind != episode.Accepted {
		t.Fatalf("Outcomes = %#v, want ACCEPTED", result.Outcomes)
	}
	commands = driver.Commands()
	if got := commandActionTypes(commands); !equalCommandActionTypes(got, []behavior.ActionType{behavior.AttendUser, behavior.Acknowledge, behavior.Speak, behavior.ReturnIdle}) {
		t.Fatalf("commands after reply = %#v", got)
	}
	if len(audit.Snapshot().Outcomes) != 1 || audit.Snapshot().Outcomes[0].Kind != episode.Accepted {
		t.Fatalf("audited Outcomes = %#v", audit.Snapshot().Outcomes)
	}
}

func TestInvalidUserReplyDoesNotDispatchPostPhaseOrEndEpisode(t *testing.T) {
	engine, clock, driver, _ := newTestEngineWithCooldown(t, 30*time.Minute)
	processOK(t, engine, presenceObservation("left", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
	openedAt := clock.Now()

	early := processOK(t, engine, replyObservation("early", 3, openedAt.Add(-time.Nanosecond)))
	if len(early.Outcomes) != 0 || len(driver.Commands()) != 3 {
		t.Fatalf("early reply outcomes=%#v commands=%#v", early.Outcomes, driver.Commands())
	}

	clock.Advance(time.Second)
	valid := processOK(t, engine, replyObservation("valid", 4, clock.Now()))
	if len(valid.Outcomes) != 1 || valid.Outcomes[0].Kind != episode.Accepted {
		t.Fatalf("valid reply Outcomes = %#v, want ACCEPTED", valid.Outcomes)
	}
	if len(driver.Commands()) != 4 || driver.Commands()[3].Action.Type != behavior.ReturnIdle {
		t.Fatalf("commands after valid reply = %#v", driver.Commands())
	}
}

func TestUserReplyWithoutActiveEpisodeIsAuditedWithoutActionsOrOutcome(t *testing.T) {
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	result := processOK(t, engine, replyObservation("reply", 1, clock.Now()))
	if len(result.Events) != 1 || result.Events[0].Kind != event.UserReplied || len(result.Outcomes) != 0 {
		t.Fatalf("Result = %#v", result)
	}
	if len(driver.Commands()) != 0 || len(audit.Snapshot().Outcomes) != 0 {
		t.Fatalf("commands=%#v audit=%#v", driver.Commands(), audit.Snapshot())
	}
}

func TestConfigRequiresPositiveNoResponseCooldown(t *testing.T) {
	config := validTestConfig()
	config.NoResponseCooldown = 0
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want no-response cooldown failure")
	}
	config.NoResponseCooldown = -time.Minute
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want no-response cooldown failure")
	}
}

func TestWelcomeExposesOneWakeupClearedByReplyOrRejection(t *testing.T) {
	for _, completion := range []string{"reply", "rejection"} {
		t.Run(completion, func(t *testing.T) {
			engine, clock, _, _ := newTestEngineWithCooldown(t, 30*time.Minute)
			processOK(t, engine, presenceObservation("left", 1, clock.Now(), false))
			clock.Advance(45 * time.Minute)
			processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
			wakeup, ok := engine.NextWakeup()
			if !ok || wakeup.Token == "" || wakeup.Deadline != clock.Now().Add(8*time.Second) {
				t.Fatalf("NextWakeup() = %#v, %t", wakeup, ok)
			}
			if completion == "reply" {
				clock.Advance(time.Second)
				processOK(t, engine, replyObservation("reply", 3, clock.Now()))
			} else if err := engine.CommitUserRejection(context.Background(), rejectionControl("reject", clock.Now())); err != nil {
				t.Fatalf("CommitUserRejection() error = %v", err)
			}
			if got, ok := engine.NextWakeup(); ok {
				t.Fatalf("NextWakeup() = %#v, want none", got)
			}
		})
	}
}

func TestAdvanceAtIgnoresStaleAndEarlyWakeupThenExpiresAtDeadline(t *testing.T) {
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	processOK(t, engine, presenceObservation("left", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
	wakeup, ok := engine.NextWakeup()
	if !ok {
		t.Fatal("NextWakeup() = none")
	}
	beforeSnapshot := engine.Snapshot()
	beforeAudit := audit.Snapshot()
	for _, stale := range []Wakeup{
		{Token: wakeup.Token + ":stale", Deadline: wakeup.Deadline},
		{Token: wakeup.Token, Deadline: wakeup.Deadline.Add(time.Second)},
	} {
		if result, err := engine.AdvanceAt(context.Background(), stale); err != nil || len(result.Events) != 0 || len(result.Outcomes) != 0 {
			t.Fatalf("AdvanceAt(stale %#v) = %#v, %v", stale, result, err)
		}
		if engine.Snapshot() != beforeSnapshot || !reflect.DeepEqual(audit.Snapshot(), beforeAudit) || len(driver.Commands()) != 3 {
			t.Fatal("stale wakeup changed state, audit, or actions")
		}
	}

	if result, err := engine.AdvanceAt(context.Background(), wakeup); err != nil || len(result.Events) != 0 || len(result.Outcomes) != 0 {
		t.Fatalf("AdvanceAt(early) = %#v, %v", result, err)
	}
	if _, ok := engine.NextWakeup(); !ok {
		t.Fatal("early wakeup cleared pending deadline")
	}

	clock.Advance(8 * time.Second)
	result, err := engine.AdvanceAt(context.Background(), wakeup)
	if err != nil {
		t.Fatalf("AdvanceAt(deadline) error = %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].Kind != event.ResponseWindowExpired || result.Events[0].ID != "evt:"+wakeup.Token+":RESPONSE_WINDOW_EXPIRED" {
		t.Fatalf("Events = %#v", result.Events)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Kind != episode.NoResponse {
		t.Fatalf("Outcomes = %#v", result.Outcomes)
	}
	if got := engine.Snapshot(); got.NoResponseCooldownStartedAt != wakeup.Deadline || got.NoResponseCooldownUntil != wakeup.Deadline.Add(5*time.Minute) {
		t.Fatalf("Snapshot = %#v", got)
	}
	if len(driver.Commands()) != 4 || driver.Commands()[3].Action.Type != behavior.ReturnIdle {
		t.Fatalf("commands = %#v", driver.Commands())
	}
	if got := audit.Snapshot(); len(got.Events) != 3 || got.Events[2].Kind != event.ResponseWindowExpired || len(got.Outcomes) != 1 {
		t.Fatalf("Audit = %#v", got)
	}
	if _, ok := engine.NextWakeup(); ok {
		t.Fatal("completed wakeup remains pending")
	}
	duplicate, err := engine.AdvanceAt(context.Background(), wakeup)
	if err != nil || len(duplicate.Events) != 0 || len(duplicate.Outcomes) != 0 || len(driver.Commands()) != 4 {
		t.Fatalf("AdvanceAt(duplicate) = %#v, %v", duplicate, err)
	}
}

func TestAdvanceAtCancelledContextHasNoSideEffects(t *testing.T) {
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	processOK(t, engine, presenceObservation("left", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, engine, presenceObservation("returned", 2, clock.Now(), true))
	wakeup, _ := engine.NextWakeup()
	clock.Advance(8 * time.Second)
	beforeState, beforeAudit := engine.Snapshot(), audit.Snapshot()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.AdvanceAt(ctx, wakeup); err == nil {
		t.Fatal("AdvanceAt(cancelled) error = nil")
	}
	if engine.Snapshot() != beforeState || !reflect.DeepEqual(audit.Snapshot(), beforeAudit) || len(driver.Commands()) != 3 {
		t.Fatal("cancelled wakeup changed state, audit, or actions")
	}
	if got, ok := engine.NextWakeup(); !ok || got != wakeup {
		t.Fatalf("NextWakeup() = %#v, %t", got, ok)
	}
}

func TestAdvanceAtCancellationBeforeCommitKeepsWakeupPending(t *testing.T) {
	engine, clock, driver, audit := newTestEngineWithCooldown(t, 30*time.Minute)
	processOK(t, engine, presenceObservation("left-cancel-before-commit", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	processOK(t, engine, presenceObservation("returned-cancel-before-commit", 2, clock.Now(), true))
	wakeup, _ := engine.NextWakeup()
	clock.Advance(8 * time.Second)
	beforeState, beforeAudit := engine.Snapshot(), audit.Snapshot()
	ctx := newCancelAfterFirstErrContext()

	if _, err := engine.AdvanceAt(ctx, wakeup); err == nil {
		t.Fatal("AdvanceAt(cancelled before commit) error = nil")
	}
	if engine.Snapshot() != beforeState || !reflect.DeepEqual(audit.Snapshot(), beforeAudit) || len(driver.Commands()) != 3 {
		t.Fatal("cancellation before commit changed state, audit, or actions")
	}
	if got, ok := engine.NextWakeup(); !ok || got != wakeup {
		t.Fatalf("NextWakeup() = %#v, %t", got, ok)
	}
}

type cancelAfterFirstErrContext struct {
	context.Context
	cancel context.CancelFunc
	first  bool
}

func newCancelAfterFirstErrContext() *cancelAfterFirstErrContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelAfterFirstErrContext{Context: ctx, cancel: cancel, first: true}
}

func (c *cancelAfterFirstErrContext) Err() error {
	err := c.Context.Err()
	if c.first {
		c.first = false
		c.cancel()
	}
	return err
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
		NoResponseCooldown:     5 * time.Minute,
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

func validTestConfig() Config {
	return Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome.v1",
		ConfigHash:             "config.test.v1",
		RandomSeed:             1,
	}
}

func rejectionControl(id string, at time.Time) control.Command {
	return control.Command{
		ID: id, Kind: control.StopAll, Reason: control.ReasonUserRejected,
		SubjectID: "user-1", OccurredAt: at, TraceID: "trace-" + id,
	}
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

func replyObservation(id string, seq uint64, at time.Time) observation.Observation {
	payload := observation.UserReply{}
	return observation.Observation{
		ID:         id,
		SourceID:   "test",
		SourceSeq:  seq,
		OccurredAt: at,
		TTL:        time.Minute,
		SubjectID:  "user-1",
		Confidence: 1,
		TraceID:    "trace-" + id,
		UserReply:  &payload,
	}
}

func commandActionTypes(commands []behavior.ActionCommand) []behavior.ActionType {
	output := make([]behavior.ActionType, 0, len(commands))
	for _, command := range commands {
		output = append(output, command.Action.Type)
	}
	return output
}

func equalCommandActionTypes(left, right []behavior.ActionType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
