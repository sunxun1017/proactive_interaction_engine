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
	"proactive-interaction-engine/internal/domain/observation"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func main() {
	busy := flag.Bool("busy", false, "mark the user as on a call before returning")
	flag.Parse()

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
	output := struct {
		Scenario string                      `json:"scenario"`
		Result   application.Result          `json:"result"`
		Audit    memorystorage.AuditSnapshot `json:"audit"`
	}{
		Scenario: map[bool]string{true: "returned_while_on_call", false: "welcome_after_return"}[*busy],
		Result:   result,
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

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
