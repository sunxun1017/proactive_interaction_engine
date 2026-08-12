package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

// Processor is the application behavior required by Runner.
type Processor interface {
	Process(context.Context, observation.Observation) (application.Result, error)
	StopAll(context.Context, control.Command) error
	CommitUserRejection(context.Context, control.Command) error
	NextWakeup() (application.Wakeup, bool)
	AdvanceAt(context.Context, application.Wakeup) (application.Result, error)
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
	response chan workResult
}

type workResult struct {
	result application.Result
	err    error
}

type controlRequest struct {
	command  control.Command
	response chan error
}

type activeWork struct {
	cancel    context.CancelFunc
	completed <-chan workResult
	finish    func(workResult) error
}

type scheduledWakeup struct {
	wakeup application.Wakeup
	timer  port.Timer
	due    bool
}

// Runner processes one observation at a time while accepting P0 controls on a
// separate bounded queue. Run may be called once.
type Runner struct {
	config       Config
	processor    Processor
	clock        port.Clock
	observations chan observationRequest
	controls     chan controlRequest
	done         chan struct{}

	mu      sync.Mutex
	started bool
}

func New(config Config, processor Processor, clock port.Clock) (*Runner, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate lifecycle config: %w", err)
	}
	if processor == nil || clock == nil {
		return nil, errors.New("processor and clock are required")
	}
	return &Runner{
		config:       config,
		processor:    processor,
		clock:        clock,
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
		response: make(chan workResult, 1),
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

// Run owns every work goroutine it starts and joins the active one before
// returning.
func (r *Runner) Run(ctx context.Context) error {
	if !r.markStarted() {
		return fault.New(fault.InvalidInput, "run lifecycle", errors.New("runner may only run once"))
	}
	defer close(r.done)

	var active *activeWork
	scheduled := r.refreshWakeup(nil)
	handlePriorityControl := func(request controlRequest) error {
		var workErr error
		active, workErr = r.handleControl(ctx, active, request)
		scheduled = r.refreshWakeup(scheduled)
		return workErr
	}
	shutdownAfterWorkError := func(workErr error) error {
		r.stopScheduled(scheduled)
		return r.shutdown(nil, workErr)
	}
	for {
		// P0 must be observed before either an idle dispatch or a concurrently
		// completed work result. Go select does not otherwise preserve priority.
		select {
		case request := <-r.controls:
			if workErr := handlePriorityControl(request); workErr != nil {
				return shutdownAfterWorkError(workErr)
			}
			continue
		default:
		}

		if active == nil {
			select {
			case request := <-r.observations:
				active = r.startObservation(request)
				continue
			default:
			}
			if scheduled != nil && scheduled.due {
				active = r.startWakeup(ctx, scheduled.wakeup)
				scheduled = nil
				continue
			}

			var timerC <-chan time.Time
			if scheduled != nil && scheduled.timer != nil {
				timerC = scheduled.timer.C()
			}
			select {
			case <-ctx.Done():
				r.stopScheduled(scheduled)
				return r.shutdown(nil, ctx.Err())
			case request := <-r.controls:
				if workErr := handlePriorityControl(request); workErr != nil {
					return shutdownAfterWorkError(workErr)
				}
			case request := <-r.observations:
				active = r.startObservation(request)
			case <-timerC:
				scheduled.timer = nil
				scheduled.due = true
			}
			continue
		}

		select {
		case <-ctx.Done():
			r.stopScheduled(scheduled)
			return r.shutdown(active, ctx.Err())
		case request := <-r.controls:
			if workErr := handlePriorityControl(request); workErr != nil {
				return shutdownAfterWorkError(workErr)
			}
		case response := <-active.completed:
			active.cancel()
			workErr := active.finish(response)
			active = nil
			scheduled = r.refreshWakeup(scheduled)
			if workErr != nil {
				return shutdownAfterWorkError(workErr)
			}
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

func (r *Runner) startObservation(request observationRequest) *activeWork {
	if err := request.ctx.Err(); err != nil {
		request.response <- workResult{err: err}
		return nil
	}
	processCtx, cancel := context.WithCancel(request.ctx)
	completed := make(chan workResult, 1)
	go func() {
		result, err := r.processor.Process(processCtx, request.input)
		completed <- workResult{result: result, err: err}
	}()
	return &activeWork{
		cancel:    cancel,
		completed: completed,
		finish: func(result workResult) error {
			request.response <- result
			return nil
		},
	}
}

func (r *Runner) startWakeup(ctx context.Context, wakeup application.Wakeup) *activeWork {
	workCtx, cancel := context.WithCancel(ctx)
	completed := make(chan workResult, 1)
	go func() {
		result, err := r.processor.AdvanceAt(workCtx, wakeup)
		completed <- workResult{result: result, err: err}
	}()
	return &activeWork{
		cancel:    cancel,
		completed: completed,
		finish: func(result workResult) error {
			if result.err == nil || errors.Is(result.err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("advance wakeup %s: %w", wakeup.Token, result.err)
		},
	}
}

func (r *Runner) handleControl(ctx context.Context, active *activeWork, request controlRequest) (*activeWork, error) {
	if active != nil {
		active.cancel()
	}
	stopCtx, cancel := context.WithTimeout(ctx, r.config.StopTimeout)
	stopErr := r.processor.StopAll(stopCtx, request.command)
	cancel()

	var workErr error
	if active != nil {
		response := <-active.completed
		workErr = active.finish(response)
		active = nil
	}

	var commitErr error
	if request.command.Reason == control.ReasonUserRejected {
		commitErr = r.processor.CommitUserRejection(ctx, request.command)
	}
	request.response <- errors.Join(stopErr, commitErr)
	return active, workErr
}

func (r *Runner) refreshWakeup(current *scheduledWakeup) *scheduledWakeup {
	r.stopScheduled(current)
	wakeup, ok := r.processor.NextWakeup()
	if !ok {
		return nil
	}
	now := r.clock.Now()
	if !wakeup.Deadline.After(now) {
		return &scheduledWakeup{wakeup: wakeup, due: true}
	}
	return &scheduledWakeup{
		wakeup: wakeup,
		timer:  r.clock.NewTimer(wakeup.Deadline.Sub(now)),
	}
}

func (r *Runner) stopScheduled(scheduled *scheduledWakeup) {
	if scheduled != nil && scheduled.timer != nil {
		scheduled.timer.Stop()
	}
}

func (r *Runner) shutdown(active *activeWork, cause error) error {
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
		_ = active.finish(response)
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
			request.response <- workResult{err: cause}
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
