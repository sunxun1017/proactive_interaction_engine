// Command simulator runs deterministic, device-free interaction scenarios.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/behavior"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/observation"
	"proactive-interaction-engine/internal/domain/state"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func main() {
	busy := flag.Bool("busy", false, "mark the user as on a call before returning")
	reply := flag.Bool("reply", false, "submit an adapter-confirmed user reply after the welcome")
	reject := flag.Bool("reject", false, "submit an explicit user rejection after the welcome")
	timeout := flag.Bool("timeout", false, "advance the response window to its deadline")
	flag.Parse()
	selected := 0
	for _, enabled := range []bool{*busy, *reply, *reject, *timeout} {
		if enabled {
			selected++
		}
	}
	if selected > 1 {
		fail(fmt.Errorf("busy, reply, reject, and timeout scenarios are mutually exclusive"))
	}

	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(startedAt)
	driver := fakeembodiment.NewDriver(desktopCapabilities(), clock)
	audit := &memorystorage.AuditRecorder{}
	engine, err := application.New(defaultConfig(), driver, audit, clock)
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	if _, err = engine.Process(ctx, presence("obs-left", 1, startedAt, false)); err != nil {
		fail(err)
	}
	nextSeq := uint64(2)
	if *busy {
		clock.Advance(44 * time.Minute)
		if _, err = engine.Process(ctx, busyOnCall("obs-call", nextSeq, clock.Now())); err != nil {
			fail(err)
		}
		nextSeq++
		clock.Advance(time.Minute)
	} else {
		clock.Advance(45 * time.Minute)
	}

	result, err := engine.Process(ctx, presence("obs-return", nextSeq, clock.Now(), true))
	if err != nil {
		fail(err)
	}
	var followUp *application.Result
	if *reply {
		clock.Advance(4 * time.Second)
		value, processErr := engine.Process(ctx, userReply("obs-reply", nextSeq+1, clock.Now()))
		if processErr != nil {
			fail(processErr)
		}
		followUp = &value
	} else if *reject {
		command := control.Command{
			ID:         "control-reject",
			Kind:       control.StopAll,
			Reason:     control.ReasonUserRejected,
			SubjectID:  "user-1",
			OccurredAt: clock.Now(),
			TraceID:    "trace-simulator-reject",
		}
		if stopErr := engine.StopAll(ctx, command); stopErr != nil {
			fail(stopErr)
		}
		if commitErr := engine.CommitUserRejection(ctx, command); commitErr != nil {
			fail(commitErr)
		}
	} else if *timeout {
		wakeup, ok := engine.NextWakeup()
		if !ok {
			fail(fmt.Errorf("timeout scenario has no pending wakeup"))
		}
		clock.Advance(wakeup.Deadline.Sub(clock.Now()))
		value, advanceErr := engine.AdvanceAt(ctx, wakeup)
		if advanceErr != nil {
			fail(advanceErr)
		}
		followUp = &value
	}
	scenario := "welcome_after_return"
	if *busy {
		scenario = "returned_while_on_call"
	} else if *reply {
		scenario = "welcome_after_return_and_reply"
	} else if *reject {
		scenario = "welcome_after_return_and_rejection"
	} else if *timeout {
		scenario = "welcome_after_return_no_response"
	}
	output := struct {
		Scenario string                      `json:"scenario"`
		Result   application.Result          `json:"result"`
		FollowUp *application.Result         `json:"follow_up,omitempty"`
		Snapshot state.WorldSnapshot         `json:"snapshot"`
		Audit    memorystorage.AuditSnapshot `json:"audit"`
	}{
		Scenario: scenario,
		Result:   result,
		FollowUp: followUp,
		Snapshot: engine.Snapshot(),
		Audit:    audit.Snapshot(),
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(encoded))
}

func defaultConfig() application.Config {
	return application.Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          2 * time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome_after_return.v1",
		ConfigHash:             "config.desktop.v1",
		RandomSeed:             1,
	}
}

func desktopCapabilities() behavior.Capabilities {
	return behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}
}

func presence(id string, seq uint64, at time.Time, present bool) observation.Observation {
	payload := observation.PersonPresence{Present: present}
	return observation.Observation{
		ID:             id,
		SourceID:       "simulator",
		SourceSeq:      seq,
		OccurredAt:     at,
		TTL:            time.Minute,
		SubjectID:      "user-1",
		Confidence:     0.99,
		TraceID:        "trace-simulator-1",
		PersonPresence: &payload,
	}
}

func busyOnCall(id string, seq uint64, at time.Time) observation.Observation {
	payload := observation.UserBusy{Busy: true, Reason: observation.BusyOnCall}
	return observation.Observation{
		ID:         id,
		SourceID:   "simulator",
		SourceSeq:  seq,
		OccurredAt: at,
		TTL:        time.Minute,
		SubjectID:  "user-1",
		Confidence: 0.98,
		TraceID:    "trace-simulator-1",
		UserBusy:   &payload,
	}
}

func userReply(id string, seq uint64, at time.Time) observation.Observation {
	payload := observation.UserReply{}
	return observation.Observation{
		ID:         id,
		SourceID:   "simulator",
		SourceSeq:  seq,
		OccurredAt: at,
		TTL:        time.Minute,
		SubjectID:  "user-1",
		Confidence: 0.99,
		TraceID:    "trace-simulator-1",
		UserReply:  &payload,
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
