package speechdispatcher

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"proactive-interaction-engine/internal/domain/fault"
)

// CommandRunner executes one command without interpreting it through a shell.
type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

// Synthesizer speaks local text through an explicitly configured spd-say
// binary. It is immutable and safe for concurrent use when its runner is safe.
type Synthesizer struct {
	binaryPath string
	runner     CommandRunner
}

// New constructs a Speech Dispatcher synthesizer with explicit dependencies.
func New(binaryPath string, runner CommandRunner) (*Synthesizer, error) {
	if strings.TrimSpace(binaryPath) == "" {
		return nil, fault.New(fault.InvalidInput, "create speech dispatcher synthesizer", errors.New("binary path is required"))
	}
	if isNilCommandRunner(runner) {
		return nil, fault.New(fault.InvalidInput, "create speech dispatcher synthesizer", errors.New("command runner is required"))
	}
	return &Synthesizer{binaryPath: binaryPath, runner: runner}, nil
}

// Speak waits for Speech Dispatcher to finish speaking the exact supplied text.
func (s *Synthesizer) Speak(ctx context.Context, text string) error {
	const op = "speak with speech dispatcher"
	if strings.TrimSpace(text) == "" {
		return fault.New(fault.InvalidInput, op, errors.New("text is required"))
	}
	if err := contextError(ctx); err != nil {
		return classifyError(op, err)
	}
	if err := s.runner.Run(ctx, s.binaryPath, "--wait", "--", text); err != nil {
		return classifyError(op, err)
	}
	return nil
}

// Stop asks Speech Dispatcher to stop the current utterance.
func (s *Synthesizer) Stop(ctx context.Context) error {
	const op = "stop speech dispatcher"
	if err := contextError(ctx); err != nil {
		return classifyError(op, err)
	}
	if err := s.runner.Run(ctx, s.binaryPath, "--stop"); err != nil {
		return classifyError(op, err)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	return ctx.Err()
}

func classifyError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func isNilCommandRunner(runner CommandRunner) bool {
	if runner == nil {
		return true
	}
	value := reflect.ValueOf(runner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
