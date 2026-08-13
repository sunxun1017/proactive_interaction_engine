package webavatar

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
)

// Speaker renders one utterance and can stop the current utterance.
type Speaker interface {
	Speak(context.Context, string) error
	Stop(context.Context) error
}

// Mode is the stable visual state exposed to the loopback UI boundary.
type Mode string

const (
	ModeIdle          Mode = "IDLE"
	ModeAttending     Mode = "ATTENDING"
	ModeAcknowledging Mode = "ACKNOWLEDGING"
	ModeExpressing    Mode = "EXPRESSING"
	ModeSpeaking      Mode = "SPEAKING"
)

// Update is an immutable avatar snapshot.
type Update struct {
	Revision   uint64
	Mode       Mode
	Text       string
	Speaking   bool
	ActionID   string
	OccurredAt time.Time
}

var (
	errActionFinished = errors.New("action finished")
	errActionStopped  = errors.New("action stopped")
	errActionDeadline = errors.New("action deadline elapsed")
)

const (
	reasonRealizationFailed = "TEXT_REALIZATION_FAILED"
	reasonSpeechFailed      = "SPEECH_FAILED"
	reasonParentCancelled   = "CONTEXT_CANCELLED"
	reasonStopped           = "STOP_ALL"
	reasonDeadline          = "DEADLINE_EXCEEDED"
)

// Driver owns avatar state, action idempotency, and at most one active speech.
type Driver struct {
	mu sync.Mutex

	realizer port.TextRealizer
	speaker  Speaker
	clock    port.Clock

	capabilities behavior.Capabilities
	current      Update
	records      map[string]*actionRecord
	active       *activeSpeech
	subscribers  map[uint64]chan Update
	nextSubID    uint64
}

type actionRecord struct {
	command   behavior.ActionCommand
	statuses  []behavior.ActionStatus
	finalized bool
	stream    chan behavior.ActionStatus
}

type activeSpeech struct {
	record *actionRecord
	cancel context.CancelCauseFunc
	done   chan struct{}
}

type deterministicDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deterministicDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

// New constructs a visual-only or visual-and-speech driver.
func New(realizer port.TextRealizer, speaker Speaker, clock port.Clock) (*Driver, error) {
	const op = "create web avatar driver"
	if isNilDependency(realizer) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("text realizer is required"))
	}
	if isNilDependency(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("clock is required"))
	}
	if speaker != nil && isNilDependency(speaker) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("speaker must not be typed nil"))
	}

	capabilities := behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Express:     {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: speaker != nil, Interruptible: speaker != nil},
	}
	return &Driver{
		realizer:     realizer,
		speaker:      speaker,
		clock:        clock,
		capabilities: capabilities,
		current:      Update{Mode: ModeIdle, OccurredAt: clock.Now()},
		records:      make(map[string]*actionRecord),
		subscribers:  make(map[uint64]chan Update),
	}, nil
}

// Capabilities reports an immutable copy of supported abstract actions.
func (d *Driver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	if err := validateContext("read web avatar capabilities", ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	output := make(behavior.Capabilities, len(d.capabilities))
	for action, capability := range d.capabilities {
		output[action] = capability
	}
	return output, nil
}

// Execute validates and runs one abstract action. Visual actions complete
// synchronously; speech returns a buffered stream completed by an owned worker.
func (d *Driver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	const op = "execute web avatar action"
	if err := validateContext(op, ctx); err != nil {
		return nil, err
	}
	if err := validateCommand(command); err != nil {
		return nil, err
	}

	d.mu.Lock()
	if previous, exists := d.records[command.ID]; exists {
		if previous.command != command {
			d.mu.Unlock()
			return nil, fault.New(fault.AdapterRejected, op, fmt.Errorf("action id %s conflicts with its recorded command", command.ID))
		}
		if !previous.finalized {
			d.mu.Unlock()
			return nil, fault.New(fault.AdapterRejected, op, fmt.Errorf("action %s is still active", command.ID))
		}
		statuses := append([]behavior.ActionStatus(nil), previous.statuses...)
		d.mu.Unlock()
		return completedStatusStream(statuses), nil
	}

	now := d.clock.Now()
	if !now.Before(command.Deadline) {
		d.mu.Unlock()
		return nil, fault.New(fault.DeadlineExceeded, op, fmt.Errorf("action %s deadline elapsed", command.ID))
	}
	capability := d.capabilities[command.RequiredCapability]
	if !capability.Supported {
		d.mu.Unlock()
		return nil, fault.New(fault.CapabilityMissing, op, fmt.Errorf("capability %s is unavailable", command.RequiredCapability))
	}
	if command.Action.Type == behavior.Speak && d.active != nil {
		d.mu.Unlock()
		return nil, fault.New(fault.AdapterRejected, op, errors.New("another speech action is active"))
	}

	initial := makeStatuses(command, now,
		behavior.ActionDispatched,
		behavior.ActionAccepted,
		behavior.ActionStarted,
	)
	if command.Action.Type != behavior.Speak {
		statuses := append(initial, newStatus(command, behavior.ActionCompleted, now, ""))
		d.records[command.ID] = &actionRecord{command: command, statuses: statuses, finalized: true}
		d.publishLocked(updateForVisualAction(command, d.clock.Now()))
		d.mu.Unlock()
		return completedStatusStream(statuses), nil
	}

	stream := make(chan behavior.ActionStatus, 4)
	for _, status := range initial {
		stream <- status
	}
	record := &actionRecord{command: command, statuses: initial, stream: stream}
	baseCtx, cancel := context.WithCancelCause(ctx)
	actionCtx := deterministicDeadlineContext{Context: baseCtx, deadline: command.Deadline}
	active := &activeSpeech{record: record, cancel: cancel, done: make(chan struct{})}
	d.records[command.ID] = record
	d.active = active
	d.mu.Unlock()

	go d.runSpeech(actionCtx, active)
	return stream, nil
}

// StopAll cancels active speech, immediately asks the speaker to stop, joins
// the speech worker, and only then publishes a cleared idle state.
func (d *Driver) StopAll(ctx context.Context, _ control.StopReason) error {
	const op = "stop web avatar actions"
	if err := validateContext(op, ctx); err != nil {
		return err
	}

	d.mu.Lock()
	active := d.active
	if active != nil {
		active.cancel(errActionStopped)
	}
	d.mu.Unlock()

	var stopErr error
	if d.speaker != nil {
		stopErr = d.speaker.Stop(ctx)
	}
	if active != nil {
		<-active.done
	}

	d.mu.Lock()
	d.publishLocked(Update{Mode: ModeIdle, OccurredAt: d.clock.Now()})
	d.mu.Unlock()
	if stopErr != nil {
		return classifyExternalError(op, stopErr)
	}
	return nil
}

func (d *Driver) runSpeech(ctx context.Context, active *activeSpeech) {
	defer close(active.done)
	command := active.record.command
	timer := d.clock.NewTimer(command.Deadline.Sub(d.clock.Now()))
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-timer.C():
			active.cancel(errActionDeadline)
		case <-ctx.Done():
		}
	}()

	terminal, reason, text, visualized := d.performSpeech(ctx, active)
	active.cancel(errActionFinished)
	timer.Stop()
	<-watchDone
	d.finishSpeech(active, terminal, reason, text, visualized)
}

func (d *Driver) performSpeech(ctx context.Context, active *activeSpeech) (behavior.ActionState, string, string, bool) {
	command := active.record.command
	d.applyExactDeadline(active)
	if state, reason, stopped := terminalFromCause(context.Cause(ctx)); stopped {
		return state, reason, "", false
	}

	utterance, err := d.realizer.Realize(ctx, port.UtteranceRequest{
		TemplateID:    command.Action.TemplateID,
		InteractionID: command.InteractionID,
		TraceID:       command.TraceID,
	})
	d.applyExactDeadline(active)
	if state, reason, stopped := terminalFromCause(context.Cause(ctx)); stopped {
		return state, reason, "", false
	}
	if err != nil || strings.TrimSpace(utterance.Text) == "" {
		return behavior.ActionFailed, reasonRealizationFailed, "", false
	}

	d.mu.Lock()
	d.publishLocked(Update{
		Mode:       ModeSpeaking,
		Text:       utterance.Text,
		Speaking:   true,
		ActionID:   command.ID,
		OccurredAt: d.clock.Now(),
	})
	d.mu.Unlock()

	err = d.speaker.Speak(ctx, utterance.Text)
	d.applyExactDeadline(active)
	if state, reason, stopped := terminalFromCause(context.Cause(ctx)); stopped {
		return state, reason, utterance.Text, true
	}
	if err != nil {
		return behavior.ActionFailed, reasonSpeechFailed, utterance.Text, true
	}
	return behavior.ActionCompleted, "", utterance.Text, true
}

func (d *Driver) applyExactDeadline(active *activeSpeech) {
	if !d.clock.Now().Before(active.record.command.Deadline) {
		active.cancel(errActionDeadline)
	}
}

func (d *Driver) finishSpeech(active *activeSpeech, state behavior.ActionState, reason, text string, visualized bool) {
	d.mu.Lock()
	record := active.record
	status := newStatus(record.command, state, d.clock.Now(), reason)
	record.statuses = append(record.statuses, status)
	record.finalized = true
	if visualized {
		d.publishLocked(Update{
			Mode:       ModeSpeaking,
			Text:       text,
			Speaking:   false,
			ActionID:   record.command.ID,
			OccurredAt: d.clock.Now(),
		})
	}
	if d.active == active {
		d.active = nil
	}
	record.stream <- status
	d.mu.Unlock()
	close(record.stream)
}

func terminalFromCause(cause error) (behavior.ActionState, string, bool) {
	switch {
	case cause == nil, errors.Is(cause, errActionFinished):
		return "", "", false
	case errors.Is(cause, errActionDeadline), errors.Is(cause, context.DeadlineExceeded):
		return behavior.ActionTimedOut, reasonDeadline, true
	case errors.Is(cause, errActionStopped):
		return behavior.ActionCancelled, reasonStopped, true
	default:
		return behavior.ActionCancelled, reasonParentCancelled, true
	}
}

func validateCommand(command behavior.ActionCommand) error {
	const op = "validate web avatar action"
	if command.ID == "" || command.InteractionID == "" || command.TraceID == "" {
		return fault.New(fault.InvalidInput, op, errors.New("id, interaction_id, and trace_id are required"))
	}
	if command.Deadline.IsZero() {
		return fault.New(fault.InvalidInput, op, errors.New("deadline is required"))
	}
	if command.Preemption != behavior.PreemptInterruptible {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported preemption %q", command.Preemption))
	}
	if command.Idempotency != behavior.IdempotentByActionID {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported idempotency %q", command.Idempotency))
	}
	if command.RequiredCapability != command.Action.Type {
		return fault.New(fault.InvalidInput, op, errors.New("required capability must match action type"))
	}
	if command.Action.Timeout <= 0 {
		return fault.New(fault.InvalidInput, op, errors.New("action timeout must be positive"))
	}
	switch command.Action.Type {
	case behavior.AttendUser, behavior.Acknowledge, behavior.Express, behavior.ReturnIdle:
	case behavior.Speak:
		if command.Action.TemplateID == "" {
			return fault.New(fault.InvalidInput, op, errors.New("speak action requires template_id"))
		}
	default:
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported action type %q", command.Action.Type))
	}
	return nil
}

func validateContext(op string, ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return classifyExternalError(op, err)
	}
	return nil
}

func classifyExternalError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func makeStatuses(command behavior.ActionCommand, occurredAt time.Time, states ...behavior.ActionState) []behavior.ActionStatus {
	statuses := make([]behavior.ActionStatus, 0, len(states))
	for _, state := range states {
		statuses = append(statuses, newStatus(command, state, occurredAt, ""))
	}
	return statuses
}

func newStatus(command behavior.ActionCommand, state behavior.ActionState, occurredAt time.Time, reason string) behavior.ActionStatus {
	return behavior.ActionStatus{
		ActionID:      command.ID,
		InteractionID: command.InteractionID,
		TraceID:       command.TraceID,
		State:         state,
		OccurredAt:    occurredAt,
		Reason:        reason,
	}
}

func completedStatusStream(statuses []behavior.ActionStatus) <-chan behavior.ActionStatus {
	stream := make(chan behavior.ActionStatus, len(statuses))
	for _, status := range statuses {
		stream <- status
	}
	close(stream)
	return stream
}

func updateForVisualAction(command behavior.ActionCommand, occurredAt time.Time) Update {
	mode := ModeIdle
	switch command.Action.Type {
	case behavior.AttendUser:
		mode = ModeAttending
	case behavior.Acknowledge:
		mode = ModeAcknowledging
	case behavior.Express:
		mode = ModeExpressing
	case behavior.ReturnIdle:
		mode = ModeIdle
	}
	return Update{Mode: mode, ActionID: command.ID, OccurredAt: occurredAt}
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
