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

func TestTrackerAcceptsReplyOnlyInsideActiveResponseWindow(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		subjectID string
		at        time.Time
	}{
		{name: "before window", subjectID: "user-1", at: openedAt.Add(-time.Nanosecond)},
		{name: "at deadline", subjectID: "user-1", at: openedAt.Add(8 * time.Second)},
		{name: "after deadline", subjectID: "user-1", at: openedAt.Add(9 * time.Second)},
		{name: "wrong subject", subjectID: "user-2", at: openedAt.Add(time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := trackerWithOpenWindow(t, openedAt)
			invalid := replyEvent("invalid", test.subjectID, test.at)
			got, applied, err := tracker.Accept("episode-1", invalid)
			if err != nil {
				t.Fatalf("Accept(invalid) error = %v", err)
			}
			if !applied || got != nil {
				t.Fatalf("Accept(invalid) = (%#v, %t), want applied fact without outcome", got, applied)
			}

			accepted, applied, err := tracker.Accept("episode-1", replyEvent("valid", "user-1", openedAt.Add(time.Second)))
			if err != nil {
				t.Fatalf("Accept(valid) error = %v", err)
			}
			if !applied || accepted == nil || accepted.Kind != Accepted || accepted.Reason != ReasonUserReplied {
				t.Fatalf("Accept(valid) = (%#v, %t), want ACCEPTED/USER_REPLIED", accepted, applied)
			}
			if accepted.TriggerEventID != "event-1" || accepted.FeedbackEventID != "valid" || accepted.TraceID != "trace-1" || accepted.FeedbackTraceID != "trace-valid" {
				t.Fatalf("accepted correlation = %#v", accepted)
			}
		})
	}
}

func TestTrackerOpenResponseWindowValidatesOwnershipAndTiming(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		withActive bool
		episodeID  string
		token      string
		openedAt   time.Time
		duration   time.Duration
		code       fault.Code
	}{
		{name: "no active episode", episodeID: "episode-1", token: "wakeup-1", openedAt: startedAt, duration: time.Second, code: fault.PolicyBlocked},
		{name: "wrong episode", withActive: true, episodeID: "episode-other", token: "wakeup-1", openedAt: startedAt, duration: time.Second, code: fault.InvalidInput},
		{name: "missing token", withActive: true, episodeID: "episode-1", openedAt: startedAt, duration: time.Second, code: fault.InvalidInput},
		{name: "zero duration", withActive: true, episodeID: "episode-1", token: "wakeup-1", openedAt: startedAt, code: fault.InvalidInput},
		{name: "negative duration", withActive: true, episodeID: "episode-1", token: "wakeup-1", openedAt: startedAt, duration: -time.Second, code: fault.InvalidInput},
		{name: "before episode", withActive: true, episodeID: "episode-1", token: "wakeup-1", openedAt: startedAt.Add(-time.Nanosecond), duration: time.Second, code: fault.InvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewTracker()
			if test.withActive {
				tracker = trackerWithEpisode(t, startedAt)
			}
			err := tracker.OpenResponseWindow(test.episodeID, test.token, test.openedAt, test.duration)
			if !fault.IsCode(err, test.code) {
				t.Fatalf("OpenResponseWindow() error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestTrackerOpenResponseWindowIsIdempotentButDoesNotReplaceWindow(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := trackerWithEpisode(t, openedAt.Add(-time.Second))
	if err := tracker.OpenResponseWindow("episode-1", "wakeup-1", openedAt, 8*time.Second); err != nil {
		t.Fatalf("OpenResponseWindow() error = %v", err)
	}
	if err := tracker.OpenResponseWindow("episode-1", "wakeup-1", openedAt, 8*time.Second); err != nil {
		t.Fatalf("OpenResponseWindow(same) error = %v", err)
	}
	if err := tracker.OpenResponseWindow("episode-1", "wakeup-1", openedAt, 9*time.Second); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("OpenResponseWindow(replacement) error = %v, want PolicyBlocked", err)
	}
}

func TestTrackerAcceptIsIdempotentWithoutEndingOnInvalidReply(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := trackerWithOpenWindow(t, openedAt)
	late := replyEvent("late", "user-1", openedAt.Add(9*time.Second))
	if _, _, err := tracker.Accept("episode-1", late); err != nil {
		t.Fatalf("Accept(late) error = %v", err)
	}
	got, applied, err := tracker.Accept("episode-1", late)
	if err != nil {
		t.Fatalf("Accept(duplicate) error = %v", err)
	}
	if applied || got != nil {
		t.Fatalf("Accept(duplicate) = (%#v, %t), want no change", got, applied)
	}
	accepted, _, err := tracker.Accept("episode-1", replyEvent("valid", "user-1", openedAt.Add(time.Second)))
	if err != nil || accepted == nil {
		t.Fatalf("Accept(valid) = %#v, %v", accepted, err)
	}
}

func TestTrackerAcceptRejectsMismatchedExpectedEpisodeWithoutMutation(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := trackerWithOpenWindow(t, openedAt)
	_, applied, err := tracker.Accept("episode-other", replyEvent("wrong-expected", "user-1", openedAt.Add(time.Second)))
	if !fault.IsCode(err, fault.InvalidInput) || applied {
		t.Fatalf("Accept(wrong expected) = applied %t, error %v; want InvalidInput without mutation", applied, err)
	}

	accepted, applied, err := tracker.Accept("episode-1", replyEvent("valid-after-mismatch", "user-1", openedAt.Add(2*time.Second)))
	if err != nil || !applied || accepted == nil || accepted.Kind != Accepted {
		t.Fatalf("Accept(valid) = (%#v, %t), %v", accepted, applied, err)
	}
}

func TestTrackerExplicitRejectionEndsEpisodeBeforeOrAfterWindowOpens(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	for _, openWindow := range []bool{false, true} {
		t.Run(map[bool]string{false: "before window", true: "after window"}[openWindow], func(t *testing.T) {
			tracker := trackerWithEpisode(t, startedAt)
			if openWindow {
				if err := tracker.OpenResponseWindow("episode-1", "wakeup-1", startedAt.Add(time.Second), 8*time.Second); err != nil {
					t.Fatalf("OpenResponseWindow() error = %v", err)
				}
			}
			got, applied, err := tracker.Reject(userRejection(t, "rejection", startedAt.Add(2*time.Second)))
			if err != nil || !applied || got == nil || got.Kind != Rejected {
				t.Fatalf("Reject() = (%#v, %t), %v", got, applied, err)
			}
		})
	}
}

func TestTrackerExpiresResponseWindowAtOrAfterDeadline(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tracker := trackerWithOpenWindow(t, openedAt)
	before := responseExpiredEvent(t, "expiry", "episode-1", "wakeup-1", "user-1", openedAt.Add(8*time.Second-time.Nanosecond))
	got, applied, err := tracker.Expire("episode-1", before)
	if err != nil || applied || got != nil {
		t.Fatalf("Expire(before deadline) = (%#v, %t), %v", got, applied, err)
	}

	atDeadline := responseExpiredEvent(t, "expiry", "episode-1", "wakeup-1", "user-1", openedAt.Add(8*time.Second))
	got, applied, err = tracker.Expire("episode-1", atDeadline)
	if err != nil || !applied || got == nil || got.Kind != NoResponse || got.Reason != ReasonResponseWindowElapsed {
		t.Fatalf("Expire(deadline) = (%#v, %t), %v", got, applied, err)
	}
	if got.TriggerEventID != "event-1" || got.FeedbackEventID != atDeadline.ID || got.TraceID != "trace-1" || got.FeedbackTraceID != atDeadline.TraceID {
		t.Fatalf("NO_RESPONSE correlation = %#v", got)
	}

	duplicate, applied, err := tracker.Expire("episode-1", atDeadline)
	if err != nil || applied || duplicate != nil {
		t.Fatalf("Expire(duplicate) = (%#v, %t), %v", duplicate, applied, err)
	}
	collision := atDeadline
	collision.WakeupToken = "wakeup-other"
	if _, applied, err := tracker.Expire("episode-1", collision); !fault.IsCode(err, fault.InvalidInput) || applied {
		t.Fatalf("Expire(collision) = applied %t, error %v; want InvalidInput", applied, err)
	}
}

func TestTrackerExpireRejectsWrongCorrelationWithoutEndingEpisode(t *testing.T) {
	openedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		expectedID string
		episodeID  string
		token      string
		subjectID  string
	}{
		{name: "empty expected", episodeID: "episode-1", token: "wakeup-1", subjectID: "user-1"},
		{name: "wrong expected", expectedID: "episode-other", episodeID: "episode-1", token: "wakeup-1", subjectID: "user-1"},
		{name: "wrong event episode", expectedID: "episode-1", episodeID: "episode-other", token: "wakeup-1", subjectID: "user-1"},
		{name: "wrong token", expectedID: "episode-1", episodeID: "episode-1", token: "wakeup-other", subjectID: "user-1"},
		{name: "wrong subject", expectedID: "episode-1", episodeID: "episode-1", token: "wakeup-1", subjectID: "user-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := trackerWithOpenWindow(t, openedAt)
			input := responseExpiredEvent(t, "invalid", test.episodeID, test.token, test.subjectID, openedAt.Add(8*time.Second))
			if _, applied, err := tracker.Expire(test.expectedID, input); err == nil || applied {
				t.Fatalf("Expire(invalid) = applied %t, error %v", applied, err)
			}
			valid := responseExpiredEvent(t, "valid", "episode-1", "wakeup-1", "user-1", openedAt.Add(8*time.Second))
			got, applied, err := tracker.Expire("episode-1", valid)
			if err != nil || !applied || got == nil || got.Kind != NoResponse {
				t.Fatalf("Expire(valid) = (%#v, %t), %v", got, applied, err)
			}
		})
	}
}

func trackerWithOpenWindow(t *testing.T, openedAt time.Time) *Tracker {
	t.Helper()
	tracker := trackerWithEpisode(t, openedAt.Add(-time.Second))
	if err := tracker.OpenResponseWindow("episode-1", "wakeup-1", openedAt, 8*time.Second); err != nil {
		t.Fatalf("OpenResponseWindow() error = %v", err)
	}
	return tracker
}

func responseExpiredEvent(t *testing.T, id, episodeID, token, subjectID string, occurredAt time.Time) event.SemanticEvent {
	t.Helper()
	result, err := event.NewResponseWindowExpired(id, episodeID, token, subjectID, occurredAt, "trace-"+id)
	if err != nil {
		t.Fatalf("NewResponseWindowExpired() error = %v", err)
	}
	return result
}

func trackerWithEpisode(t *testing.T, startedAt time.Time) *Tracker {
	t.Helper()
	tracker := NewTracker()
	if err := tracker.Start(Episode{
		ID:             "episode-1",
		InteractionID:  "interaction-1",
		SubjectID:      "user-1",
		DecisionID:     "decision-1",
		TriggerEventID: "event-1",
		TraceID:        "trace-1",
		ConfigHash:     "config-1",
		StartedAt:      startedAt,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return tracker
}

func replyEvent(id, subjectID string, occurredAt time.Time) event.SemanticEvent {
	return event.SemanticEvent{
		ID:         id,
		Kind:       event.UserReplied,
		SubjectID:  subjectID,
		OccurredAt: occurredAt,
		TraceID:    "trace-" + id,
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
