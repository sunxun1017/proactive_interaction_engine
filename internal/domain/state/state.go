package state

import (
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/observation"
)

// WorldSnapshot is an immutable-by-convention value copy of current state.
type WorldSnapshot struct {
	SubjectID       string
	Version         uint64
	UpdatedAt       time.Time
	PersonPresent   bool
	UserBusy        bool
	BusyReason      observation.BusyReason
	QuietMode       bool
	RecentRejection bool
	LastReturnedAt  time.Time
}

// Projector is the single writer for its world snapshot.
type Projector struct {
	snapshot WorldSnapshot
}

func NewProjector(subjectID string) *Projector {
	return &Projector{snapshot: WorldSnapshot{SubjectID: subjectID}}
}

// Apply projects one semantic event and returns a value copy.
func (p *Projector) Apply(input event.SemanticEvent) WorldSnapshot {
	if input.SubjectID != p.snapshot.SubjectID {
		return p.snapshot
	}

	switch input.Kind {
	case event.PersonLeft:
		p.snapshot.PersonPresent = false
	case event.PersonDetected:
		p.snapshot.PersonPresent = true
	case event.PersonReturned:
		p.snapshot.PersonPresent = true
		p.snapshot.LastReturnedAt = input.OccurredAt
	case event.UserBecameBusy:
		p.snapshot.UserBusy = true
		p.snapshot.BusyReason = input.BusyReason
	case event.UserBecameAvailable:
		p.snapshot.UserBusy = false
		p.snapshot.BusyReason = ""
	case event.QuietModeEnabled:
		p.snapshot.QuietMode = true
	case event.QuietModeDisabled:
		p.snapshot.QuietMode = false
	}

	p.snapshot.Version++
	p.snapshot.UpdatedAt = input.OccurredAt
	return p.snapshot
}

// Snapshot returns a value copy and never exposes mutable projector state.
func (p *Projector) Snapshot() WorldSnapshot { return p.snapshot }
