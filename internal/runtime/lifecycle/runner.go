package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

// Processor is the application behavior required by Runner.
type Processor interface {
	Process(context.Context, observation.Observation) (application.Result, error)
	StopAll(context.Context, control.Command) error
	CommitUserRejection(context.Context, control.Command) error
}

type Config struct {
	ObservationCapacity int
	ControlCapacity     int
	StopTimeout         time.Duration
}

func DefaultConfig() Config {
	return Config{
		ObservationCapacity: 256,
		ControlCapacity:     32,
		StopTimeout:         50 * time.Millisecond,
	}
}

func (c Config) Validate() error {
	if c.ObservationCapacity <= 0 || c.ControlCapacity <= 0 {
		return errors.New("observation and control capacities must be positive")
	}
	if c.StopTimeout <= 0 {
		return errors.New("stop timeout must be positive")
	}
	return nil
}

type observationRequest struct {
	ctx      context.Context
	input    observation.Observation
	response chan observationResponse
}

type observationResponse struct {
	result application.Result
	err    error
}

type controlRequest struct {
	command  control.Command
	response chan error
}

type activeObservation struct {
	request   observationRequest
	cancel    context.CancelFunc
	completed <-chan observationResponse
}

// Runner processes one observation at a time while accepting P0 controls on a
// separate bounded queue. Run may be called once.
type Runner struct {
	config       Config
	processor    Processor
	observations chan observationRequest
	controls     chan controlRequest
	done         chan struct{}

	mu      sync.Mutex
	started bool
}

func New(config Config, processor Processor) (*Runner, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate lifecycle config: %w", err)
	}
	if processor == nil {
		return nil, errors.New("processor is required")
	}
	return &Runner{
		config:       config,
		processor:    processor,
		observations: make(chan observationRequest, config.ObservationCapacity),
		controls:     make(chan controlRequest, config.ControlCapacity),
		done:         make(chan struct{}),
	}, nil
}

// SubmitObservation applies bounded backpressure until the request is accepted
// and returns the result produced by the single observation worker.
func (r *Runner) SubmitObservation(ctx context.Context, input observation.Observation) (application.Result, error) {
	if err := ctx.Err(); err != nil {
		return application.Result{}, err
	}
	request := observationRequest{
		ctx:      ctx,
		input:    input,
		response: make(chan observationResponse, 1),
	}
	select {
	case r.observations <- request:
	case <-r.done:
		return application.Result{}, runnerUnavailable()
	case <-ctx.Done():
		return application.Result{}, ctx.Err()
	}

	select {
	case response := <-request.response:
		return response.result, response.err
	case <-r.done:
		select {
		case response := <-request.response:
			return response.result, response.err
		default:
			return application.Result{}, runnerUnavailable()
		}
	case <-ctx.Done():
		return application.Result{}, ctx.Err()
	}
}

// SubmitControl never shares the observation queue and waits until StopAll has
// reached the processor or the caller cancels.
func (r *Runner) SubmitControl(ctx context.Context, command control.Command) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request := controlRequest{command: command, response: make(chan error, 1)}
	select {
	case r.controls <- request:
	case <-r.done:
		return runnerUnavailable()
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-request.response:
		return err
	case <-r.done:
		select {
		case err := <-request.response:
			return err
		default:
			return runnerUnavailable()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run owns every observation-processing goroutine it starts and joins the
// active one before returning.
func (r *Runner) Run(ctx context.Context) error {
	if !r.markStarted() {
		return fault.New(fault.InvalidInput, "run lifecycle", errors.New("runner may only run once"))
	}
	defer close(r.done)

	var active *activeObservation
	for {
		// Mirror the engine's P0 priority rule before entering a blocking select.
		select {
		case request := <-r.controls:
			active = r.handleControl(ctx, active, request)
			continue
		default:
		}

		if active == nil {
			select {
			case <-ctx.Done():
				return r.shutdown(nil, ctx.Err())
			case request := <-r.controls:
				active = r.handleControl(ctx, nil, request)
			case request := <-r.observations:
				active = r.startObservation(request)
			}
			continue
		}

		select {
		case <-ctx.Done():
			return r.shutdown(active, ctx.Err())
		case request := <-r.controls:
			active = r.handleControl(ctx, active, request)
		case response := <-active.completed:
			active.cancel()
			active.request.response <- response
			active = nil
		}
	}
}

func (r *Runner) markStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return false
	}
	r.started = true
	return true
}

func (r *Runner) startObservation(request observationRequest) *activeObservation {
	if err := request.ctx.Err(); err != nil {
		request.response <- observationResponse{err: err}
		return nil
	}
	processCtx, cancel := context.WithCancel(request.ctx)
	completed := make(chan observationResponse, 1)
	go func() {
		result, err := r.processor.Process(processCtx, request.input)
		completed <- observationResponse{result: result, err: err}
	}()
	return &activeObservation{request: request, cancel: cancel, completed: completed}
}

func (r *Runner) handleControl(ctx context.Context, active *activeObservation, request controlRequest) *activeObservation {
	if active != nil {
		active.cancel()
	}
	stopCtx, cancel := context.WithTimeout(ctx, r.config.StopTimeout)
	stopErr := r.processor.StopAll(stopCtx, request.command)
	cancel()

	if active != nil {
		response := <-active.completed
		active.request.response <- response
		active = nil
	}

	var commitErr error
	if request.command.Reason == control.ReasonUserRejected {
		commitErr = r.processor.CommitUserRejection(ctx, request.command)
	}
	request.response <- errors.Join(stopErr, commitErr)
	return active
}

func (r *Runner) shutdown(active *activeObservation, cause error) error {
	if active != nil {
		active.cancel()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), r.config.StopTimeout)
	stopErr := r.processor.StopAll(stopCtx, control.Command{
		ID:      "runtime-shutdown",
		Kind:    control.StopAll,
		Reason:  control.ReasonShutdown,
		TraceID: "runtime-shutdown",
	})
	cancel()

	if active != nil {
		response := <-active.completed
		active.request.response <- response
	}
	r.rejectQueued(cause)
	if stopErr != nil {
		return fault.New(fault.Unavailable, "stop actions during lifecycle shutdown", stopErr)
	}
	return cause
}

func (r *Runner) rejectQueued(cause error) {
	for {
		select {
		case request := <-r.observations:
			request.response <- observationResponse{err: cause}
		default:
			r.rejectQueuedControls(cause)
			return
		}
	}
}

func (r *Runner) rejectQueuedControls(cause error) {
	for {
		select {
		case request := <-r.controls:
			request.response <- cause
		default:
			return
		}
	}
}

func runnerUnavailable() error {
	return fault.New(fault.Unavailable, "submit to lifecycle", errors.New("runner is not running"))
}
