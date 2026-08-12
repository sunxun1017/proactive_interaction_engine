// Command conformance runs a small executable smoke test for output adapters.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"proactive-interaction-engine/adapters/embodiment/fake"
	"proactive-interaction-engine/internal/domain/behavior"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func main() {
	now := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	driver := fake.NewDriver(behavior.Capabilities{
		behavior.Acknowledge: {Supported: true, Interruptible: true},
	}, clock)
	command := behavior.ActionCommand{
		ID:                 "action-conformance-1",
		InteractionID:      "interaction-conformance-1",
		TraceID:            "trace-conformance-1",
		Deadline:           now.Add(time.Second),
		Preemption:         behavior.PreemptInterruptible,
		RequiredCapability: behavior.Acknowledge,
		Idempotency:        behavior.IdempotentByActionID,
		Action: behavior.ActionSpec{
			Type:     behavior.Acknowledge,
			Resource: behavior.Gesture,
			Timeout:  time.Second,
		},
	}

	for attempt := 0; attempt < 2; attempt++ {
		stream, err := driver.Execute(context.Background(), command)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var terminal behavior.ActionState
		for status := range stream {
			terminal = status.State
		}
		if terminal != behavior.ActionCompleted {
			fmt.Fprintf(os.Stderr, "attempt %d terminal state = %s\n", attempt+1, terminal)
			os.Exit(1)
		}
	}
	if len(driver.Commands()) != 1 {
		fmt.Fprintln(os.Stderr, "adapter violated action-id idempotency")
		os.Exit(1)
	}
	fmt.Println("fake embodiment conformance: PASS")
}
