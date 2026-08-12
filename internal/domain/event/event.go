package event

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

type Kind string

const (
	PersonLeft          Kind = "PERSON_LEFT"
	PersonDetected      Kind = "PERSON_DETECTED"
	PersonReturned      Kind = "PERSON_RETURNED"
	UserBecameBusy      Kind = "USER_BECAME_BUSY"
	UserBecameAvailable Kind = "USER_BECAME_AVAILABLE"
	QuietModeEnabled    Kind = "QUIET_MODE_ENABLED"
	QuietModeDisabled   Kind = "QUIET_MODE_DISABLED"
	UserRejected        Kind = "USER_REJECTED"
)

// SemanticEvent is a fact emitted after temporal compilation.
type SemanticEvent struct {
	ID                  string
	Kind                Kind
	SubjectID           string
	OccurredAt          time.Time
	Confidence          float32
	TraceID             string
	SourceObservationID string
	AbsenceDuration     time.Duration
	BusyReason          observation.BusyReason
}

// NewUserRejected constructs the semantic fact derived from an explicit user
// control. Unlike a control command, this value records what already happened.
func NewUserRejected(id, subjectID string, occurredAt time.Time, traceID string) (SemanticEvent, error) {
	input := SemanticEvent{
		ID:         id,
		Kind:       UserRejected,
		SubjectID:  subjectID,
		OccurredAt: occurredAt,
		TraceID:    traceID,
	}
	if err := input.ValidateUserRejection(); err != nil {
		return SemanticEvent{}, err
	}
	return input, nil
}

// ValidateUserRejection validates fields required by the user-feedback path
// without imposing observation-source fields on control-derived facts.
func (e SemanticEvent) ValidateUserRejection() error {
	const op = "validate user rejection event"
	if e.Kind != UserRejected {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("event kind %q is not USER_REJECTED", e.Kind))
	}
	if e.ID == "" || e.SubjectID == "" || e.TraceID == "" {
		return fault.New(fault.InvalidInput, op, errors.New("id, subject_id, and trace_id are required"))
	}
	if e.OccurredAt.IsZero() {
		return fault.New(fault.InvalidInput, op, errors.New("occurred_at is required"))
	}
	return nil
}

type subjectTimeline struct {
	presenceKnown bool
	present       bool
	absentSince   time.Time
	busyKnown     bool
	busy          bool
	quietKnown    bool
	quiet         bool
}

// Compiler performs deterministic temporal compilation. Call it from a single
// engine goroutine; it is intentionally not concurrency-safe.
type Compiler struct {
	returnAbsenceThreshold time.Duration
	subjects               map[string]subjectTimeline
}

func NewCompiler(returnAbsenceThreshold time.Duration) *Compiler {
	return &Compiler{
		returnAbsenceThreshold: returnAbsenceThreshold,
		subjects:               make(map[string]subjectTimeline),
	}
}

// Compile returns zero or one semantic transition for the current payload.
func (c *Compiler) Compile(input observation.Observation) []SemanticEvent {
	timeline := c.subjects[input.SubjectID]
	var output *SemanticEvent

	switch {
	case input.PersonPresence != nil:
		value := input.PersonPresence.Present
		if !timeline.presenceKnown || value != timeline.present {
			kind := PersonDetected
			var absence time.Duration
			if !value {
				kind = PersonLeft
				timeline.absentSince = input.OccurredAt
			} else if timeline.presenceKnown && !timeline.present {
				absence = input.OccurredAt.Sub(timeline.absentSince)
				if absence >= c.returnAbsenceThreshold {
					kind = PersonReturned
				}
			}
			output = newEvent(input, kind)
			output.AbsenceDuration = absence
		}
		timeline.presenceKnown = true
		timeline.present = value

	case input.UserBusy != nil:
		value := input.UserBusy.Busy
		if !timeline.busyKnown || value != timeline.busy {
			kind := UserBecameAvailable
			if value {
				kind = UserBecameBusy
			}
			output = newEvent(input, kind)
			output.BusyReason = input.UserBusy.Reason
		}
		timeline.busyKnown = true
		timeline.busy = value

	case input.QuietMode != nil:
		value := input.QuietMode.Enabled
		if !timeline.quietKnown || value != timeline.quiet {
			kind := QuietModeDisabled
			if value {
				kind = QuietModeEnabled
			}
			output = newEvent(input, kind)
		}
		timeline.quietKnown = true
		timeline.quiet = value
	}

	c.subjects[input.SubjectID] = timeline
	if output == nil {
		return nil
	}
	return []SemanticEvent{*output}
}

func newEvent(input observation.Observation, kind Kind) *SemanticEvent {
	return &SemanticEvent{
		ID:                  fmt.Sprintf("evt:%s:%s", input.ID, kind),
		Kind:                kind,
		SubjectID:           input.SubjectID,
		OccurredAt:          input.OccurredAt,
		Confidence:          input.Confidence,
		TraceID:             input.TraceID,
		SourceObservationID: input.ID,
	}
}
