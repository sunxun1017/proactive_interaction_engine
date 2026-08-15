package identity

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	maxSpeakerVerificationChallenges = 1024
	maxSpeakerVerificationIDBytes    = 256

	configureSpeakerVerificationOp = "configure speaker verification coordinator"
	issueSpeakerVerificationOp     = "issue speaker verification challenge"
	submitSpeakerVerificationOp    = "submit speaker verification fragment"
	advanceSpeakerVerificationOp   = "advance speaker verification challenge"
)

// SpeakerVerificationCoordinatorConfig supplies explicit lifetime and capacity
// bounds. The coordinator defines no implicit defaults.
type SpeakerVerificationCoordinatorConfig struct {
	ChallengeDuration time.Duration
	MaxOpenChallenges int
}

// SpeakerVerificationChallenge contains only opaque worker-safe correlation
// values. The expected profile remains private coordinator state.
type SpeakerVerificationChallenge struct {
	ID               string
	EvidenceWindowID string
	OpenedAt         time.Time
	Deadline         time.Time
}

// Wakeup returns the exact opaque deadline owner for this challenge.
func (c SpeakerVerificationChallenge) Wakeup() SpeakerVerificationWakeup {
	return SpeakerVerificationWakeup{
		ChallengeID: c.ID, EvidenceWindowID: c.EvidenceWindowID, Deadline: c.Deadline,
	}
}

// SpeakerVerificationWakeup carries no policy, enrollment, or profile data.
type SpeakerVerificationWakeup struct {
	ChallengeID      string
	EvidenceWindowID string
	Deadline         time.Time
}

// SpeakerVerificationSubmissionCandidate is the complete candidate metadata a
// worker may return. It structurally cannot claim the expected profile.
type SpeakerVerificationSubmissionCandidate struct {
	ID           string
	Score        float64
	ModelVersion string
}

// SpeakerVerificationFragment is one worker response bound to both opaque
// application-issued tokens.
type SpeakerVerificationFragment struct {
	ChallengeID string
	WindowToken string
	FragmentID  string
	OccurredAt  time.Time
	Candidate   SpeakerVerificationSubmissionCandidate
}

// SpeakerVerificationAdvance reports whether an exact due challenge was
// consumed and resolved. Wrong, stale scheduler wakeups are no-ops.
type SpeakerVerificationAdvance struct {
	Resolved   bool
	Resolution Resolution
}

// SpeakerVerificationCoordinator owns bounded, single-use challenges. It
// starts no goroutines and is independent of identification evidence windows.
type SpeakerVerificationCoordinator struct {
	mu         sync.Mutex
	config     SpeakerVerificationCoordinatorConfig
	challenges map[string]*speakerVerificationChallengeState
}

type speakerVerificationChallengeState struct {
	challenge          SpeakerVerificationChallenge
	expectedProfileRef string
	fragment           *SpeakerVerificationFragment
}

// NewSpeakerVerificationCoordinator validates explicit bounds and creates an
// empty coordinator.
func NewSpeakerVerificationCoordinator(config SpeakerVerificationCoordinatorConfig) (*SpeakerVerificationCoordinator, error) {
	if config.ChallengeDuration <= 0 {
		return nil, speakerVerificationInvalid(configureSpeakerVerificationOp, "challenge duration must be positive")
	}
	if config.MaxOpenChallenges <= 0 || config.MaxOpenChallenges > maxSpeakerVerificationChallenges {
		return nil, speakerVerificationInvalid(
			configureSpeakerVerificationOp,
			"maximum open challenges must be within [1,%d]",
			maxSpeakerVerificationChallenges,
		)
	}
	return &SpeakerVerificationCoordinator{
		config: config, challenges: make(map[string]*speakerVerificationChallengeState, config.MaxOpenChallenges),
	}, nil
}

// IssueChallenge binds two opaque transport tokens to an application-owned
// expected profile without disclosing that profile in the returned handle.
func (c *SpeakerVerificationCoordinator) IssueChallenge(
	challengeID string,
	evidenceWindowID string,
	expectedProfileRef string,
	openedAt time.Time,
) (SpeakerVerificationChallenge, error) {
	if !validSpeakerVerificationID(challengeID) || !validSpeakerVerificationID(evidenceWindowID) {
		return SpeakerVerificationChallenge{}, speakerVerificationInvalid(
			issueSpeakerVerificationOp,
			"challenge and evidence window identifiers are required and limited to %d bytes",
			maxSpeakerVerificationIDBytes,
		)
	}
	if !validSpeakerVerificationID(expectedProfileRef) {
		return SpeakerVerificationChallenge{}, speakerVerificationInvalid(
			issueSpeakerVerificationOp,
			"expected profile reference is required and limited to %d bytes",
			maxSpeakerVerificationIDBytes,
		)
	}
	if openedAt.IsZero() {
		return SpeakerVerificationChallenge{}, speakerVerificationInvalid(issueSpeakerVerificationOp, "opening time is required")
	}
	deadline := openedAt.Add(c.config.ChallengeDuration)
	if !deadline.After(openedAt) {
		return SpeakerVerificationChallenge{}, speakerVerificationInvalid(issueSpeakerVerificationOp, "challenge deadline overflows opening time")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.challenges[challengeID]; exists {
		return SpeakerVerificationChallenge{}, speakerVerificationInvalid(issueSpeakerVerificationOp, "challenge id %q is already open", challengeID)
	}
	if len(c.challenges) >= c.config.MaxOpenChallenges {
		return SpeakerVerificationChallenge{}, fault.New(
			fault.Unavailable,
			issueSpeakerVerificationOp,
			fmt.Errorf("open challenge capacity %d is exhausted", c.config.MaxOpenChallenges),
		)
	}
	challenge := SpeakerVerificationChallenge{
		ID: challengeID, EvidenceWindowID: evidenceWindowID, OpenedAt: openedAt, Deadline: deadline,
	}
	c.challenges[challengeID] = &speakerVerificationChallengeState{
		challenge: challenge, expectedProfileRef: expectedProfileRef,
	}
	return challenge, nil
}

// SubmitSpeakerVerificationAt accepts at most one structurally valid response
// in the challenge's half-open window. It never reads permission, enrollment,
// expected-profile, or threshold state and therefore cannot leak those facts.
func (c *SpeakerVerificationCoordinator) SubmitSpeakerVerificationAt(
	fragment SpeakerVerificationFragment,
	arrivedAt time.Time,
) (FragmentReceipt, error) {
	if err := validateSpeakerVerificationFragment(fragment); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitSpeakerVerificationOp, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, exists := c.challenges[fragment.ChallengeID]
	if !exists || state.challenge.EvidenceWindowID != fragment.WindowToken {
		return FragmentReceipt{}, fault.New(
			fault.StaleInput,
			submitSpeakerVerificationOp,
			fmt.Errorf("speaker verification challenge %q is not open for evidence window %q", fragment.ChallengeID, fragment.WindowToken),
		)
	}
	if err := validateSpeakerVerificationTiming(state.challenge, fragment.OccurredAt, arrivedAt); err != nil {
		return FragmentReceipt{}, err
	}
	if state.fragment != nil && state.fragment.FragmentID == fragment.FragmentID {
		if equalSpeakerVerificationFragments(*state.fragment, fragment) {
			return FragmentReceipt{FragmentID: fragment.FragmentID, Status: FragmentDuplicate}, nil
		}
		return FragmentReceipt{}, speakerVerificationInvalid(
			submitSpeakerVerificationOp,
			"fragment id %q collides with different content",
			fragment.FragmentID,
		)
	}
	if state.fragment != nil {
		return FragmentReceipt{}, fault.New(
			fault.PolicyBlocked,
			submitSpeakerVerificationOp,
			fmt.Errorf("challenge %q already has verification evidence", fragment.ChallengeID),
		)
	}
	cloned := fragment
	state.fragment = &cloned
	return FragmentReceipt{FragmentID: fragment.FragmentID, Status: FragmentAccepted}, nil
}

// AdvanceAt consumes an exact due challenge once. Only after consumption does
// it bind the private expected profile and evaluate current authorization.
func (c *SpeakerVerificationCoordinator) AdvanceAt(
	wakeup SpeakerVerificationWakeup,
	policy Policy,
	permissions privacy.Snapshot,
	enrollments biometric.Snapshot,
	now time.Time,
) (SpeakerVerificationAdvance, error) {
	if !validSpeakerVerificationID(wakeup.ChallengeID) ||
		!validSpeakerVerificationID(wakeup.EvidenceWindowID) ||
		wakeup.Deadline.IsZero() || now.IsZero() {
		return SpeakerVerificationAdvance{}, speakerVerificationInvalid(
			advanceSpeakerVerificationOp,
			"challenge id, evidence window id, deadline, and evaluation time are required",
		)
	}

	c.mu.Lock()
	state, exists := c.challenges[wakeup.ChallengeID]
	if !exists ||
		state.challenge.EvidenceWindowID != wakeup.EvidenceWindowID ||
		!state.challenge.Deadline.Equal(wakeup.Deadline) ||
		now.Before(state.challenge.Deadline) {
		c.mu.Unlock()
		return SpeakerVerificationAdvance{}, nil
	}
	delete(c.challenges, wakeup.ChallengeID)
	expectedProfileRef := state.expectedProfileRef
	var fragment *SpeakerVerificationFragment
	if state.fragment != nil {
		cloned := *state.fragment
		fragment = &cloned
	}
	c.mu.Unlock()

	var evidence Evidence
	if fragment != nil {
		evidence.SpeakerVerification = &SpeakerVerificationEvidence{
			OccurredAt:         fragment.OccurredAt,
			ExpectedProfileRef: expectedProfileRef,
			Candidates: []SpeakerVerificationCandidate{{
				ID: fragment.Candidate.ID, ProfileRef: expectedProfileRef,
				Score: fragment.Candidate.Score, ModelVersion: fragment.Candidate.ModelVersion,
			}},
		}
	}
	resolution, err := ResolveAuthorizedAt(
		policy,
		evidence,
		clonePrivacySnapshot(permissions),
		cloneEnrollmentSnapshot(enrollments),
		now,
	)
	if err != nil {
		return SpeakerVerificationAdvance{}, err
	}
	return SpeakerVerificationAdvance{Resolved: true, Resolution: resolution}, nil
}

func validateSpeakerVerificationFragment(fragment SpeakerVerificationFragment) error {
	if !validSpeakerVerificationID(fragment.ChallengeID) ||
		!validSpeakerVerificationID(fragment.WindowToken) ||
		!validSpeakerVerificationID(fragment.FragmentID) {
		return fmt.Errorf("challenge, evidence window, and fragment identifiers are required and limited to %d bytes", maxSpeakerVerificationIDBytes)
	}
	if !validSpeakerVerificationID(fragment.Candidate.ID) || !validSpeakerVerificationID(fragment.Candidate.ModelVersion) {
		return fmt.Errorf("candidate id and model version are required and limited to %d bytes", maxSpeakerVerificationIDBytes)
	}
	if math.IsNaN(fragment.Candidate.Score) || math.IsInf(fragment.Candidate.Score, 0) || fragment.Candidate.Score < 0 || fragment.Candidate.Score > 1 {
		return fmt.Errorf("candidate %q score is outside [0,1]", fragment.Candidate.ID)
	}
	return nil
}

func validateSpeakerVerificationTiming(
	challenge SpeakerVerificationChallenge,
	occurredAt time.Time,
	arrivedAt time.Time,
) error {
	if arrivedAt.IsZero() {
		return speakerVerificationInvalid(submitSpeakerVerificationOp, "arrival time is required")
	}
	if arrivedAt.Before(challenge.OpenedAt) {
		return speakerVerificationInvalid(submitSpeakerVerificationOp, "arrival precedes challenge %q", challenge.ID)
	}
	if !arrivedAt.Before(challenge.Deadline) {
		return fault.New(fault.StaleInput, submitSpeakerVerificationOp, fmt.Errorf("arrival is outside challenge %q", challenge.ID))
	}
	if occurredAt.IsZero() {
		return speakerVerificationInvalid(submitSpeakerVerificationOp, "fragment occurrence time is required")
	}
	if occurredAt.Before(challenge.OpenedAt) || !occurredAt.Before(challenge.Deadline) {
		return fault.New(fault.StaleInput, submitSpeakerVerificationOp, fmt.Errorf("fragment occurrence is outside challenge %q", challenge.ID))
	}
	if occurredAt.After(arrivedAt) {
		return speakerVerificationInvalid(submitSpeakerVerificationOp, "fragment occurrence follows arrival")
	}
	return nil
}

func equalSpeakerVerificationFragments(left, right SpeakerVerificationFragment) bool {
	return left.ChallengeID == right.ChallengeID &&
		left.WindowToken == right.WindowToken &&
		left.FragmentID == right.FragmentID &&
		left.OccurredAt.Equal(right.OccurredAt) &&
		left.Candidate == right.Candidate
}

func validSpeakerVerificationID(value string) bool {
	return value != "" && len(value) <= maxSpeakerVerificationIDBytes && value == strings.TrimSpace(value)
}

func speakerVerificationInvalid(op, format string, args ...any) error {
	return fault.New(fault.InvalidInput, op, fmt.Errorf(format, args...))
}
