package episode

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestTrackerRejectsActiveEpisodeOnce(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := NewTracker()
	active := Episode{
		ID:             "episode-1",
		InteractionID:  "interaction-1",
		SubjectID:      "user-1",
		DecisionID:     "decision-1",
		TriggerEventID: "event-1",
		TraceID:        "trace-1",
		ConfigHash:     "config-1",
		StartedAt:      startedAt,
	}
	if err := tracker.Start(active); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	rejection := userRejection(t, "rejection-1", startedAt.Add(time.Minute))

	got, applied, err := tracker.Reject(rejection)
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if !applied || got == nil || got.Kind != Rejected {
		t.Fatalf("Reject() = (%#v, %t), want one REJECTED outcome", got, applied)
	}
	if got.InteractionID != "interaction-1" || got.FeedbackEventID != rejection.ID {
		t.Fatalf("Outcome correlation = %#v", got)
	}
	if got.TraceID != active.TraceID || got.FeedbackTraceID != rejection.TraceID {
		t.Fatalf("Outcome traces = %#v", got)
	}

	duplicate, applied, err := tracker.Reject(rejection)
	if err != nil {
		t.Fatalf("Reject(duplicate) error = %v", err)
	}
	if applied || duplicate != nil {
		t.Fatalf("Reject(duplicate) = (%#v, %t), want no change", duplicate, applied)
	}
}

func TestTrackerDoesNotFabricateOutcomeWithoutActiveEpisode(t *testing.T) {
	tracker := NewTracker()
	got, applied, err := tracker.Reject(userRejection(
		t,
		"rejection-1",
		time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC),
	))
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if !applied || got != nil {
		t.Fatalf("Reject() = (%#v, %t), want applied fact without outcome", got, applied)
	}
}

func TestTrackerStartIsIdempotentButDoesNotReplaceActiveEpisode(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := NewTracker()
	active := Episode{
		ID:             "episode-1",
		InteractionID:  "interaction-1",
		SubjectID:      "user-1",
		DecisionID:     "decision-1",
		TriggerEventID: "event-1",
		TraceID:        "trace-1",
		ConfigHash:     "config-1",
		StartedAt:      startedAt,
	}
	if err := tracker.Start(active); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := tracker.Start(active); err != nil {
		t.Fatalf("Start(same) error = %v", err)
	}

	replacement := active
	replacement.ID = "episode-2"
	replacement.InteractionID = "interaction-2"
	if err := tracker.Start(replacement); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("Start(replacement) error = %v, want PolicyBlocked", err)
	}

	got, applied, err := tracker.Reject(userRejection(t, "rejection-1", startedAt.Add(time.Minute)))
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if !applied || got == nil || got.EpisodeID != active.ID {
		t.Fatalf("Reject() = (%#v, %t), want original active episode", got, applied)
	}
}

func TestTrackerDoesNotEndActiveEpisodeForEarlierFeedback(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := NewTracker()
	active := Episode{
		ID:             "episode-1",
		InteractionID:  "interaction-1",
		SubjectID:      "user-1",
		DecisionID:     "decision-1",
		TriggerEventID: "event-1",
		TraceID:        "trace-1",
		ConfigHash:     "config-1",
		StartedAt:      startedAt,
	}
	if err := tracker.Start(active); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	earlier, applied, err := tracker.Reject(userRejection(t, "earlier", startedAt.Add(-time.Minute)))
	if err != nil {
		t.Fatalf("Reject(earlier) error = %v", err)
	}
	if !applied || earlier != nil {
		t.Fatalf("Reject(earlier) = (%#v, %t), want applied fact without outcome", earlier, applied)
	}

	got, applied, err := tracker.Reject(userRejection(t, "valid", startedAt.Add(time.Minute)))
	if err != nil {
		t.Fatalf("Reject(valid) error = %v", err)
	}
	if !applied || got == nil || got.EpisodeID != active.ID {
		t.Fatalf("Reject(valid) = (%#v, %t), want original active episode", got, applied)
	}
}

func TestOutcomeValidateRejectsIncompleteCorrelation(t *testing.T) {
	if err := (Outcome{Kind: Rejected, Reason: ReasonExplicitUserRejection}).Validate(); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Validate() error = %v, want InvalidInput", err)
	}
}

func userRejection(t *testing.T, id string, occurredAt time.Time) event.SemanticEvent {
	t.Helper()
	result, err := event.NewUserRejected(id, "user-1", occurredAt, "trace-"+id)
	if err != nil {
		t.Fatalf("NewUserRejected() error = %v", err)
	}
	return result
}
