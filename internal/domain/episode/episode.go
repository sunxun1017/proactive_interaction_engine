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

type OutcomeReason string

const ReasonExplicitUserRejection OutcomeReason = "EXPLICIT_USER_REJECTION"

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
	if o.Kind != Rejected || o.Reason != ReasonExplicitUserRejection {
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
