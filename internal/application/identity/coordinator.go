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
	maxIdentificationWindows    = 1024
	maxIdentificationIDBytes    = 256
	maxIdentificationCandidates = 16

	configureCoordinatorOp        = "configure identity evidence coordinator"
	openWindowOp                  = "open identification window"
	submitFaceDetectionOp         = "submit face detection fragment"
	submitFaceIdentificationOp    = "submit face identification fragment"
	submitFaceLivenessOp          = "submit face liveness fragment"
	submitSpeakerIdentificationOp = "submit speaker identification fragment"
	advanceCoordinatorOp          = "advance identification window"
)

// IdentificationCoordinatorConfig supplies every coordinator bound. There are
// no implicit duration or capacity defaults.
type IdentificationCoordinatorConfig struct {
	WindowDuration time.Duration
	MaxOpenWindows int
}

// IdentificationWindow is an application-issued, short-lived evidence group.
// Workers may only echo its token on strongly typed evidence fragments.
type IdentificationWindow struct {
	Token    string
	OpenedAt time.Time
	Deadline time.Time
}

// Wakeup returns the opaque deadline value that owns this exact window.
func (w IdentificationWindow) Wakeup() IdentificationWakeup {
	return IdentificationWakeup{Token: w.Token, Deadline: w.Deadline}
}

// IdentificationWakeup identifies one window close. It carries no workflow or
// biometric policy meaning for a runtime timer owner.
type IdentificationWakeup struct {
	Token    string
	Deadline time.Time
}

// FaceDetectionFragment is one provider's complete face-count contribution.
type FaceDetectionFragment struct {
	WindowToken   string
	FragmentID    string
	OccurredAt    time.Time
	FacesObserved uint32
}

// FaceIdentificationFragment is one provider's complete open-set face match
// contribution. An empty candidate slice is an explicit no-match result.
type FaceIdentificationFragment struct {
	WindowToken string
	FragmentID  string
	OccurredAt  time.Time
	Candidates  []FaceIdentificationCandidate
}

// FaceLivenessFragment is one independent liveness contribution.
type FaceLivenessFragment struct {
	WindowToken string
	FragmentID  string
	OccurredAt  time.Time
	State       Liveness
}

// SpeakerIdentificationFragment is one provider's complete open-set speaker
// match contribution. An empty candidate slice is an explicit no-match result.
type SpeakerIdentificationFragment struct {
	WindowToken string
	FragmentID  string
	OccurredAt  time.Time
	Candidates  []SpeakerIdentificationCandidate
}

// FragmentReceiptStatus is the non-sensitive ingestion result for a fragment.
type FragmentReceiptStatus string

const (
	FragmentAccepted  FragmentReceiptStatus = "ACCEPTED"
	FragmentDuplicate FragmentReceiptStatus = "DUPLICATE"
)

// FragmentReceipt confirms only ingestion and deduplication. Enrollment,
// permission, model, and resolution details are deliberately absent.
type FragmentReceipt struct {
	FragmentID string
	Status     FragmentReceiptStatus
}

// IdentificationAdvance reports whether this call consumed and resolved the
// exact supplied window. Stale, mismatched, and early wakeups are no-ops.
type IdentificationAdvance struct {
	Resolved   bool
	Resolution Resolution
}

// EvidenceCoordinator owns bounded identification windows. It starts no
// goroutines and is not a general scheduler or workflow engine.
type EvidenceCoordinator struct {
	mu      sync.Mutex
	config  IdentificationCoordinatorConfig
	windows map[string]*identificationWindowState
}

type identificationWindowState struct {
	window      IdentificationWindow
	detection   *FaceDetectionFragment
	faceID      *FaceIdentificationFragment
	liveness    *FaceLivenessFragment
	speaker     *SpeakerIdentificationFragment
	fragmentIDs map[string]struct{}
}

// NewEvidenceCoordinator validates explicit bounds and creates an empty
// application-owned identification coordinator.
func NewEvidenceCoordinator(config IdentificationCoordinatorConfig) (*EvidenceCoordinator, error) {
	if config.WindowDuration <= 0 {
		return nil, coordinatorInvalid(configureCoordinatorOp, "window duration must be positive")
	}
	if config.MaxOpenWindows <= 0 || config.MaxOpenWindows > maxIdentificationWindows {
		return nil, coordinatorInvalid(configureCoordinatorOp, "maximum open windows must be within [1,%d]", maxIdentificationWindows)
	}
	return &EvidenceCoordinator{
		config:  config,
		windows: make(map[string]*identificationWindowState, config.MaxOpenWindows),
	}, nil
}

// OpenIdentificationWindow registers a token issued by trusted application
// composition. Callers must supply a unique token; providers cannot open windows.
func (c *EvidenceCoordinator) OpenIdentificationWindow(token string, openedAt time.Time) (IdentificationWindow, error) {
	if !validCoordinatorID(token) {
		return IdentificationWindow{}, coordinatorInvalid(openWindowOp, "window token is required and must not exceed %d bytes", maxIdentificationIDBytes)
	}
	if openedAt.IsZero() {
		return IdentificationWindow{}, coordinatorInvalid(openWindowOp, "opening time is required")
	}
	deadline := openedAt.Add(c.config.WindowDuration)
	if !deadline.After(openedAt) {
		return IdentificationWindow{}, coordinatorInvalid(openWindowOp, "window deadline overflows opening time")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.windows[token]; exists {
		return IdentificationWindow{}, coordinatorInvalid(openWindowOp, "window token %q is already open", token)
	}
	if len(c.windows) >= c.config.MaxOpenWindows {
		return IdentificationWindow{}, fault.New(fault.Unavailable, openWindowOp, fmt.Errorf("open window capacity %d is exhausted", c.config.MaxOpenWindows))
	}
	window := IdentificationWindow{Token: token, OpenedAt: openedAt, Deadline: deadline}
	c.windows[token] = &identificationWindowState{
		window: window, fragmentIDs: make(map[string]struct{}),
	}
	return window, nil
}

// SubmitFaceDetectionAt accepts at most one face detection fragment inside the
// window's half-open occurrence and arrival interval.
func (c *EvidenceCoordinator) SubmitFaceDetectionAt(fragment FaceDetectionFragment, arrivedAt time.Time) (FragmentReceipt, error) {
	if err := validateFragmentIdentity(fragment.WindowToken, fragment.FragmentID); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitFaceDetectionOp, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.acceptingWindow(fragment.WindowToken, fragment.OccurredAt, arrivedAt, submitFaceDetectionOp)
	if err != nil {
		return FragmentReceipt{}, err
	}
	if state.detection != nil && state.detection.FragmentID == fragment.FragmentID {
		if equalFaceDetectionFragments(*state.detection, fragment) {
			return duplicateReceipt(fragment.FragmentID), nil
		}
		return FragmentReceipt{}, fragmentCollision(submitFaceDetectionOp, fragment.FragmentID)
	}
	if state.hasFragmentID(fragment.FragmentID) {
		return FragmentReceipt{}, fragmentCollision(submitFaceDetectionOp, fragment.FragmentID)
	}
	if state.detection != nil {
		return FragmentReceipt{}, fragmentAlreadyPresent(submitFaceDetectionOp, fragment.WindowToken, "face detection")
	}
	cloned := fragment
	state.detection = &cloned
	state.addFragmentID(fragment.FragmentID)
	return acceptedReceipt(fragment.FragmentID), nil
}

// SubmitFaceIdentificationAt accepts at most one face identification fragment.
func (c *EvidenceCoordinator) SubmitFaceIdentificationAt(fragment FaceIdentificationFragment, arrivedAt time.Time) (FragmentReceipt, error) {
	if err := validateFragmentIdentity(fragment.WindowToken, fragment.FragmentID); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitFaceIdentificationOp, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.acceptingWindow(fragment.WindowToken, fragment.OccurredAt, arrivedAt, submitFaceIdentificationOp)
	if err != nil {
		return FragmentReceipt{}, err
	}
	if state.faceID != nil && state.faceID.FragmentID == fragment.FragmentID {
		if equalFaceIdentificationFragments(*state.faceID, fragment) {
			return duplicateReceipt(fragment.FragmentID), nil
		}
		return FragmentReceipt{}, fragmentCollision(submitFaceIdentificationOp, fragment.FragmentID)
	}
	if state.hasFragmentID(fragment.FragmentID) {
		return FragmentReceipt{}, fragmentCollision(submitFaceIdentificationOp, fragment.FragmentID)
	}
	if state.faceID != nil {
		return FragmentReceipt{}, fragmentAlreadyPresent(submitFaceIdentificationOp, fragment.WindowToken, "face identification")
	}
	if err := validateFaceCandidates(fragment.Candidates); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitFaceIdentificationOp, err)
	}
	cloned := cloneFaceIdentificationFragment(fragment)
	state.faceID = &cloned
	state.addFragmentID(fragment.FragmentID)
	return acceptedReceipt(fragment.FragmentID), nil
}

// SubmitFaceLivenessAt accepts at most one independent liveness fragment.
func (c *EvidenceCoordinator) SubmitFaceLivenessAt(fragment FaceLivenessFragment, arrivedAt time.Time) (FragmentReceipt, error) {
	if err := validateFragmentIdentity(fragment.WindowToken, fragment.FragmentID); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitFaceLivenessOp, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.acceptingWindow(fragment.WindowToken, fragment.OccurredAt, arrivedAt, submitFaceLivenessOp)
	if err != nil {
		return FragmentReceipt{}, err
	}
	if state.liveness != nil && state.liveness.FragmentID == fragment.FragmentID {
		if equalFaceLivenessFragments(*state.liveness, fragment) {
			return duplicateReceipt(fragment.FragmentID), nil
		}
		return FragmentReceipt{}, fragmentCollision(submitFaceLivenessOp, fragment.FragmentID)
	}
	if state.hasFragmentID(fragment.FragmentID) {
		return FragmentReceipt{}, fragmentCollision(submitFaceLivenessOp, fragment.FragmentID)
	}
	if state.liveness != nil {
		return FragmentReceipt{}, fragmentAlreadyPresent(submitFaceLivenessOp, fragment.WindowToken, "face liveness")
	}
	if fragment.State != LivenessUnknown && fragment.State != LivenessPassed && fragment.State != LivenessFailed {
		return FragmentReceipt{}, coordinatorInvalid(submitFaceLivenessOp, "face liveness state is unknown")
	}
	cloned := fragment
	state.liveness = &cloned
	state.addFragmentID(fragment.FragmentID)
	return acceptedReceipt(fragment.FragmentID), nil
}

// SubmitSpeakerIdentificationAt accepts at most one speaker identification fragment.
func (c *EvidenceCoordinator) SubmitSpeakerIdentificationAt(fragment SpeakerIdentificationFragment, arrivedAt time.Time) (FragmentReceipt, error) {
	if err := validateFragmentIdentity(fragment.WindowToken, fragment.FragmentID); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitSpeakerIdentificationOp, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.acceptingWindow(fragment.WindowToken, fragment.OccurredAt, arrivedAt, submitSpeakerIdentificationOp)
	if err != nil {
		return FragmentReceipt{}, err
	}
	if state.speaker != nil && state.speaker.FragmentID == fragment.FragmentID {
		if equalSpeakerIdentificationFragments(*state.speaker, fragment) {
			return duplicateReceipt(fragment.FragmentID), nil
		}
		return FragmentReceipt{}, fragmentCollision(submitSpeakerIdentificationOp, fragment.FragmentID)
	}
	if state.hasFragmentID(fragment.FragmentID) {
		return FragmentReceipt{}, fragmentCollision(submitSpeakerIdentificationOp, fragment.FragmentID)
	}
	if state.speaker != nil {
		return FragmentReceipt{}, fragmentAlreadyPresent(submitSpeakerIdentificationOp, fragment.WindowToken, "speaker identification")
	}
	if err := validateSpeakerCandidates(fragment.Candidates); err != nil {
		return FragmentReceipt{}, fault.New(fault.InvalidInput, submitSpeakerIdentificationOp, err)
	}
	cloned := cloneSpeakerIdentificationFragment(fragment)
	state.speaker = &cloned
	state.addFragmentID(fragment.FragmentID)
	return acceptedReceipt(fragment.FragmentID), nil
}

// AdvanceAt consumes an exact due window once. Authorization snapshots are
// evaluated only after the window closes, so receipts reveal no identity state.
func (c *EvidenceCoordinator) AdvanceAt(
	wakeup IdentificationWakeup,
	policy Policy,
	permissions privacy.Snapshot,
	enrollments biometric.Snapshot,
	now time.Time,
) (IdentificationAdvance, error) {
	if !validCoordinatorID(wakeup.Token) || wakeup.Deadline.IsZero() || now.IsZero() {
		return IdentificationAdvance{}, coordinatorInvalid(advanceCoordinatorOp, "wakeup token, deadline, and evaluation time are required")
	}
	c.mu.Lock()
	state, exists := c.windows[wakeup.Token]
	if !exists || !state.window.Deadline.Equal(wakeup.Deadline) || now.Before(state.window.Deadline) {
		c.mu.Unlock()
		return IdentificationAdvance{}, nil
	}
	delete(c.windows, wakeup.Token)
	evidence := cloneWindowEvidence(state)
	c.mu.Unlock()

	resolution, err := ResolveAuthorizedAt(policy, evidence, clonePrivacySnapshot(permissions), cloneEnrollmentSnapshot(enrollments), now)
	if err != nil {
		return IdentificationAdvance{}, err
	}
	return IdentificationAdvance{Resolved: true, Resolution: resolution}, nil
}

func (c *EvidenceCoordinator) acceptingWindow(token string, occurredAt, arrivedAt time.Time, op string) (*identificationWindowState, error) {
	state, exists := c.windows[token]
	if !exists {
		return nil, fault.New(fault.StaleInput, op, fmt.Errorf("identification window %q is not open", token))
	}
	if arrivedAt.IsZero() {
		return nil, coordinatorInvalid(op, "arrival time is required")
	}
	if arrivedAt.Before(state.window.OpenedAt) {
		return nil, coordinatorInvalid(op, "arrival precedes window %q", token)
	}
	if !arrivedAt.Before(state.window.Deadline) {
		return nil, fault.New(fault.StaleInput, op, fmt.Errorf("arrival is outside window %q", token))
	}
	if occurredAt.IsZero() {
		return nil, coordinatorInvalid(op, "fragment occurrence time is required")
	}
	if occurredAt.Before(state.window.OpenedAt) || !occurredAt.Before(state.window.Deadline) {
		return nil, fault.New(fault.StaleInput, op, fmt.Errorf("fragment occurrence is outside window %q", token))
	}
	if occurredAt.After(arrivedAt) {
		return nil, coordinatorInvalid(op, "fragment occurrence follows arrival")
	}
	return state, nil
}

func (state *identificationWindowState) hasFragmentID(fragmentID string) bool {
	_, exists := state.fragmentIDs[fragmentID]
	return exists
}

func (state *identificationWindowState) addFragmentID(fragmentID string) {
	state.fragmentIDs[fragmentID] = struct{}{}
}

func validateFragmentIdentity(windowToken, fragmentID string) error {
	if !validCoordinatorID(windowToken) {
		return fmt.Errorf("window token is required and must not exceed %d bytes", maxIdentificationIDBytes)
	}
	if !validCoordinatorID(fragmentID) {
		return fmt.Errorf("fragment id is required and must not exceed %d bytes", maxIdentificationIDBytes)
	}
	return nil
}

func validateFaceCandidates(candidates []FaceIdentificationCandidate) error {
	if len(candidates) > maxIdentificationCandidates {
		return fmt.Errorf("face fragment candidate count must not exceed %d", maxIdentificationCandidates)
	}
	seenIDs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := validateCoordinatorCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion); err != nil {
			return err
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return fmt.Errorf("candidate id %q is duplicated", candidate.ID)
		}
		seenIDs[candidate.ID] = struct{}{}
	}
	return nil
}

func validateSpeakerCandidates(candidates []SpeakerIdentificationCandidate) error {
	if len(candidates) > maxIdentificationCandidates {
		return fmt.Errorf("speaker fragment candidate count must not exceed %d", maxIdentificationCandidates)
	}
	seenIDs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := validateCoordinatorCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion); err != nil {
			return err
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return fmt.Errorf("candidate id %q is duplicated", candidate.ID)
		}
		seenIDs[candidate.ID] = struct{}{}
	}
	return nil
}

func validateCoordinatorCandidate(id, profileRef string, score float64, modelVersion string) error {
	if !validCoordinatorID(id) || !validCoordinatorID(profileRef) || !validCoordinatorID(modelVersion) {
		return fmt.Errorf("candidate id, profile reference, and model version are required and limited to %d bytes", maxIdentificationIDBytes)
	}
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
		return fmt.Errorf("candidate %q score is outside [0,1]", id)
	}
	return nil
}

func validCoordinatorID(value string) bool {
	return value != "" && len(value) <= maxIdentificationIDBytes && value == strings.TrimSpace(value)
}

func cloneFaceIdentificationFragment(fragment FaceIdentificationFragment) FaceIdentificationFragment {
	fragment.Candidates = append([]FaceIdentificationCandidate(nil), fragment.Candidates...)
	return fragment
}

func cloneSpeakerIdentificationFragment(fragment SpeakerIdentificationFragment) SpeakerIdentificationFragment {
	fragment.Candidates = append([]SpeakerIdentificationCandidate(nil), fragment.Candidates...)
	return fragment
}

func cloneWindowEvidence(state *identificationWindowState) Evidence {
	var evidence Evidence
	if state.detection != nil {
		evidence.FaceDetection = &FaceDetectionEvidence{OccurredAt: state.detection.OccurredAt, FacesObserved: state.detection.FacesObserved}
	}
	if state.faceID != nil {
		evidence.FaceIdentification = &FaceIdentificationEvidence{OccurredAt: state.faceID.OccurredAt, Candidates: append([]FaceIdentificationCandidate(nil), state.faceID.Candidates...)}
	}
	if state.liveness != nil {
		evidence.FaceLiveness = &FaceLivenessEvidence{OccurredAt: state.liveness.OccurredAt, State: state.liveness.State}
	}
	if state.speaker != nil {
		evidence.SpeakerIdentification = &SpeakerIdentificationEvidence{OccurredAt: state.speaker.OccurredAt, Candidates: append([]SpeakerIdentificationCandidate(nil), state.speaker.Candidates...)}
	}
	return evidence
}

func clonePrivacySnapshot(snapshot privacy.Snapshot) privacy.Snapshot {
	snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	return snapshot
}

func cloneEnrollmentSnapshot(snapshot biometric.Snapshot) biometric.Snapshot {
	snapshot.Records = append([]biometric.Record(nil), snapshot.Records...)
	for index := range snapshot.Records {
		if snapshot.Records[index].PendingDelete != nil {
			pending := *snapshot.Records[index].PendingDelete
			snapshot.Records[index].PendingDelete = &pending
		}
	}
	return snapshot
}

func equalFaceDetectionFragments(left, right FaceDetectionFragment) bool {
	return equalFragmentMetadata(left.WindowToken, left.FragmentID, left.OccurredAt, right.WindowToken, right.FragmentID, right.OccurredAt) && left.FacesObserved == right.FacesObserved
}

func equalFaceIdentificationFragments(left, right FaceIdentificationFragment) bool {
	if !equalFragmentMetadata(left.WindowToken, left.FragmentID, left.OccurredAt, right.WindowToken, right.FragmentID, right.OccurredAt) || len(left.Candidates) != len(right.Candidates) {
		return false
	}
	for index := range left.Candidates {
		if !equalFaceCandidate(left.Candidates[index], right.Candidates[index]) {
			return false
		}
	}
	return true
}

func equalFaceLivenessFragments(left, right FaceLivenessFragment) bool {
	return equalFragmentMetadata(left.WindowToken, left.FragmentID, left.OccurredAt, right.WindowToken, right.FragmentID, right.OccurredAt) && left.State == right.State
}

func equalSpeakerIdentificationFragments(left, right SpeakerIdentificationFragment) bool {
	if !equalFragmentMetadata(left.WindowToken, left.FragmentID, left.OccurredAt, right.WindowToken, right.FragmentID, right.OccurredAt) || len(left.Candidates) != len(right.Candidates) {
		return false
	}
	for index := range left.Candidates {
		if !equalSpeakerCandidate(left.Candidates[index], right.Candidates[index]) {
			return false
		}
	}
	return true
}

func equalFragmentMetadata(leftWindow, leftID string, leftAt time.Time, rightWindow, rightID string, rightAt time.Time) bool {
	return leftWindow == rightWindow && leftID == rightID && leftAt.Equal(rightAt)
}

func equalFaceCandidate(left, right FaceIdentificationCandidate) bool {
	return left.ID == right.ID && left.ProfileRef == right.ProfileRef && left.Score == right.Score && left.ModelVersion == right.ModelVersion
}

func equalSpeakerCandidate(left, right SpeakerIdentificationCandidate) bool {
	return left.ID == right.ID && left.ProfileRef == right.ProfileRef && left.Score == right.Score && left.ModelVersion == right.ModelVersion
}

func acceptedReceipt(fragmentID string) FragmentReceipt {
	return FragmentReceipt{FragmentID: fragmentID, Status: FragmentAccepted}
}

func duplicateReceipt(fragmentID string) FragmentReceipt {
	return FragmentReceipt{FragmentID: fragmentID, Status: FragmentDuplicate}
}

func fragmentCollision(op, fragmentID string) error {
	return coordinatorInvalid(op, "fragment id %q collides with different content", fragmentID)
}

func fragmentAlreadyPresent(op, windowToken, kind string) error {
	return fault.New(fault.PolicyBlocked, op, fmt.Errorf("window %q already has %s evidence", windowToken, kind))
}

func coordinatorInvalid(op, format string, args ...any) error {
	return fault.New(fault.InvalidInput, op, fmt.Errorf(format, args...))
}
