package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

func TestStopAllCancelsActiveObservation(t *testing.T) {
	processor := newBlockingProcessor()
	runner, err := New(Config{
		ObservationCapacity: 2,
		ControlCapacity:     1,
		StopTimeout:         time.Second,
	}, processor)
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
		ID:      "control-stop-1",
		Kind:    control.StopAll,
		Reason:  control.ReasonUserRequested,
		TraceID: "trace-stop-1",
	}
	if err := runner.SubmitControl(context.Background(), command); err != nil {
		t.Fatalf("SubmitControl() error = %v", err)
	}

	select {
	case got := <-processor.stopReasons:
		if got != control.ReasonUserRequested {
			t.Fatalf("stop reason = %s, want USER_REQUESTED", got)
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

func TestRunnerUsesConfiguredBoundedQueues(t *testing.T) {
	runner, err := New(Config{
		ObservationCapacity: 7,
		ControlCapacity:     3,
		StopTimeout:         time.Second,
	}, newBlockingProcessor())
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
	}, processor)
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
}

func TestShutdownReportsStopFailure(t *testing.T) {
	processor := newBlockingProcessor()
	processor.stopErr = errors.New("adapter disconnected")
	runner, err := New(Config{
		ObservationCapacity: 1,
		ControlCapacity:     1,
		StopTimeout:         time.Second,
	}, processor)
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

type blockingProcessor struct {
	started     chan struct{}
	stopReasons chan control.StopReason
	stopErr     error
}

func newBlockingProcessor() *blockingProcessor {
	return &blockingProcessor{
		started:     make(chan struct{}, 1),
		stopReasons: make(chan control.StopReason, 2),
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

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}
