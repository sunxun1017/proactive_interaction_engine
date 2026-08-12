package behavior

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/decision"
)

func TestPlannerNeverEmitsUnsupportedCapability(t *testing.T) {
	planner := Planner{ActionTimeout: time.Second}
	input := decision.Decision{
		ID:              "decision-1",
		InteractionID:   "interaction-1",
		Kind:            decision.GreetShort,
		BehaviorVersion: "welcome.v1",
		TraceID:         "trace-1",
	}
	capabilities := Capabilities{
		Acknowledge: {Supported: true, Interruptible: true},
		ReturnIdle:  {Supported: true, Interruptible: true},
	}

	plan, err := planner.Plan(input, capabilities, time.Time{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	for _, action := range plan.Actions() {
		if action.Type == AttendUser || action.Type == Speak {
			t.Fatalf("Plan() emitted unsupported action %s", action.Type)
		}
	}
}
