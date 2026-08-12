package state

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestProjectorAppliesRejectionCooldown(t *testing.T) {
	rejectedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	projector := NewProjector("user-1")

	got, err := projector.ApplyUserRejection(rejectionEvent(t, "rejection-1", "user-1", rejectedAt), 30*time.Minute)
	if err != nil {
		t.Fatalf("ApplyUserRejection() error = %v", err)
	}

	if got.RejectionCooldownStartedAt != rejectedAt {
		t.Fatalf("RejectionCooldownStartedAt = %s, want %s", got.RejectionCooldownStartedAt, rejectedAt)
	}
	if want := rejectedAt.Add(30 * time.Minute); got.RejectionCooldownUntil != want {
		t.Fatalf("RejectionCooldownUntil = %s, want %s", got.RejectionCooldownUntil, want)
	}
	if got.Version != 1 || got.UpdatedAt != rejectedAt {
		t.Fatalf("projected metadata = %#v", got)
	}
}

func TestProjectorRejectsInvalidRejectionWithoutMutation(t *testing.T) {
	projector := NewProjector("user-1")
	input := event.SemanticEvent{
		ID:         "event-1",
		Kind:       event.PersonReturned,
		SubjectID:  "user-1",
		OccurredAt: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC),
		TraceID:    "trace-1",
	}
	if _, err := projector.ApplyUserRejection(input, time.Minute); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ApplyUserRejection(kind) error = %v, want InvalidInput", err)
	}
	valid := rejectionEvent(t, "rejection-1", "user-1", input.OccurredAt)
	if _, err := projector.ApplyUserRejection(valid, 0); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ApplyUserRejection(cooldown) error = %v, want InvalidInput", err)
	}
	if got := projector.Snapshot(); got.Version != 0 {
		t.Fatalf("Snapshot() = %#v, want unchanged", got)
	}
}

func TestProjectorIgnoresOtherSubjectAndOlderRejection(t *testing.T) {
	projector := NewProjector("user-1")
	base := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	if _, err := projector.ApplyUserRejection(rejectionEvent(t, "other", "user-2", base), 30*time.Minute); err != nil {
		t.Fatalf("ApplyUserRejection(other subject) error = %v", err)
	}
	if got := projector.Snapshot(); got.Version != 0 {
		t.Fatalf("other-subject Snapshot() = %#v, want unchanged", got)
	}

	newer := base.Add(10 * time.Minute)
	if _, err := projector.ApplyUserRejection(rejectionEvent(t, "newer", "user-1", newer), 30*time.Minute); err != nil {
		t.Fatalf("ApplyUserRejection(newer) error = %v", err)
	}
	want := projector.Snapshot()
	if _, err := projector.ApplyUserRejection(rejectionEvent(t, "older", "user-1", base), time.Minute); err != nil {
		t.Fatalf("ApplyUserRejection(older) error = %v", err)
	}
	if got := projector.Snapshot(); got != want {
		t.Fatalf("older rejection changed snapshot: got %#v, want %#v", got, want)
	}
}

func rejectionEvent(t *testing.T, id, subjectID string, occurredAt time.Time) event.SemanticEvent {
	t.Helper()
	result, err := event.NewUserRejected(id, subjectID, occurredAt, "trace-"+id)
	if err != nil {
		t.Fatalf("NewUserRejected() error = %v", err)
	}
	return result
}
