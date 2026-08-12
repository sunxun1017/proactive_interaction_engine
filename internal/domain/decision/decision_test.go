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

func TestPolicyRejectionCooldownUsesHalfOpenEventTimeWindow(t *testing.T) {
	rejectedAt := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)
	snapshot := state.WorldSnapshot{
		SubjectID:                  "user-1",
		PersonPresent:              true,
		RejectionCooldownStartedAt: rejectedAt,
		RejectionCooldownUntil:     rejectedAt.Add(30 * time.Minute),
	}
	policy := RulePolicy{Version: "policy.v1", BehaviorVersion: "behavior.v1"}

	for _, test := range []struct {
		name string
		at   time.Time
		kind Kind
	}{
		{name: "inside cooldown", at: rejectedAt.Add(29 * time.Minute), kind: Silent},
		{name: "at cooldown deadline", at: rejectedAt.Add(30 * time.Minute), kind: GreetShort},
		{name: "before rejection", at: rejectedAt.Add(-time.Minute), kind: GreetShort},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputEvent := event.SemanticEvent{
				ID:         "evt-" + test.name,
				Kind:       event.PersonReturned,
				SubjectID:  "user-1",
				OccurredAt: test.at,
				TraceID:    "trace-1",
			}
			got, err := Evaluate(policy, snapshot, inputEvent, "config-test")
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got == nil || got.Kind != test.kind {
				t.Fatalf("Evaluate() = %#v, want %s", got, test.kind)
			}
			if test.kind == Silent && (len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != ReasonRecentRejection) {
				t.Fatalf("ReasonCodes = %#v, want RECENT_REJECTION", got.ReasonCodes)
			}
		})
	}
}

func TestPolicyIgnoresInvalidRejectionCooldownInterval(t *testing.T) {
	rejectedAt := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)
	policy := RulePolicy{Version: "policy.v1", BehaviorVersion: "behavior.v1"}
	inputEvent := event.SemanticEvent{
		ID:         "evt-returned",
		Kind:       event.PersonReturned,
		SubjectID:  "user-1",
		OccurredAt: rejectedAt.Add(time.Minute),
		TraceID:    "trace-1",
	}
	for _, until := range []time.Time{rejectedAt, rejectedAt.Add(-time.Minute)} {
		snapshot := state.WorldSnapshot{
			SubjectID:                  "user-1",
			PersonPresent:              true,
			RejectionCooldownStartedAt: rejectedAt,
			RejectionCooldownUntil:     until,
		}
		got, err := Evaluate(policy, snapshot, inputEvent, "config-test")
		if err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
		if got == nil || got.Kind != GreetShort {
			t.Fatalf("Evaluate(until=%s) = %#v, want GREET_SHORT", until, got)
		}
	}
}

func TestPolicyNoResponseCooldownIsHalfOpenAndRejectionTakesPriority(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)
	policy := RulePolicy{Version: "policy.v1", BehaviorVersion: "behavior.v1"}
	for _, test := range []struct {
		name      string
		at        time.Time
		rejection bool
		kind      Kind
		reason    ReasonCode
	}{
		{name: "inside no response cooldown", at: startedAt.Add(4 * time.Minute), kind: Silent, reason: ReasonRecentNoResponse},
		{name: "rejection wins overlap", at: startedAt.Add(4 * time.Minute), rejection: true, kind: Silent, reason: ReasonRecentRejection},
		{name: "deadline allows greeting", at: startedAt.Add(5 * time.Minute), kind: GreetShort},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := state.WorldSnapshot{
				SubjectID:                   "user-1",
				PersonPresent:               true,
				NoResponseCooldownStartedAt: startedAt,
				NoResponseCooldownUntil:     startedAt.Add(5 * time.Minute),
			}
			if test.rejection {
				snapshot.RejectionCooldownStartedAt = startedAt
				snapshot.RejectionCooldownUntil = startedAt.Add(30 * time.Minute)
			}
			got, err := Evaluate(policy, snapshot, event.SemanticEvent{
				ID: "evt-returned", Kind: event.PersonReturned, SubjectID: "user-1", OccurredAt: test.at, TraceID: "trace-1",
			}, "config-test")
			if err != nil || got == nil || got.Kind != test.kind {
				t.Fatalf("Evaluate() = %#v, %v", got, err)
			}
			if test.reason != "" && (len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != test.reason) {
				t.Fatalf("ReasonCodes = %#v, want %s", got.ReasonCodes, test.reason)
			}
		})
	}
}
