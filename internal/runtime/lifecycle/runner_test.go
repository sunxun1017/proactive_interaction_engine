package lifecycle

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestStopAllCancelsActiveObservation(t *testing.T) {
	processor := newBlockingProcessor()
	runner, err := New(Config{
		ObservationCapacity: 2,
		ControlCapacity:     1,
		StopTimeout:         time.Second,
	}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()

	observationDone := make(chan error, 1)
	go func() {
		_, submitErr := runner.SubmitObservation(context.Background(), observation.Observation{ID: "obs-active"})
		observationDone <- submitErr
	}()

	waitSignal(t, processor.started, "active observation did not start")
	command := control.Command{
		ID:         "control-stop-1",
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC),
		TraceID:    "trace-stop-1",
	}
	if err := runner.SubmitControl(context.Background(), command); err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}

	select {
	case got := <-processor.stopReasons:
		if got != control.ReasonUserRejected {
			t.Fatalf("stop reason = %s, want USER_REJECTED", got)
		}
	case <-time.After(time.Second):
		t.Fatal("processor did not receive StopAll")
	}
	select {
	case err := <-observationDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SubmitObservation() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active observation was not cancelled")
	}

	cancelRun()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop")
	}
}

func TestUserRejectionJoinsActiveProcessBeforeCommit(t *testing.T) {
	processor := newOrderedProcessor()
	runner, err := New(Config{ObservationCapacity: 2, ControlCapacity: 1, StopTimeout: time.Second}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()

	observationDone := make(chan error, 1)
	go func() {
		_, submitErr := runner.SubmitObservation(context.Background(), observation.Observation{ID: "active"})
		observationDone <- submitErr
	}()
	wantEvent(t, processor.events, "start:active")

	controlDone := make(chan error, 1)
	go func() { controlDone <- runner.SubmitControl(context.Background(), rejectionCommand("reject-1")) }()
	wantEventsInAnyOrder(t, processor.events, "stop:USER_REJECTED", "cancel:active")
	select {
	case got := <-processor.events:
		t.Fatalf("event before process joined = %q", got)
	default:
	}
	close(processor.release)
	wantEvent(t, processor.events, "return:active")
	wantEvent(t, processor.events, "commit:reject-1")
	if err := <-controlDone; err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}
	if err := <-observationDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("SubmitObservation() error = %v, want context.Canceled", err)
	}

	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestUserRejectionCommitsBeforeNextObservationStarts(t *testing.T) {
	processor := newOrderedProcessor()
	runner, err := New(Config{ObservationCapacity: 2, ControlCapacity: 1, StopTimeout: time.Second}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()

	go runner.SubmitObservation(context.Background(), observation.Observation{ID: "active"})
	wantEvent(t, processor.events, "start:active")
	go runner.SubmitObservation(context.Background(), observation.Observation{ID: "next"})
	waitQueueLength(t, runner.observations, 1)
	controlDone := make(chan error, 1)
	go func() { controlDone <- runner.SubmitControl(context.Background(), rejectionCommand("reject-ordered")) }()
	wantEventsInAnyOrder(t, processor.events, "stop:USER_REJECTED", "cancel:active")
	close(processor.release)
	wantEvent(t, processor.events, "return:active")
	wantEvent(t, processor.events, "commit:reject-ordered")
	if err := <-controlDone; err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}
	wantEvent(t, processor.events, "start:next")

	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestStopFailureStillCommitsUserRejection(t *testing.T) {
	processor := newBlockingProcessor()
	processor.stopErr = errors.New("adapter disconnected")
	runner, err := New(Config{ObservationCapacity: 1, ControlCapacity: 1, StopTimeout: time.Second}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()

	err = runner.SubmitControl(context.Background(), rejectionCommand("reject-after-stop-failure"))
	if err == nil {
		t.Fatal("SubmitControl() error = nil, want stop failure")
	}
	select {
	case got := <-processor.committed:
		if got.ID != "reject-after-stop-failure" {
			t.Fatalf("committed command = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("rejection was not committed after StopAll failure")
	}

	processor.stopErr = nil
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestRunnerUsesConfiguredBoundedQueues(t *testing.T) {
	runner, err := New(Config{
		ObservationCapacity: 7,
		ControlCapacity:     3,
		StopTimeout:         time.Second,
	}, newBlockingProcessor(), newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := cap(runner.observations); got != 7 {
		t.Fatalf("observation capacity = %d, want 7", got)
	}
	if got := cap(runner.controls); got != 3 {
		t.Fatalf("control capacity = %d, want 3", got)
	}
}

func TestShutdownCancelsAndJoinsActiveObservation(t *testing.T) {
	processor := newBlockingProcessor()
	runner, err := New(Config{
		ObservationCapacity: 1,
		ControlCapacity:     1,
		StopTimeout:         time.Second,
	}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(runCtx) }()
	observationDone := make(chan error, 1)
	go func() {
		_, submitErr := runner.SubmitObservation(context.Background(), observation.Observation{ID: "obs-shutdown"})
		observationDone <- submitErr
	}()
	waitSignal(t, processor.started, "active observation did not start")

	cancelRun()
	select {
	case reason := <-processor.stopReasons:
		if reason != control.ReasonShutdown {
			t.Fatalf("stop reason = %s, want SHUTDOWN", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not call StopAll")
	}
	select {
	case err := <-observationDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SubmitObservation() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active observation did not exit")
	}
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after active work exited")
	}
	select {
	case command := <-processor.committed:
		t.Fatalf("shutdown committed rejection %#v", command)
	default:
	}
}

func TestShutdownReportsStopFailure(t *testing.T) {
	processor := newBlockingProcessor()
	processor.stopErr = errors.New("adapter disconnected")
	runner, err := New(Config{
		ObservationCapacity: 1,
		ControlCapacity:     1,
		StopTimeout:         time.Second,
	}, processor, newLifecycleClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = runner.Run(ctx)
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Run() error = %v, want Unavailable", err)
	}
}

func TestNewRequiresClock(t *testing.T) {
	if _, err := New(DefaultConfig(), newBlockingProcessor(), nil); err == nil {
		t.Fatal("New() error = nil, want missing clock failure")
	}
}

type blockingProcessor struct {
	started     chan struct{}
	stopReasons chan control.StopReason
	committed   chan control.Command
	stopErr     error
}

func newBlockingProcessor() *blockingProcessor {
	return &blockingProcessor{
		started:     make(chan struct{}, 1),
		stopReasons: make(chan control.StopReason, 2),
		committed:   make(chan control.Command, 2),
	}
}

func (p *blockingProcessor) Process(ctx context.Context, _ observation.Observation) (application.Result, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return application.Result{}, ctx.Err()
}

func (p *blockingProcessor) StopAll(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.stopReasons <- command.Reason
	return p.stopErr
}

func (p *blockingProcessor) CommitUserRejection(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.committed <- command
	return nil
}

func (p *blockingProcessor) NextWakeup() (application.Wakeup, bool) {
	return application.Wakeup{}, false
}

func (p *blockingProcessor) AdvanceAt(ctx context.Context, _ application.Wakeup) (application.Result, error) {
	return application.Result{}, ctx.Err()
}

type orderedProcessor struct {
	events  chan string
	release chan struct{}
}

func newOrderedProcessor() *orderedProcessor {
	return &orderedProcessor{events: make(chan string, 16), release: make(chan struct{})}
}

func (p *orderedProcessor) Process(ctx context.Context, input observation.Observation) (application.Result, error) {
	p.events <- "start:" + input.ID
	<-ctx.Done()
	p.events <- "cancel:" + input.ID
	<-p.release
	p.events <- "return:" + input.ID
	return application.Result{}, ctx.Err()
}

func (p *orderedProcessor) StopAll(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.events <- "stop:" + string(command.Reason)
	return nil
}

func (p *orderedProcessor) CommitUserRejection(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.events <- "commit:" + command.ID
	return nil
}

func (p *orderedProcessor) NextWakeup() (application.Wakeup, bool) {
	return application.Wakeup{}, false
}

func (p *orderedProcessor) AdvanceAt(ctx context.Context, _ application.Wakeup) (application.Result, error) {
	return application.Result{}, ctx.Err()
}

func rejectionCommand(id string) control.Command {
	return control.Command{
		ID:         id,
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  "user-1",
		OccurredAt: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC),
		TraceID:    "trace-" + id,
	}
}

func newLifecycleClock() *engineclock.Fake {
	return engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
}

func wantEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("event = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("event %q not received", want)
	}
}

func wantEventsInAnyOrder(t *testing.T, events <-chan string, first, second string) {
	t.Helper()
	want := map[string]bool{first: false, second: false}
	for range 2 {
		select {
		case got := <-events:
			seen, expected := want[got]
			if !expected || seen {
				t.Fatalf("unexpected event %q, want %q and %q", got, first, second)
			}
			want[got] = true
		case <-time.After(time.Second):
			t.Fatalf("events %q and %q not received", first, second)
		}
	}
}

func waitQueueLength[T any](t *testing.T, queue chan T, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(queue) != want {
		select {
		case <-timer.C:
			t.Fatalf("queue length = %d, want %d", len(queue), want)
		default:
			runtime.Gosched()
		}
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}
