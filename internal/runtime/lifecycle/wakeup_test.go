package lifecycle

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestRunAdvancesInitialWakeupOnceAtDeadline(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-initial", Deadline: clock.Now().Add(10 * time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	runner := newWakeupTestRunner(t, processor, clock)
	runCtx, cancelRun, runDone := startTestRunner(runner)
	defer cancelRun()

	waitSignal(t, processor.nextCalls, "runner did not discover initial wakeup")
	clock.Advance(9 * time.Second)
	if _, err := runner.SubmitObservation(runCtx, observation.Observation{ID: "before-deadline"}); err != nil {
		t.Fatalf("SubmitObservation() error = %v", err)
	}
	wantEvent(t, processor.events, "process:before-deadline")
	waitNextWakeupCount(t, processor, 2)
	if got := processor.advanceCallsSnapshot(); len(got) != 0 {
		t.Fatalf("AdvanceAt calls before deadline = %#v", got)
	}

	clock.Advance(time.Second)
	wantEvent(t, processor.events, "advance:wake-initial")
	waitNextWakeupCount(t, processor, 3)
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := processor.advanceCallsSnapshot(); len(got) != 1 || got[0] != wakeup {
		t.Fatalf("AdvanceAt calls = %#v, want exactly %#v", got, wakeup)
	}
}

func TestProcessRefreshReplacesOldWakeupTimer(t *testing.T) {
	clock := newLifecycleClock()
	oldWakeup := application.Wakeup{Token: "wake-old", Deadline: clock.Now().Add(10 * time.Second)}
	newWakeup := application.Wakeup{Token: "wake-new", Deadline: clock.Now().Add(20 * time.Second)}
	processor := newWakeupTestProcessor(oldWakeup)
	processor.processFn = func(_ context.Context, input observation.Observation) (application.Result, error) {
		processor.events <- "process:" + input.ID
		if input.ID == "replace" {
			processor.setWakeup(newWakeup)
		}
		return application.Result{}, nil
	}
	runner := newWakeupTestRunner(t, processor, clock)
	_, cancelRun, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover old wakeup")

	if _, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "replace"}); err != nil {
		t.Fatalf("SubmitObservation(replace) error = %v", err)
	}
	wantEvent(t, processor.events, "process:replace")
	waitNextWakeupCount(t, processor, 2)
	clock.Advance(10 * time.Second)
	if _, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "old-deadline-barrier"}); err != nil {
		t.Fatalf("SubmitObservation(barrier) error = %v", err)
	}
	wantEvent(t, processor.events, "process:old-deadline-barrier")
	waitNextWakeupCount(t, processor, 3)
	if got := processor.advanceCallsSnapshot(); len(got) != 0 {
		t.Fatalf("old timer called AdvanceAt = %#v", got)
	}

	clock.Advance(10 * time.Second)
	wantEvent(t, processor.events, "advance:wake-new")
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := processor.advanceCallsSnapshot(); len(got) != 1 || got[0] != newWakeup {
		t.Fatalf("AdvanceAt calls = %#v, want only %#v", got, newWakeup)
	}
}

func TestQueuedObservationPrecedesDueWakeupAndClearsIt(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-observation-priority", Deadline: clock.Now().Add(10 * time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	release := make(chan struct{})
	processor.processFn = func(_ context.Context, input observation.Observation) (application.Result, error) {
		processor.events <- "start:" + input.ID
		if input.ID == "active" {
			<-release
		} else {
			processor.clearWakeup()
		}
		processor.events <- "return:" + input.ID
		return application.Result{}, nil
	}
	runner := newWakeupTestRunner(t, processor, clock)
	_, cancelRun, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover wakeup")

	activeDone := make(chan error, 1)
	go func() {
		_, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "active"})
		activeDone <- err
	}()
	wantEvent(t, processor.events, "start:active")
	queuedDone := make(chan error, 1)
	go func() {
		_, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "queued-clear"})
		queuedDone <- err
	}()
	waitQueueLength(t, runner.observations, 1)
	clock.Advance(10 * time.Second)
	close(release)
	wantEvent(t, processor.events, "return:active")
	wantEvent(t, processor.events, "start:queued-clear")
	wantEvent(t, processor.events, "return:queued-clear")
	if err := <-activeDone; err != nil {
		t.Fatalf("active observation error = %v", err)
	}
	if err := <-queuedDone; err != nil {
		t.Fatalf("queued observation error = %v", err)
	}
	waitNextWakeupCount(t, processor, 3)
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := processor.advanceCallsSnapshot(); len(got) != 0 {
		t.Fatalf("stale due wakeup was advanced after observation cleared it: %#v", got)
	}
}

func TestQueuedP0PrecedesDueWakeupAndClearsIt(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-control-priority", Deadline: clock.Now().Add(10 * time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	release := make(chan struct{})
	processor.processFn = func(ctx context.Context, input observation.Observation) (application.Result, error) {
		processor.events <- "start:" + input.ID
		<-ctx.Done()
		processor.events <- "cancel:" + input.ID
		<-release
		processor.events <- "return:" + input.ID
		return application.Result{}, ctx.Err()
	}
	runner := newWakeupTestRunner(t, processor, clock)
	_, cancelRun, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover wakeup")

	activeDone := make(chan error, 1)
	go func() {
		_, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "active"})
		activeDone <- err
	}()
	wantEvent(t, processor.events, "start:active")
	clock.Advance(10 * time.Second)
	controlDone := make(chan error, 1)
	go func() { controlDone <- runner.SubmitControl(context.Background(), rejectionCommand("reject-due")) }()
	wantEventsInAnyOrder(t, processor.events, "stop:USER_REJECTED", "cancel:active")
	assertNoProcessorEvent(t, processor.events)
	close(release)
	wantEvent(t, processor.events, "return:active")
	wantEvent(t, processor.events, "commit:reject-due")
	if err := <-controlDone; err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}
	if err := <-activeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("active observation error = %v, want context.Canceled", err)
	}
	waitNextWakeupCount(t, processor, 2)
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if got := processor.advanceCallsSnapshot(); len(got) != 0 {
		t.Fatalf("wakeup advanced despite P0 clearing it: %#v", got)
	}
}

func TestP0CancelsAndJoinsActiveWakeupBeforeCommitAndNextWork(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-active", Deadline: clock.Now().Add(time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	release := make(chan struct{})
	processor.advanceFn = func(ctx context.Context, _ application.Wakeup) (application.Result, error) {
		processor.events <- "start:advance"
		<-ctx.Done()
		processor.events <- "cancel:advance"
		<-release
		processor.events <- "return:advance"
		return application.Result{}, ctx.Err()
	}
	runner := newWakeupTestRunner(t, processor, clock)
	_, cancelRun, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover wakeup")
	clock.Advance(time.Second)
	wantEvent(t, processor.events, "start:advance")

	nextDone := make(chan error, 1)
	go func() {
		_, err := runner.SubmitObservation(context.Background(), observation.Observation{ID: "next"})
		nextDone <- err
	}()
	waitQueueLength(t, runner.observations, 1)
	controlDone := make(chan error, 1)
	go func() {
		controlDone <- runner.SubmitControl(context.Background(), rejectionCommand("reject-active-wakeup"))
	}()
	wantEventsInAnyOrder(t, processor.events, "stop:USER_REJECTED", "cancel:advance")
	assertNoProcessorEvent(t, processor.events)
	close(release)
	wantEvent(t, processor.events, "return:advance")
	wantEvent(t, processor.events, "commit:reject-active-wakeup")
	if err := <-controlDone; err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}
	wantEvent(t, processor.events, "process:next")
	if err := <-nextDone; err != nil {
		t.Fatalf("next observation error = %v", err)
	}
	cancelRun()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestQueuedP0CommitsBeforeActiveWakeupErrorStopsRunner(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-error-race", Deadline: clock.Now().Add(time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	wantErr := errors.New("advance failed after cancellation")
	release := make(chan struct{})
	controlResponse := make(chan error, 1)
	var runner *Runner
	processor.advanceFn = func(ctx context.Context, _ application.Wakeup) (application.Result, error) {
		processor.events <- "start:advance"
		runner.controls <- controlRequest{
			command:  rejectionCommand("reject-before-work-error"),
			response: controlResponse,
		}
		processor.events <- "control:queued"
		<-ctx.Done()
		processor.events <- "cancel:advance"
		<-release
		processor.events <- "return:advance"
		return application.Result{}, wantErr
	}
	runner = newWakeupTestRunner(t, processor, clock)
	_, _, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover wakeup")
	clock.Advance(time.Second)
	wantEvent(t, processor.events, "start:advance")
	wantEvent(t, processor.events, "control:queued")
	wantEventsInAnyOrder(t, processor.events, "stop:USER_REJECTED", "cancel:advance")
	assertNoProcessorEvent(t, processor.events)
	close(release)
	wantEvent(t, processor.events, "return:advance")
	wantEvent(t, processor.events, "commit:reject-before-work-error")
	if err := <-controlResponse; err != nil {
		t.Fatalf("queued control error = %v", err)
	}
	wantEvent(t, processor.events, "stop:SHUTDOWN")
	if err := <-runDone; !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

func TestAdvanceFailureStopsActionsAndReturns(t *testing.T) {
	clock := newLifecycleClock()
	wakeup := application.Wakeup{Token: "wake-failure", Deadline: clock.Now().Add(time.Second)}
	processor := newWakeupTestProcessor(wakeup)
	wantErr := errors.New("advance failed")
	processor.advanceFn = func(_ context.Context, _ application.Wakeup) (application.Result, error) {
		processor.events <- "advance:failed"
		return application.Result{}, wantErr
	}
	runner := newWakeupTestRunner(t, processor, clock)
	_, _, runDone := startTestRunner(runner)
	waitSignal(t, processor.nextCalls, "runner did not discover wakeup")
	clock.Advance(time.Second)
	wantEvent(t, processor.events, "advance:failed")
	wantEvent(t, processor.events, "stop:SHUTDOWN")
	if err := <-runDone; !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
}

type wakeupTestProcessor struct {
	mu           sync.Mutex
	wakeup       application.Wakeup
	hasWakeup    bool
	nextCount    int
	advanceCalls []application.Wakeup
	nextCalls    chan struct{}
	events       chan string
	processFn    func(context.Context, observation.Observation) (application.Result, error)
	advanceFn    func(context.Context, application.Wakeup) (application.Result, error)
}

func newWakeupTestProcessor(wakeup application.Wakeup) *wakeupTestProcessor {
	return &wakeupTestProcessor{
		wakeup:    wakeup,
		hasWakeup: true,
		nextCalls: make(chan struct{}, 32),
		events:    make(chan string, 64),
	}
}

func (p *wakeupTestProcessor) Process(ctx context.Context, input observation.Observation) (application.Result, error) {
	if p.processFn != nil {
		return p.processFn(ctx, input)
	}
	p.events <- "process:" + input.ID
	return application.Result{}, nil
}

func (p *wakeupTestProcessor) StopAll(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.events <- "stop:" + string(command.Reason)
	return nil
}

func (p *wakeupTestProcessor) CommitUserRejection(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.clearWakeup()
	p.events <- "commit:" + command.ID
	return nil
}

func (p *wakeupTestProcessor) NextWakeup() (application.Wakeup, bool) {
	p.mu.Lock()
	p.nextCount++
	wakeup, ok := p.wakeup, p.hasWakeup
	p.mu.Unlock()
	select {
	case p.nextCalls <- struct{}{}:
	default:
	}
	return wakeup, ok
}

func (p *wakeupTestProcessor) AdvanceAt(ctx context.Context, wakeup application.Wakeup) (application.Result, error) {
	p.mu.Lock()
	p.advanceCalls = append(p.advanceCalls, wakeup)
	p.mu.Unlock()
	if p.advanceFn != nil {
		return p.advanceFn(ctx, wakeup)
	}
	p.events <- "advance:" + wakeup.Token
	p.clearWakeup()
	return application.Result{}, nil
}

func (p *wakeupTestProcessor) setWakeup(wakeup application.Wakeup) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wakeup = wakeup
	p.hasWakeup = true
}

func (p *wakeupTestProcessor) clearWakeup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wakeup = application.Wakeup{}
	p.hasWakeup = false
}

func (p *wakeupTestProcessor) nextWakeupCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextCount
}

func (p *wakeupTestProcessor) advanceCallsSnapshot() []application.Wakeup {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]application.Wakeup(nil), p.advanceCalls...)
}

func newWakeupTestRunner(t *testing.T, processor Processor, clock *engineclock.Fake) *Runner {
	t.Helper()
	runner, err := New(Config{ObservationCapacity: 4, ControlCapacity: 2, StopTimeout: time.Second}, processor, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return runner
}

func startTestRunner(runner *Runner) (context.Context, context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	return ctx, cancel, done
}

func waitNextWakeupCount(t *testing.T, processor *wakeupTestProcessor, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for processor.nextWakeupCount() < want {
		select {
		case <-timer.C:
			t.Fatalf("NextWakeup calls = %d, want at least %d", processor.nextWakeupCount(), want)
		default:
			runtime.Gosched()
		}
	}
}

func assertNoProcessorEvent(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case got := <-events:
		t.Fatalf("unexpected processor event %q", got)
	default:
	}
}
