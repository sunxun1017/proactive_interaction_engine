package webavatar

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

var _ port.ActionDriver = (*Driver)(nil)

func TestNewValidatesDependencies(t *testing.T) {
	now := time.Date(2026, time.August, 14, 9, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	realizer := &recordingRealizer{utterance: port.Utterance{Text: "hello"}}
	var nilRealizer *recordingRealizer
	var nilSpeaker *recordingSpeaker
	var nilClock *engineclock.Fake

	for _, test := range []struct {
		name     string
		realizer port.TextRealizer
		speaker  Speaker
		clock    port.Clock
	}{
		{name: "nil realizer", clock: clock},
		{name: "typed nil realizer", realizer: nilRealizer, clock: clock},
		{name: "nil clock", realizer: realizer},
		{name: "typed nil clock", realizer: realizer, clock: nilClock},
		{name: "typed nil optional speaker", realizer: realizer, speaker: nilSpeaker, clock: clock},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver, err := New(test.realizer, test.speaker, test.clock)
			if err == nil || driver != nil {
				t.Fatalf("New() = %#v, %v, want nil and error", driver, err)
			}
		})
	}
}

func TestCapabilitiesMatchAvailableOutputsAndAreDefensive(t *testing.T) {
	for _, test := range []struct {
		name      string
		speaker   Speaker
		wantSpeak bool
	}{
		{name: "visual only"},
		{name: "visual and speech", speaker: &recordingSpeaker{}, wantSpeak: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver, _, _ := newTestDriver(t, test.speaker)
			capabilities, err := driver.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities() error = %v", err)
			}
			for _, action := range []behavior.ActionType{
				behavior.AttendUser, behavior.Acknowledge, behavior.Express, behavior.ReturnIdle,
			} {
				if capability := capabilities[action]; !capability.Supported || !capability.Interruptible {
					t.Errorf("Capabilities()[%s] = %#v, want supported and interruptible", action, capability)
				}
			}
			if capability := capabilities[behavior.Speak]; capability.Supported != test.wantSpeak || capability.Interruptible != test.wantSpeak {
				t.Errorf("Capabilities()[SPEAK] = %#v, want supported=%t interruptible=%t", capability, test.wantSpeak, test.wantSpeak)
			}

			capabilities[behavior.AttendUser] = behavior.Capability{}
			again, err := driver.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities() second error = %v", err)
			}
			if !again[behavior.AttendUser].Supported {
				t.Fatal("mutating returned capabilities changed driver state")
			}
		})
	}
}

func TestVisualActionsPublishTypedUpdatesAndComplete(t *testing.T) {
	for _, test := range []struct {
		action behavior.ActionType
		mode   Mode
	}{
		{action: behavior.AttendUser, mode: ModeAttending},
		{action: behavior.Acknowledge, mode: ModeAcknowledging},
		{action: behavior.Express, mode: ModeExpressing},
		{action: behavior.ReturnIdle, mode: ModeIdle},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			driver, clock, _ := newTestDriver(t, nil)
			command := testCommand(clock, "visual-"+string(test.action), test.action)
			statuses := executeAndCollect(t, driver, context.Background(), command)
			assertStates(t, statuses,
				behavior.ActionDispatched,
				behavior.ActionAccepted,
				behavior.ActionStarted,
				behavior.ActionCompleted,
			)
			for _, status := range statuses {
				if status.ActionID != command.ID || status.InteractionID != command.InteractionID || status.TraceID != command.TraceID || status.OccurredAt != clock.Now() {
					t.Fatalf("status = %#v, want command correlations and clock timestamp", status)
				}
			}
			update := driver.Current()
			if update.Revision == 0 || update.Mode != test.mode || update.ActionID != command.ID || update.OccurredAt != clock.Now() || update.Speaking {
				t.Fatalf("Current() = %#v, want mode %s for action", update, test.mode)
			}
			if update.Text != "" {
				t.Fatalf("Current().Text = %q, want empty", update.Text)
			}
		})
	}
}

func TestSpeakRealizesExactTemplateAndRetainsCompletedText(t *testing.T) {
	speaker := &recordingSpeaker{}
	driver, clock, realizer := newTestDriver(t, speaker)
	command := testCommand(clock, "speak-success", behavior.Speak)
	statuses := executeAndCollect(t, driver, context.Background(), command)
	assertStates(t, statuses,
		behavior.ActionDispatched,
		behavior.ActionAccepted,
		behavior.ActionStarted,
		behavior.ActionCompleted,
	)
	wantRequest := port.UtteranceRequest{
		TemplateID:    command.Action.TemplateID,
		InteractionID: command.InteractionID,
		TraceID:       command.TraceID,
	}
	if got := realizer.snapshot(); !reflect.DeepEqual(got, []port.UtteranceRequest{wantRequest}) {
		t.Fatalf("realizer requests = %#v, want %#v", got, []port.UtteranceRequest{wantRequest})
	}
	if got := speaker.spokenTexts(); !reflect.DeepEqual(got, []string{"你回来啦。"}) {
		t.Fatalf("speaker texts = %#v, want exact realized text", got)
	}
	update := driver.Current()
	if update.Mode != ModeSpeaking || update.Text != "你回来啦。" || update.Speaking || update.ActionID != command.ID {
		t.Fatalf("Current() = %#v, want retained completed speech text", update)
	}

	idle := testCommand(clock, "return-idle", behavior.ReturnIdle)
	executeAndCollect(t, driver, context.Background(), idle)
	if update := driver.Current(); update.Mode != ModeIdle || update.Text != "" || update.Speaking {
		t.Fatalf("Current() after RETURN_IDLE = %#v, want cleared idle state", update)
	}
}

func TestSpeakFailuresEndFailedWithoutFakeSuccess(t *testing.T) {
	tests := []struct {
		name       string
		realizeErr error
		speakerErr error
		wantText   string
		wantMode   Mode
		wantSpeaks int
	}{
		{
			name:       "realizer failure leaves previous visual state",
			realizeErr: errors.New("template unavailable"),
			wantMode:   ModeIdle,
		},
		{
			name:       "speaker failure retains visual text",
			speakerErr: errors.New("speech dispatcher failed"),
			wantText:   "你回来啦。",
			wantMode:   ModeSpeaking,
			wantSpeaks: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			speaker := &recordingSpeaker{speakErr: test.speakerErr}
			driver, clock, realizer := newTestDriver(t, speaker)
			realizer.err = test.realizeErr
			command := testCommand(clock, "speak-failure", behavior.Speak)
			statuses := executeAndCollect(t, driver, context.Background(), command)
			assertStates(t, statuses,
				behavior.ActionDispatched,
				behavior.ActionAccepted,
				behavior.ActionStarted,
				behavior.ActionFailed,
			)
			if statuses[len(statuses)-1].Reason == "" {
				t.Fatal("FAILED status reason is empty")
			}
			for _, status := range statuses {
				if status.State == behavior.ActionCompleted {
					t.Fatal("failure stream contained fake COMPLETED status")
				}
			}
			if got := len(speaker.spokenTexts()); got != test.wantSpeaks {
				t.Fatalf("speaker calls = %d, want %d", got, test.wantSpeaks)
			}
			update := driver.Current()
			if update.Mode != test.wantMode || update.Text != test.wantText || update.Speaking {
				t.Fatalf("Current() = %#v, want mode=%s text=%q speaking=false", update, test.wantMode, test.wantText)
			}
		})
	}
}

func TestExecuteRejectsExpiredCommandWithoutUpdate(t *testing.T) {
	driver, clock, _ := newTestDriver(t, nil)
	before := driver.Current()
	command := testCommand(clock, "expired", behavior.AttendUser)
	command.Deadline = clock.Now().Add(-time.Nanosecond)
	stream, err := driver.Execute(context.Background(), command)
	if !fault.IsCode(err, fault.DeadlineExceeded) || stream != nil {
		t.Fatalf("Execute() = %#v, %v, want nil and DeadlineExceeded", stream, err)
	}
	if after := driver.Current(); after != before {
		t.Fatalf("Current() changed from %#v to %#v for expired command", before, after)
	}
}

func TestCompletedActionIDIsIdempotentAndConflictsFailClosed(t *testing.T) {
	speaker := &recordingSpeaker{}
	driver, clock, realizer := newTestDriver(t, speaker)
	command := testCommand(clock, "idempotent", behavior.Speak)
	first := executeAndCollect(t, driver, context.Background(), command)
	firstUpdate := driver.Current()
	second := executeAndCollect(t, driver, context.Background(), command)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("replayed statuses = %#v, want %#v", second, first)
	}
	if update := driver.Current(); update != firstUpdate {
		t.Fatalf("replay changed update from %#v to %#v", firstUpdate, update)
	}
	if got := len(realizer.snapshot()); got != 1 {
		t.Fatalf("realizer calls = %d, want 1", got)
	}
	if got := len(speaker.spokenTexts()); got != 1 {
		t.Fatalf("speaker calls = %d, want 1", got)
	}

	conflict := command
	conflict.Action.TemplateID = "different.template"
	stream, err := driver.Execute(context.Background(), conflict)
	if !fault.IsCode(err, fault.AdapterRejected) || stream != nil {
		t.Fatalf("conflicting Execute() = %#v, %v, want nil and AdapterRejected", stream, err)
	}
	if got := len(realizer.snapshot()); got != 1 {
		t.Fatalf("conflict repeated side effect, realizer calls = %d", got)
	}
}

func TestOnlyOneSpeakMayBeActive(t *testing.T) {
	speaker := newBlockingSpeaker()
	driver, clock, _ := newTestDriver(t, speaker)
	first := testCommand(clock, "active-speak", behavior.Speak)
	stream, err := driver.Execute(context.Background(), first)
	if err != nil {
		t.Fatalf("Execute(first) error = %v", err)
	}
	waitSignal(t, speaker.started, "first Speak did not start")
	second := testCommand(clock, "second-speak", behavior.Speak)
	secondStream, err := driver.Execute(context.Background(), second)
	if !fault.IsCode(err, fault.AdapterRejected) || secondStream != nil {
		t.Fatalf("Execute(second) = %#v, %v, want nil and AdapterRejected", secondStream, err)
	}
	close(speaker.release)
	assertTerminal(t, collectStatuses(t, stream), behavior.ActionCompleted)
	if got := speaker.spokenTexts(); len(got) != 1 {
		t.Fatalf("speaker calls = %#v, want only first action", got)
	}
}

func TestClosedSpeechStreamAllowsImmediateNextSpeak(t *testing.T) {
	speaker := newBlockingSpeaker()
	driver, clock, _ := newTestDriver(t, speaker)

	firstStream, err := driver.Execute(context.Background(), testCommand(clock, "first-completed-speak", behavior.Speak))
	if err != nil {
		t.Fatalf("Execute(first) error = %v", err)
	}
	waitSignal(t, speaker.started, "first Speak did not start")
	close(speaker.release)
	assertTerminal(t, collectStatuses(t, firstStream), behavior.ActionCompleted)

	secondStream, err := driver.Execute(context.Background(), testCommand(clock, "immediate-next-speak", behavior.Speak))
	if err != nil {
		t.Fatalf("Execute(second immediately after first stream closed) error = %v", err)
	}
	assertTerminal(t, collectStatuses(t, secondStream), behavior.ActionCompleted)
	if got := speaker.spokenTexts(); !reflect.DeepEqual(got, []string{"你回来啦。", "你回来啦。"}) {
		t.Fatalf("speaker texts = %#v, want both completed utterances", got)
	}
}

func TestStopAllCancelsJoinsSpeechAndReturnsStopFailureAfterCleanup(t *testing.T) {
	for _, test := range []struct {
		name    string
		stopErr error
	}{
		{name: "stop succeeds"},
		{name: "stop fails", stopErr: errors.New("speech stop failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			speaker := newBlockingSpeaker()
			speaker.stopErr = test.stopErr
			driver, clock, _ := newTestDriver(t, speaker)
			stream, err := driver.Execute(context.Background(), testCommand(clock, "stopped-speak", behavior.Speak))
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			waitSignal(t, speaker.started, "Speak did not start")
			err = driver.StopAll(context.Background(), control.ReasonUserRejected)
			if test.stopErr == nil && err != nil {
				t.Fatalf("StopAll() error = %v", err)
			}
			if test.stopErr != nil && !fault.IsCode(err, fault.Unavailable) {
				t.Fatalf("StopAll() error = %v, want Unavailable", err)
			}
			statuses := collectStatuses(t, stream)
			assertTerminal(t, statuses, behavior.ActionCancelled)
			if countTerminal(statuses) != 1 {
				t.Fatalf("statuses = %#v, want exactly one terminal", statuses)
			}
			if speaker.stopCount() != 1 {
				t.Fatalf("speaker Stop calls = %d, want 1", speaker.stopCount())
			}
			if update := driver.Current(); update.Mode != ModeIdle || update.Text != "" || update.Speaking {
				t.Fatalf("Current() after StopAll = %#v, want cleared idle", update)
			}
		})
	}
}

func TestSpeakParentCancellationAndDeadlineHaveDistinctTerminalStates(t *testing.T) {
	t.Run("parent cancellation", func(t *testing.T) {
		speaker := newBlockingSpeaker()
		driver, clock, _ := newTestDriver(t, speaker)
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := driver.Execute(ctx, testCommand(clock, "cancelled-speak", behavior.Speak))
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		waitSignal(t, speaker.started, "Speak did not start")
		cancel()
		assertTerminal(t, collectStatuses(t, stream), behavior.ActionCancelled)
	})

	t.Run("action deadline", func(t *testing.T) {
		speaker := newBlockingSpeaker()
		driver, clock, _ := newTestDriver(t, speaker)
		command := testCommand(clock, "timed-out-speak", behavior.Speak)
		command.Deadline = clock.Now().Add(5 * time.Second)
		stream, err := driver.Execute(context.Background(), command)
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		waitSignal(t, speaker.started, "Speak did not start")
		clock.Advance(5 * time.Second)
		assertTerminal(t, collectStatuses(t, stream), behavior.ActionTimedOut)
	})
}

func newTestDriver(t *testing.T, speaker Speaker) (*Driver, *engineclock.Fake, *recordingRealizer) {
	t.Helper()
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 9, 0, 0, 0, time.UTC))
	realizer := &recordingRealizer{utterance: port.Utterance{Text: "你回来啦。", UsedFallback: true, ModelVersion: "local.v1"}}
	driver, err := New(realizer, speaker, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return driver, clock, realizer
}

func testCommand(clock port.Clock, id string, action behavior.ActionType) behavior.ActionCommand {
	resource := behavior.Attention
	templateID := ""
	if action == behavior.Speak {
		resource = behavior.Voice
		templateID = "welcome.return"
	}
	return behavior.ActionCommand{
		ID:                 id,
		InteractionID:      "interaction-1",
		TraceID:            "trace-1",
		Deadline:           clock.Now().Add(time.Minute),
		Preemption:         behavior.PreemptInterruptible,
		RequiredCapability: action,
		Idempotency:        behavior.IdempotentByActionID,
		Action: behavior.ActionSpec{
			Type:       action,
			TemplateID: templateID,
			Resource:   resource,
			Timeout:    time.Minute,
		},
	}
}

func executeAndCollect(t *testing.T, driver *Driver, ctx context.Context, command behavior.ActionCommand) []behavior.ActionStatus {
	t.Helper()
	stream, err := driver.Execute(ctx, command)
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", command.ID, err)
	}
	if stream == nil {
		t.Fatalf("Execute(%s) returned nil stream", command.ID)
	}
	return collectStatuses(t, stream)
}

func collectStatuses(t *testing.T, stream <-chan behavior.ActionStatus) []behavior.ActionStatus {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	var statuses []behavior.ActionStatus
	for {
		select {
		case status, ok := <-stream:
			if !ok {
				return statuses
			}
			statuses = append(statuses, status)
		case <-timer.C:
			t.Fatal("status stream did not close")
		}
	}
}

func assertStates(t *testing.T, statuses []behavior.ActionStatus, want ...behavior.ActionState) {
	t.Helper()
	got := make([]behavior.ActionState, len(statuses))
	for index, status := range statuses {
		got[index] = status.State
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status states = %#v, want %#v", got, want)
	}
}

func assertTerminal(t *testing.T, statuses []behavior.ActionStatus, want behavior.ActionState) {
	t.Helper()
	if len(statuses) == 0 || statuses[len(statuses)-1].State != want {
		t.Fatalf("statuses = %#v, want terminal %s", statuses, want)
	}
	if statuses[len(statuses)-1].Reason == "" && want != behavior.ActionCompleted {
		t.Fatalf("terminal %s has empty reason", want)
	}
}

func countTerminal(statuses []behavior.ActionStatus) int {
	count := 0
	for _, status := range statuses {
		switch status.State {
		case behavior.ActionCompleted, behavior.ActionRejected, behavior.ActionCancelled, behavior.ActionTimedOut, behavior.ActionFailed:
			count++
		}
	}
	return count
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

type recordingRealizer struct {
	mu        sync.Mutex
	requests  []port.UtteranceRequest
	utterance port.Utterance
	err       error
}

func (r *recordingRealizer) Realize(ctx context.Context, request port.UtteranceRequest) (port.Utterance, error) {
	if err := ctx.Err(); err != nil {
		return port.Utterance{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	return r.utterance, r.err
}

func (r *recordingRealizer) snapshot() []port.UtteranceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]port.UtteranceRequest(nil), r.requests...)
}

type recordingSpeaker struct {
	mu       sync.Mutex
	texts    []string
	stops    int
	speakErr error
	stopErr  error
	started  chan struct{}
	release  chan struct{}
}

func newBlockingSpeaker() *recordingSpeaker {
	return &recordingSpeaker{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *recordingSpeaker) Speak(ctx context.Context, text string) error {
	s.mu.Lock()
	s.texts = append(s.texts, text)
	err := s.speakErr
	started := s.started
	release := s.release
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *recordingSpeaker) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return s.stopErr
}

func (s *recordingSpeaker) spokenTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

func (s *recordingSpeaker) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}
