// Command engine is the production composition-root placeholder. The first
// milestone validates behavior through cmd/simulator; platform input adapters
// will own the long-running ingress loop without changing the domain core.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	fakeembodiment "proactive-interaction-engine/adapters/embodiment/fake"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/domain/behavior"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func main() {
	clock := engineclock.System{}
	driver := fakeembodiment.NewDriver(behavior.Capabilities{
		behavior.AttendUser:  {Supported: true, Interruptible: true},
		behavior.Acknowledge: {Supported: true, Interruptible: true},
		behavior.Speak:       {Supported: true, Interruptible: true},
		behavior.ReturnIdle:  {Supported: true, Interruptible: true},
	}, clock)
	core, err := application.New(application.Config{
		SubjectID:              "user-1",
		ReturnAbsenceThreshold: 30 * time.Minute,
		ActionTimeout:          2 * time.Second,
		ExternalCallTimeout:    time.Second,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome_after_return.v1",
		ConfigHash:             "config.runtime.v1",
		RandomSeed:             1,
	}, driver, &memorystorage.AuditRecorder{}, clock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("proactive interaction engine ready; run cmd/simulator for the replay vertical slice")
	<-ctx.Done()
	if err := core.StopAll(context.Background(), "shutdown"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
