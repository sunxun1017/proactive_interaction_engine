package observation

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestObservationValidateAtRejectsExpiredInput(t *testing.T) {
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	present := PersonPresence{Present: true}
	input := Observation{
		ID:             "obs-1",
		SourceID:       "replay",
		SourceSeq:      1,
		OccurredAt:     now.Add(-2 * time.Second),
		TTL:            time.Second,
		SubjectID:      "user-1",
		Confidence:     1,
		TraceID:        "trace-1",
		PersonPresence: &present,
	}

	err := input.ValidateAt(now)
	if !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("ValidateAt() error = %v, want StaleInput", err)
	}
}

func TestObservationValidateAtAcceptsOnlyUserReplyPayload(t *testing.T) {
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	reply := UserReply{}
	input := Observation{
		ID:         "reply-1",
		SourceID:   "replay",
		SourceSeq:  1,
		OccurredAt: now,
		TTL:        time.Minute,
		SubjectID:  "user-1",
		Confidence: 1,
		TraceID:    "trace-1",
		UserReply:  &reply,
	}
	if err := input.ValidateAt(now); err != nil {
		t.Fatalf("ValidateAt() error = %v", err)
	}

	present := PersonPresence{Present: true}
	input.PersonPresence = &present
	if err := input.ValidateAt(now); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ValidateAt(two payloads) error = %v, want InvalidInput", err)
	}
}
