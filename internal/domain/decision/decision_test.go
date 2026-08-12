package decision

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/observation"
	"proactive-interaction-engine/internal/domain/state"
)

func TestPolicyUserOnCallMustRemainSilent(t *testing.T) {
	inputEvent := event.SemanticEvent{
		ID:         "evt-1",
		Kind:       event.PersonReturned,
		SubjectID:  "user-1",
		OccurredAt: time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC),
		TraceID:    "trace-1",
	}
	snapshot := state.WorldSnapshot{
		SubjectID:     "user-1",
		PersonPresent: true,
		UserBusy:      true,
		BusyReason:    observation.BusyOnCall,
	}
	policy := RulePolicy{Version: "policy.v1", BehaviorVersion: "behavior.v1"}

	got, err := Evaluate(policy, snapshot, inputEvent, "config-test")
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got == nil || got.Kind != Silent {
		t.Fatalf("Evaluate() = %#v, want SILENT", got)
	}
	if len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != ReasonUserOnCall {
		t.Fatalf("ReasonCodes = %#v, want USER_ON_CALL", got.ReasonCodes)
	}
}
