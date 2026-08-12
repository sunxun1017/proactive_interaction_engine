package control

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

type Kind string

const StopAll Kind = "STOP_ALL"

type StopReason string

const (
	ReasonUserRejected StopReason = "USER_REJECTED"
	ReasonShutdown     StopReason = "SHUTDOWN"
)

// Command requests a high-priority software control operation.
type Command struct {
	ID         string
	Kind       Kind
	Reason     StopReason
	SubjectID  string
	OccurredAt time.Time
	TraceID    string
}

func (c Command) Validate() error {
	const op = "validate control command"
	if c.ID == "" || c.TraceID == "" {
		return fault.New(fault.InvalidInput, op, errors.New("id and trace_id are required"))
	}
	if c.Kind != StopAll {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported kind %q", c.Kind))
	}
	if c.Reason != ReasonUserRejected && c.Reason != ReasonShutdown {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("unsupported stop reason %q", c.Reason))
	}
	if c.Reason == ReasonUserRejected && (c.SubjectID == "" || c.OccurredAt.IsZero()) {
		return fault.New(fault.InvalidInput, op, errors.New("subject_id and occurred_at are required for user rejection"))
	}
	return nil
}
