package episode

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
)

// Episode identifies the proactive interaction currently awaiting feedback.
type Episode struct {
	ID             string
	InteractionID  string
	SubjectID      string
	DecisionID     string
	TriggerEventID string
	TraceID        string
	ConfigHash     string
	StartedAt      time.Time
}

func (e Episode) Validate() error {
	const op = "validate interaction episode"
	if e.ID == "" || e.InteractionID == "" || e.SubjectID == "" || e.DecisionID == "" || e.TriggerEventID == "" || e.TraceID == "" || e.ConfigHash == "" {
		return fault.New(fault.InvalidInput, op, errors.New("identity, correlation, subject, and config fields are required"))
	}
	if e.StartedAt.IsZero() {
		return fault.New(fault.InvalidInput, op, errors.New("started_at is required"))
	}
	return nil
}

type OutcomeKind string

const Rejected OutcomeKind = "REJECTED"

const Accepted OutcomeKind = "ACCEPTED"

type OutcomeReason string

const ReasonExplicitUserRejection OutcomeReason = "EXPLICIT_USER_REJECTION"

const ReasonUserReplied OutcomeReason = "USER_REPLIED"

type ResponseWindow struct {
	OpenedAt time.Time
	Deadline time.Time
}

// Outcome is an immutable evaluation of user feedback for one episode.
type Outcome struct {
	ID              string
	Kind            OutcomeKind
	Reason          OutcomeReason
	EpisodeID       string
	InteractionID   string
	SubjectID       string
	DecisionID      string
	TriggerEventID  string
	FeedbackEventID string
	TraceID         string
	FeedbackTraceID string
	ConfigHash      string
	OccurredAt      time.Time
}

func (o Outcome) Validate() error {
	const op = "validate interaction outcome"
	validKindReason := (o.Kind == Rejected && o.Reason == ReasonExplicitUserRejection) ||
		(o.Kind == Accepted && o.Reason == ReasonUserReplied)
	if !validKindReason {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported outcome %q/%q", o.Kind, o.Reason))
	}
	if o.ID == "" || o.EpisodeID == "" || o.InteractionID == "" || o.SubjectID == "" || o.DecisionID == "" ||
		o.TriggerEventID == "" || o.FeedbackEventID == "" || o.TraceID == "" || o.FeedbackTraceID == "" || o.ConfigHash == "" {
		return fault.New(fault.InvalidInput, op, errors.New("identity, correlation, subject, trace, and config fields are required"))
	}
	if o.OccurredAt.IsZero() {
		return fault.New(fault.InvalidInput, op, errors.New("occurred_at is required"))
	}
	return nil
}

// Tracker owns at most one active interaction. Call it from the same
// single-writer application path that owns WorldState.
type Tracker struct {
	active                  *Episode
	responseWindow          *ResponseWindow
	processedFeedbackEvents map[string]struct{}
}

func NewTracker() *Tracker {
	return &Tracker{processedFeedbackEvents: make(map[string]struct{})}
}

// Start makes the supplied episode the active feedback target. Repeating the
// same value is idempotent; a different active episode must end first.
func (t *Tracker) Start(input Episode) error {
	if err := input.Validate(); err != nil {
		return err
	}
	if t.active != nil {
		if *t.active == input {
			return nil
		}
		return fault.New(
			fault.PolicyBlocked,
			"start interaction episode",
			fmt.Errorf("episode %s is still active", t.active.ID),
		)
	}
	value := input
	t.active = &value
	return nil
}

// OpenResponseWindow adds the one supported response window to the active
// episode. The same window is idempotent; replacement is explicit failure.
func (t *Tracker) OpenResponseWindow(episodeID string, openedAt time.Time, duration time.Duration) error {
	const op = "open episode response window"
	if t.active == nil {
		return fault.New(fault.PolicyBlocked, op, errors.New("no active episode"))
	}
	if episodeID == "" || episodeID != t.active.ID {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("episode %q is not active", episodeID))
	}
	if duration <= 0 {
		return fault.New(fault.InvalidInput, op, errors.New("duration must be positive"))
	}
	if openedAt.IsZero() || openedAt.Before(t.active.StartedAt) {
		return fault.New(fault.InvalidInput, op, errors.New("opened_at must not precede episode start"))
	}
	window := ResponseWindow{OpenedAt: openedAt, Deadline: openedAt.Add(duration)}
	if t.responseWindow != nil {
		if *t.responseWindow == window {
			return nil
		}
		return fault.New(fault.PolicyBlocked, op, errors.New("response window is already open"))
	}
	t.responseWindow = &window
	return nil
}

// Accept evaluates one USER_REPLIED fact against the active half-open response
// window. Invalid-time and wrong-subject facts never end the active episode.
func (t *Tracker) Accept(expectedEpisodeID string, input event.SemanticEvent) (*Outcome, bool, error) {
	if err := input.ValidateUserReply(); err != nil {
		return nil, false, err
	}
	if expectedEpisodeID == "" {
		return nil, false, fault.New(fault.InvalidInput, "accept user reply", errors.New("expected episode id is required"))
	}
	if t.active == nil {
		return nil, false, fault.New(fault.PolicyBlocked, "accept user reply", errors.New("no active episode"))
	}
	if expectedEpisodeID != t.active.ID {
		return nil, false, fault.New(
			fault.InvalidInput,
			"accept user reply",
			fmt.Errorf("expected episode %s does not match active episode %s", expectedEpisodeID, t.active.ID),
		)
	}
	if _, exists := t.processedFeedbackEvents[input.ID]; exists {
		return nil, false, nil
	}
	t.processedFeedbackEvents[input.ID] = struct{}{}
	if t.responseWindow == nil || t.active.SubjectID != input.SubjectID {
		return nil, true, nil
	}
	if input.OccurredAt.Before(t.responseWindow.OpenedAt) || !input.OccurredAt.Before(t.responseWindow.Deadline) {
		return nil, true, nil
	}

	active := *t.active
	t.active = nil
	t.responseWindow = nil
	outcome := Outcome{
		ID:              fmt.Sprintf("outcome:%s", input.ID),
		Kind:            Accepted,
		Reason:          ReasonUserReplied,
		EpisodeID:       active.ID,
		InteractionID:   active.InteractionID,
		SubjectID:       active.SubjectID,
		DecisionID:      active.DecisionID,
		TriggerEventID:  active.TriggerEventID,
		FeedbackEventID: input.ID,
		TraceID:         active.TraceID,
		FeedbackTraceID: input.TraceID,
		ConfigHash:      active.ConfigHash,
		OccurredAt:      input.OccurredAt,
	}
	if err := outcome.Validate(); err != nil {
		return nil, false, err
	}
	return &outcome, true, nil
}

// Reject applies a rejection fact once. A rejection without an active episode
// is still applied so callers can project cooldown, but it creates no outcome.
func (t *Tracker) Reject(input event.SemanticEvent) (*Outcome, bool, error) {
	if err := input.ValidateUserRejection(); err != nil {
		return nil, false, err
	}
	if _, exists := t.processedFeedbackEvents[input.ID]; exists {
		return nil, false, nil
	}
	t.processedFeedbackEvents[input.ID] = struct{}{}

	if t.active == nil || t.active.SubjectID != input.SubjectID {
		return nil, true, nil
	}
	if input.OccurredAt.Before(t.active.StartedAt) {
		return nil, true, nil
	}
	active := *t.active
	t.active = nil
	t.responseWindow = nil
	outcome := Outcome{
		ID:              fmt.Sprintf("outcome:%s", input.ID),
		Kind:            Rejected,
		Reason:          ReasonExplicitUserRejection,
		EpisodeID:       active.ID,
		InteractionID:   active.InteractionID,
		SubjectID:       active.SubjectID,
		DecisionID:      active.DecisionID,
		TriggerEventID:  active.TriggerEventID,
		FeedbackEventID: input.ID,
		TraceID:         active.TraceID,
		FeedbackTraceID: input.TraceID,
		ConfigHash:      active.ConfigHash,
		OccurredAt:      input.OccurredAt,
	}
	if err := outcome.Validate(); err != nil {
		return nil, false, err
	}
	return &outcome, true, nil
}
