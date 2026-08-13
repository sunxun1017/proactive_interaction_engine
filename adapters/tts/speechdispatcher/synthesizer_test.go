package speechdispatcher

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"proactive-interaction-engine/internal/domain/fault"
)

var _ CommandRunner = (*recordingRunner)(nil)

func TestNewValidatesDependencies(t *testing.T) {
	runner := &recordingRunner{}
	for _, test := range []struct {
		name   string
		binary string
		runner CommandRunner
	}{
		{name: "empty binary", runner: runner},
		{name: "nil runner", binary: "/usr/bin/spd-say"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synthesizer, err := New(test.binary, test.runner)
			if err == nil || synthesizer != nil {
				t.Fatalf("New() = %#v, %v, want nil and error", synthesizer, err)
			}
		})
	}
}

func TestSpeakRejectsBlankTextWithoutRunningCommand(t *testing.T) {
	for _, text := range []string{"", " ", "\t\n"} {
		t.Run(text, func(t *testing.T) {
			runner := &recordingRunner{}
			synthesizer := newTestSynthesizer(t, runner)
			if err := synthesizer.Speak(context.Background(), text); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Speak(%q) error = %v, want InvalidInput", text, err)
			}
			if calls := runner.snapshot(); len(calls) != 0 {
				t.Fatalf("runner calls = %#v, want none", calls)
			}
		})
	}
}

func TestSpeakRunsExactArgumentVectorWithoutShellOrTextNormalization(t *testing.T) {
	runner := &recordingRunner{}
	synthesizer := newTestSynthesizer(t, runner)
	text := "  你回来啦；$(touch forbidden)  "

	if err := synthesizer.Speak(context.Background(), text); err != nil {
		t.Fatalf("Speak() error = %v", err)
	}
	want := []commandCall{{binary: "/opt/speech/bin/spd-say", args: []string{"--wait", "--", text}}}
	if got := runner.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runner calls = %#v, want %#v", got, want)
	}
}

func TestSpeakMapsRunnerAndContextFailures(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func() context.Context
		runErr   error
		wantCode fault.Code
		wantRuns int
	}{
		{
			name: "cancelled before call",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCode: fault.Unavailable,
		},
		{
			name:     "runner unavailable",
			ctx:      context.Background,
			runErr:   errors.New("speech dispatcher unavailable"),
			wantCode: fault.Unavailable,
			wantRuns: 1,
		},
		{
			name:     "runner observes cancellation",
			ctx:      context.Background,
			runErr:   context.Canceled,
			wantCode: fault.Unavailable,
			wantRuns: 1,
		},
		{
			name:     "runner observes deadline",
			ctx:      context.Background,
			runErr:   context.DeadlineExceeded,
			wantCode: fault.DeadlineExceeded,
			wantRuns: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{err: test.runErr}
			synthesizer := newTestSynthesizer(t, runner)
			if err := synthesizer.Speak(test.ctx(), "hello"); !fault.IsCode(err, test.wantCode) {
				t.Fatalf("Speak() error = %v, want %s", err, test.wantCode)
			}
			if got := len(runner.snapshot()); got != test.wantRuns {
				t.Fatalf("runner calls = %d, want %d", got, test.wantRuns)
			}
		})
	}
}

func TestStopUsesCurrentUtteranceCommand(t *testing.T) {
	runner := &recordingRunner{}
	synthesizer := newTestSynthesizer(t, runner)
	if err := synthesizer.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	want := []commandCall{{binary: "/opt/speech/bin/spd-say", args: []string{"--stop"}}}
	if got := runner.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runner calls = %#v, want %#v", got, want)
	}
}

func TestStopMapsCancellationAndRunnerFailure(t *testing.T) {
	t.Run("cancelled before call", func(t *testing.T) {
		runner := &recordingRunner{}
		synthesizer := newTestSynthesizer(t, runner)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := synthesizer.Stop(ctx); !fault.IsCode(err, fault.Unavailable) {
			t.Fatalf("Stop() error = %v, want Unavailable", err)
		}
		if calls := runner.snapshot(); len(calls) != 0 {
			t.Fatalf("runner calls = %#v, want none", calls)
		}
	})

	t.Run("runner unavailable", func(t *testing.T) {
		runner := &recordingRunner{err: errors.New("stop failed")}
		synthesizer := newTestSynthesizer(t, runner)
		if err := synthesizer.Stop(context.Background()); !fault.IsCode(err, fault.Unavailable) {
			t.Fatalf("Stop() error = %v, want Unavailable", err)
		}
		if got := len(runner.snapshot()); got != 1 {
			t.Fatalf("runner calls = %d, want 1", got)
		}
	})
}

func TestSynthesizerSupportsConcurrentSpeakAndStop(t *testing.T) {
	runner := &recordingRunner{}
	synthesizer := newTestSynthesizer(t, runner)
	const pairCount = 8
	start := make(chan struct{})
	errorsSeen := make(chan error, pairCount*2)
	var workers sync.WaitGroup
	for range pairCount {
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			errorsSeen <- synthesizer.Speak(context.Background(), "hello")
		}()
		go func() {
			defer workers.Done()
			<-start
			errorsSeen <- synthesizer.Stop(context.Background())
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
	if got := len(runner.snapshot()); got != pairCount*2 {
		t.Fatalf("runner calls = %d, want %d", got, pairCount*2)
	}
}

func newTestSynthesizer(t *testing.T, runner CommandRunner) *Synthesizer {
	t.Helper()
	synthesizer, err := New("/opt/speech/bin/spd-say", runner)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return synthesizer
}

type commandCall struct {
	binary string
	args   []string
}

type recordingRunner struct {
	mu    sync.Mutex
	calls []commandCall
	err   error
}

func (r *recordingRunner) Run(_ context.Context, binary string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, commandCall{binary: binary, args: append([]string(nil), args...)})
	return r.err
}

func (r *recordingRunner) snapshot() []commandCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	output := make([]commandCall, len(r.calls))
	for index, call := range r.calls {
		output[index] = commandCall{binary: call.binary, args: append([]string(nil), call.args...)}
	}
	return output
}
