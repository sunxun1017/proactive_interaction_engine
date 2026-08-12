package decision

import (
	"errors"
	"fmt"
	"time"

	"proactive-interaction-engine/internal/domain/event"
	"proactive-interaction-engine/internal/domain/state"
)

type OpportunityKind string

const WelcomeAfterReturn OpportunityKind = "WELCOME_AFTER_RETURN"

type Opportunity struct {
	Kind    OpportunityKind
	EventID string
}

type Kind string

const (
	Silent     Kind = "SILENT"
	GreetShort Kind = "GREET_SHORT"
)

type Style string

const (
	StyleNone Style = "NONE"
	StyleWarm Style = "WARM"
)

type ReasonCode string

const (
	ReasonLongAbsence     ReasonCode = "LONG_ABSENCE"
	ReasonUserAttending   ReasonCode = "USER_ATTENDING"
	ReasonNoRecentReject  ReasonCode = "NO_RECENT_REJECTION"
	ReasonQuietMode       ReasonCode = "QUIET_MODE"
	ReasonUserOnCall      ReasonCode = "USER_ON_CALL"
	ReasonUserBusy        ReasonCode = "USER_BUSY"
	ReasonRecentRejection ReasonCode = "RECENT_REJECTION"
)

// Decision is a deterministic, auditable policy output. Silent is first class.
type Decision struct {
	ID              string
	InteractionID   string
	Kind            Kind
	Style           Style
	ReasonCodes     []ReasonCode
	EventID         string
	TraceID         string
	PolicyVersion   string
	BehaviorVersion string
	ConfigHash      string
	SnapshotHash    string
	ModelVersion    string
	RandomSeed      int64
}

type Input struct {
	Snapshot    state.WorldSnapshot
	Opportunity Opportunity
	Event       event.SemanticEvent
	ConfigHash  string
}

// Policy is deliberately pure: identical inputs produce identical output.
type Policy interface {
	Decide(Input) Decision
}

type RulePolicy struct {
	Version         string
	BehaviorVersion string
	RandomSeed      int64
}

func (p RulePolicy) Decide(input Input) Decision {
	decisionID := "decision:" + input.Event.ID
	interactionID := "interaction:" + input.Event.ID
	base := Decision{
		ID:              decisionID,
		InteractionID:   interactionID,
		EventID:         input.Event.ID,
		TraceID:         input.Event.TraceID,
		PolicyVersion:   p.Version,
		BehaviorVersion: p.BehaviorVersion,
		ConfigHash:      input.ConfigHash,
		SnapshotHash:    SnapshotHash(input.Snapshot),
		ModelVersion:    "none",
		RandomSeed:      p.RandomSeed,
	}

	if reasons := hardGuard(input.Snapshot, input.Event.OccurredAt); len(reasons) > 0 {
		base.Kind = Silent
		base.Style = StyleNone
		base.ReasonCodes = reasons
		return base
	}

	base.Kind = GreetShort
	base.Style = StyleWarm
	base.ReasonCodes = []ReasonCode{
		ReasonLongAbsence,
		ReasonUserAttending,
		ReasonNoRecentReject,
	}
	return base
}

// Evaluate runs the fixed opportunity -> guard/policy -> validation pipeline.
func Evaluate(policy Policy, snapshot state.WorldSnapshot, input event.SemanticEvent, configHash string) (*Decision, error) {
	opportunity, ok := DetectOpportunity(input)
	if !ok {
		return nil, nil
	}
	result := policy.Decide(Input{
		Snapshot:    snapshot,
		Opportunity: opportunity,
		Event:       input,
		ConfigHash:  configHash,
	})
	if err := Validate(result); err != nil {
		return nil, fmt.Errorf("validate decision %s: %w", result.ID, err)
	}
	return &result, nil
}

func DetectOpportunity(input event.SemanticEvent) (Opportunity, bool) {
	if input.Kind != event.PersonReturned {
		return Opportunity{}, false
	}
	return Opportunity{Kind: WelcomeAfterReturn, EventID: input.ID}, true
}

func hardGuard(snapshot state.WorldSnapshot, eventTime time.Time) []ReasonCode {
	if snapshot.QuietMode {
		return []ReasonCode{ReasonQuietMode}
	}
	if snapshot.UserBusy {
		if snapshot.BusyReason == "ON_CALL" {
			return []ReasonCode{ReasonUserOnCall}
		}
		return []ReasonCode{ReasonUserBusy}
	}
	if !snapshot.RejectionCooldownStartedAt.IsZero() &&
		snapshot.RejectionCooldownUntil.After(snapshot.RejectionCooldownStartedAt) &&
		!eventTime.Before(snapshot.RejectionCooldownStartedAt) &&
		eventTime.Before(snapshot.RejectionCooldownUntil) {
		return []ReasonCode{ReasonRecentRejection}
	}
	return nil
}

func Validate(input Decision) error {
	if input.ID == "" || input.EventID == "" || input.InteractionID == "" || input.TraceID == "" {
		return errors.New("decision identity and correlation fields are required")
	}
	if input.Kind != Silent && input.Kind != GreetShort {
		return fmt.Errorf("unsupported decision kind %q", input.Kind)
	}
	if len(input.ReasonCodes) == 0 {
		return errors.New("reason codes are required")
	}
	if input.Kind == Silent && input.Style != StyleNone {
		return errors.New("silent decision must not specify an expressive style")
	}
	return nil
}

// SnapshotHash is a stable, dependency-free representation for replay audits.
func SnapshotHash(snapshot state.WorldSnapshot) string {
	return fmt.Sprintf(
		"subject=%s;version=%d;present=%t;busy=%t;busy_reason=%s;quiet=%t;rejection_started_at=%s;rejection_until=%s",
		snapshot.SubjectID,
		snapshot.Version,
		snapshot.PersonPresent,
		snapshot.UserBusy,
		snapshot.BusyReason,
		snapshot.QuietMode,
		snapshot.RejectionCooldownStartedAt.UTC().Format(time.RFC3339Nano),
		snapshot.RejectionCooldownUntil.UTC().Format(time.RFC3339Nano),
	)
}
