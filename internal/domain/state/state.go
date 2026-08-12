package state

import (
	"errors"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"
)

// WorldSnapshot is an immutable-by-convention value copy of current state.
type WorldSnapshot struct {
	SubjectID                   string
	Version                     uint64
	UpdatedAt                   time.Time
	PersonPresent               bool
	UserBusy                    bool
	BusyReason                  observation.BusyReason
	QuietMode                   bool
	RejectionCooldownStartedAt  time.Time
	RejectionCooldownUntil      time.Time
	NoResponseCooldownStartedAt time.Time
	NoResponseCooldownUntil     time.Time
	LastReturnedAt              time.Time
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

// ApplyUserRejection projects one validated user-feedback fact. The caller
// remains responsible for preserving the Projector's single-writer ownership.
func (p *Projector) ApplyUserRejection(input event.SemanticEvent, cooldown time.Duration) (WorldSnapshot, error) {
	if err := input.ValidateUserRejection(); err != nil {
		return p.snapshot, err
	}
	if cooldown <= 0 {
		return p.snapshot, fault.New(fault.InvalidInput, "project user rejection", errors.New("cooldown must be positive"))
	}
	if input.SubjectID != p.snapshot.SubjectID {
		return p.snapshot, nil
	}
	if !p.snapshot.RejectionCooldownStartedAt.IsZero() && input.OccurredAt.Before(p.snapshot.RejectionCooldownStartedAt) {
		return p.snapshot, nil
	}
	p.snapshot.RejectionCooldownStartedAt = input.OccurredAt
	p.snapshot.RejectionCooldownUntil = input.OccurredAt.Add(cooldown)
	p.snapshot.Version++
	p.snapshot.UpdatedAt = input.OccurredAt
	return p.snapshot, nil
}

// ApplyNoResponse projects the independent cooldown caused by an unanswered
// proactive interaction.
func (p *Projector) ApplyNoResponse(input event.SemanticEvent, cooldown time.Duration) (WorldSnapshot, error) {
	if err := input.ValidateResponseWindowExpired(); err != nil {
		return p.snapshot, err
	}
	if cooldown <= 0 {
		return p.snapshot, fault.New(fault.InvalidInput, "project no response", errors.New("cooldown must be positive"))
	}
	if input.SubjectID != p.snapshot.SubjectID {
		return p.snapshot, nil
	}
	if !p.snapshot.NoResponseCooldownStartedAt.IsZero() && input.OccurredAt.Before(p.snapshot.NoResponseCooldownStartedAt) {
		return p.snapshot, nil
	}
	p.snapshot.NoResponseCooldownStartedAt = input.OccurredAt
	p.snapshot.NoResponseCooldownUntil = input.OccurredAt.Add(cooldown)
	p.snapshot.Version++
	p.snapshot.UpdatedAt = input.OccurredAt
	return p.snapshot, nil
}
