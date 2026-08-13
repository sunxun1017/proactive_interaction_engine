package engine

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestReplyAcceptanceWindowStartsAbsentAndOpensAfterPreActions(t *testing.T) {
	core, clock := newReplyWindowQueryEngine(t, nil)
	if window, ok := core.CurrentReplyAcceptanceWindow(); ok {
		t.Fatalf("CurrentReplyAcceptanceWindow() = %#v, want none", window)
	}

	openedAt := openReplyAcceptanceWindow(t, core, clock)
	want := ReplyAcceptanceWindow{
		SubjectID: "user-1",
		OpenedAt:  openedAt,
		Deadline:  openedAt.Add(8 * time.Second),
	}
	if got, ok := core.CurrentReplyAcceptanceWindow(); !ok || got != want {
		t.Fatalf("CurrentReplyAcceptanceWindow() = %#v, %t, want %#v", got, ok, want)
	}
}

func TestReplyAcceptanceWindowDoesNotOpenWhenPlanningOrPreActionFails(t *testing.T) {
	tests := map[string]*replyWindowFaultDriver{
		"capabilities": {capabilitiesErr: errors.New("capabilities unavailable")},
		"pre action":   {executeErr: errors.New("pre action rejected")},
	}
	for name, driver := range tests {
		t.Run(name, func(t *testing.T) {
			core, clock := newReplyWindowQueryEngine(t, driver)
			processOK(t, core, presenceObservation("left-"+name, 1, clock.Now(), false))
			clock.Advance(45 * time.Minute)
			if _, err := core.Process(context.Background(), presenceObservation("returned-"+name, 2, clock.Now(), true)); err == nil {
				t.Fatal("Process(returned) error = nil, want failure")
			}
			if window, ok := core.CurrentReplyAcceptanceWindow(); ok {
				t.Fatalf("CurrentReplyAcceptanceWindow() = %#v after failure, want none", window)
			}
		})
	}
}

func TestReplyAcceptanceWindowClearsOnEveryTerminalPath(t *testing.T) {
	for _, terminal := range []string{"reply", "rejection", "expiration"} {
		t.Run(terminal, func(t *testing.T) {
			core, clock := newReplyWindowQueryEngine(t, nil)
			openReplyAcceptanceWindow(t, core, clock)
			if _, ok := core.CurrentReplyAcceptanceWindow(); !ok {
				t.Fatal("CurrentReplyAcceptanceWindow() = none before terminal path")
			}

			switch terminal {
			case "reply":
				clock.Advance(time.Second)
				processOK(t, core, replyObservation("reply-query", 3, clock.Now()))
			case "rejection":
				if err := core.CommitUserRejection(context.Background(), rejectionControl("reject-query", clock.Now())); err != nil {
					t.Fatalf("CommitUserRejection() error = %v", err)
				}
			case "expiration":
				wakeup, ok := core.NextWakeup()
				if !ok {
					t.Fatal("NextWakeup() = none")
				}
				clock.Advance(wakeup.Deadline.Sub(clock.Now()))
				if _, err := core.AdvanceAt(context.Background(), wakeup); err != nil {
					t.Fatalf("AdvanceAt() error = %v", err)
				}
			}

			if window, ok := core.CurrentReplyAcceptanceWindow(); ok {
				t.Fatalf("CurrentReplyAcceptanceWindow() = %#v after %s, want none", window, terminal)
			}
		})
	}
}

func TestReplyAcceptanceWindowIsAnImmutableMinimalValue(t *testing.T) {
	typeOfWindow := reflect.TypeOf(ReplyAcceptanceWindow{})
	wantFields := map[string]reflect.Type{
		"SubjectID": reflect.TypeOf(""),
		"OpenedAt":  reflect.TypeOf(time.Time{}),
		"Deadline":  reflect.TypeOf(time.Time{}),
	}
	if typeOfWindow.NumField() != len(wantFields) {
		t.Fatalf("ReplyAcceptanceWindow has %d fields, want exactly %d", typeOfWindow.NumField(), len(wantFields))
	}
	for name, wantType := range wantFields {
		field, ok := typeOfWindow.FieldByName(name)
		if !ok || field.Type != wantType {
			t.Fatalf("ReplyAcceptanceWindow.%s = %#v, want %s", name, field, wantType)
		}
	}

	core, clock := newReplyWindowQueryEngine(t, nil)
	openReplyAcceptanceWindow(t, core, clock)
	original, ok := core.CurrentReplyAcceptanceWindow()
	if !ok {
		t.Fatal("CurrentReplyAcceptanceWindow() = none")
	}
	modified := original
	modified.SubjectID = "other-user"
	modified.OpenedAt = modified.OpenedAt.Add(time.Hour)
	modified.Deadline = modified.Deadline.Add(time.Hour)
	if reread, ok := core.CurrentReplyAcceptanceWindow(); !ok || reread != original {
		t.Fatalf("query mutation changed internal window: got %#v, %t, want %#v", reread, ok, original)
	}
}

func TestReplyAcceptanceWindowSupportsConcurrentReadersAndSingleWriterLifecycle(t *testing.T) {
	core, clock := newReplyWindowQueryEngine(t, nil)
	openedAt := openReplyAcceptanceWindow(t, core, clock)
	want := ReplyAcceptanceWindow{SubjectID: "user-1", OpenedAt: openedAt, Deadline: openedAt.Add(8 * time.Second)}

	const readerCount = 8
	ready := make(chan struct{}, readerCount)
	stop := make(chan struct{})
	errorsSeen := make(chan string, readerCount)
	var readers sync.WaitGroup
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			ready <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					window, ok := core.CurrentReplyAcceptanceWindow()
					if ok && window != want {
						select {
						case errorsSeen <- "reader observed a partial or unexpected window":
						default:
						}
						return
					}
					runtime.Gosched()
				}
			}
		}()
	}
	for range readerCount {
		<-ready
	}
	clock.Advance(time.Second)
	processOK(t, core, replyObservation("reply-concurrent-query", 3, clock.Now()))
	close(stop)
	readers.Wait()
	close(errorsSeen)
	for message := range errorsSeen {
		t.Error(message)
	}
	if window, ok := core.CurrentReplyAcceptanceWindow(); ok {
		t.Fatalf("CurrentReplyAcceptanceWindow() = %#v after reply, want none", window)
	}
}

func newReplyWindowQueryEngine(t *testing.T, faultDriver *replyWindowFaultDriver) (*Engine, *engineclock.Fake) {
	t.Helper()
	clock := engineclock.NewFake(time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC))
	base := fakeembodiment.NewDriver(desktopTestCapabilities(), clock)
	var driver interface {
		Capabilities(context.Context) (behavior.Capabilities, error)
		Execute(context.Context, behavior.ActionCommand) (<-chan behavior.ActionStatus, error)
		StopAll(context.Context, control.StopReason) error
	} = base
	if faultDriver != nil {
		faultDriver.delegate = base
		driver = faultDriver
	}
	core, err := New(validTestConfig(), driver, &memorystorage.AuditRecorder{}, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return core, clock
}

func openReplyAcceptanceWindow(t *testing.T, core *Engine, clock *engineclock.Fake) time.Time {
	t.Helper()
	processOK(t, core, presenceObservation("left-query", 1, clock.Now(), false))
	clock.Advance(45 * time.Minute)
	openedAt := clock.Now()
	processOK(t, core, presenceObservation("returned-query", 2, openedAt, true))
	return openedAt
}

type replyWindowFaultDriver struct {
	delegate        *fakeembodiment.Driver
	capabilitiesErr error
	executeErr      error
}

func (d *replyWindowFaultDriver) Capabilities(ctx context.Context) (behavior.Capabilities, error) {
	if d.capabilitiesErr != nil {
		return nil, d.capabilitiesErr
	}
	return d.delegate.Capabilities(ctx)
}

func (d *replyWindowFaultDriver) Execute(ctx context.Context, command behavior.ActionCommand) (<-chan behavior.ActionStatus, error) {
	if d.executeErr != nil {
		return nil, d.executeErr
	}
	return d.delegate.Execute(ctx, command)
}

func (d *replyWindowFaultDriver) StopAll(ctx context.Context, reason control.StopReason) error {
	return d.delegate.StopAll(ctx, reason)
}
