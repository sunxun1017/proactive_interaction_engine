package observation

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

// Observation is the canonical ingress value produced by input adapters.
// Exactly one payload pointer must be non-nil.
type Observation struct {
	ID         string
	SourceID   string
	SourceSeq  uint64
	OccurredAt time.Time
	TTL        time.Duration
	SubjectID  string
	Confidence float32
	TraceID    string

	PersonPresence *PersonPresence
	UserBusy       *UserBusy
	QuietMode      *QuietMode
	UserReply      *UserReply
}

type PersonPresence struct {
	Present bool
}

type BusyReason string

const (
	BusyUnknown BusyReason = "UNKNOWN"
	BusyOnCall  BusyReason = "ON_CALL"
	BusyFocused BusyReason = "FOCUSED"
)

type UserBusy struct {
	Busy   bool
	Reason BusyReason
}

type QuietMode struct {
	Enabled bool
}

// UserReply is a canonical signal already classified by its adapter as
// directed at the agent. It deliberately contains no audio or transcript.
type UserReply struct{}

// ValidateAt enforces ingress invariants and TTL against the injected clock.
func (o Observation) ValidateAt(now time.Time) error {
	const op = "validate observation"
	if o.ID == "" || o.SourceID == "" || o.SubjectID == "" || o.TraceID == "" {
		return fault.New(fault.InvalidInput, op, errors.New("id, source_id, subject_id, and trace_id are required"))
	}
	if o.SourceSeq == 0 {
		return fault.New(fault.InvalidInput, op, errors.New("source_seq must be positive"))
	}
	if o.OccurredAt.IsZero() {
		return fault.New(fault.InvalidInput, op, errors.New("occurred_at is required"))
	}
	if o.TTL <= 0 {
		return fault.New(fault.InvalidInput, op, errors.New("ttl must be positive"))
	}
	if o.Confidence < 0 || o.Confidence > 1 {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("confidence %.3f is outside [0,1]", o.Confidence))
	}
	if now.After(o.OccurredAt.Add(o.TTL)) {
		return fault.New(fault.StaleInput, op, fmt.Errorf("observation %s expired at %s", o.ID, o.OccurredAt.Add(o.TTL).UTC().Format(time.RFC3339Nano)))
	}

	payloads := 0
	for _, present := range []bool{o.PersonPresence != nil, o.UserBusy != nil, o.QuietMode != nil, o.UserReply != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("exactly one payload is required, got %d", payloads))
	}
	if o.UserBusy != nil && o.UserBusy.Busy && o.UserBusy.Reason == "" {
		return fault.New(fault.InvalidInput, op, errors.New("busy reason is required when user is busy"))
	}
	return nil
}
