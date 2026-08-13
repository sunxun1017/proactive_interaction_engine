package webavatar

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
)

func TestSubscribeReturnsSnapshotAndLatestUpdateWithoutBlockingActions(t *testing.T) {
	driver, clock, _ := newTestDriver(t, nil)
	initial, updates, cancel := driver.Subscribe()
	defer cancel()
	if initial != driver.Current() || initial.Mode != ModeIdle {
		t.Fatalf("Subscribe() initial = %#v, Current = %#v, want IDLE snapshot", initial, driver.Current())
	}

	for index, action := range []behavior.ActionType{
		behavior.AttendUser,
		behavior.Acknowledge,
		behavior.Express,
		behavior.ReturnIdle,
	} {
		executeAndCollect(t, driver, context.Background(), testCommand(clock, fmt.Sprintf("slow-%d", index), action))
	}
	select {
	case update := <-updates:
		if update.Mode != ModeIdle || update.ActionID != "slow-3" || update.Revision != driver.Current().Revision {
			t.Fatalf("subscriber update = %#v, want only latest RETURN_IDLE update", update)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive latest update")
	}
	select {
	case update := <-updates:
		t.Fatalf("subscriber retained stale update %#v", update)
	default:
	}
}

func TestSubscribeCancelIsIdempotentClosesAndRemovesSubscriber(t *testing.T) {
	driver, clock, _ := newTestDriver(t, nil)
	_, updates, cancel := driver.Subscribe()
	cancel()
	cancel()

	for {
		select {
		case _, ok := <-updates:
			if !ok {
				goto closed
			}
		case <-time.After(time.Second):
			t.Fatal("cancel did not close subscriber channel")
		}
	}

closed:
	executeAndCollect(t, driver, context.Background(), testCommand(clock, "after-cancel", behavior.AttendUser))
	if update := driver.Current(); update.ActionID != "after-cancel" {
		t.Fatalf("Current() = %#v, want driver usable after subscriber cancellation", update)
	}
}

func TestConcurrentQueriesSubscriptionsActionsAndStopsAreRaceSafe(t *testing.T) {
	driver, clock, _ := newTestDriver(t, nil)
	const workerCount = 12
	start := make(chan struct{})
	errorsSeen := make(chan error, workerCount*3)
	var workers sync.WaitGroup
	for index := range workerCount {
		index := index
		workers.Add(3)
		go func() {
			defer workers.Done()
			<-start
			_ = driver.Current()
			initial, updates, cancel := driver.Subscribe()
			_ = initial
			cancel()
			select {
			case <-updates:
			default:
			}
			errorsSeen <- nil
		}()
		go func() {
			defer workers.Done()
			<-start
			command := testCommand(clock, fmt.Sprintf("concurrent-%d", index), behavior.AttendUser)
			stream, err := driver.Execute(context.Background(), command)
			if err == nil {
				for range stream {
				}
			}
			errorsSeen <- err
		}()
		go func() {
			defer workers.Done()
			<-start
			errorsSeen <- driver.StopAll(context.Background(), control.ReasonShutdown)
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
}
