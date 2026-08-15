package identity

import (
	"strings"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestEvidenceCoordinatorResolvesMergedIdentificationOnlyAtDeadline(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, 2*time.Second, 2)
	window, err := coordinator.OpenIdentificationWindow("identity-window-1", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	if window.OpenedAt != openedAt || window.Deadline != openedAt.Add(2*time.Second) {
		t.Fatalf("window = %#v", window)
	}
	if got := window.Wakeup(); got.Token != window.Token || got.Deadline != window.Deadline {
		t.Fatalf("Wakeup() = %#v", got)
	}

	detection := FaceDetectionFragment{
		WindowToken: window.Token, FragmentID: "detection-fragment-1", OccurredAt: openedAt, FacesObserved: 1,
	}
	face := FaceIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  "face-fragment-1",
		OccurredAt:  openedAt,
		Candidates: []FaceIdentificationCandidate{
			faceCandidate("face-candidate-1", "profile-a", 1),
		},
	}
	liveness := FaceLivenessFragment{
		WindowToken: window.Token, FragmentID: "liveness-fragment-1", OccurredAt: openedAt, State: LivenessPassed,
	}
	speaker := SpeakerIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  "speaker-fragment-1",
		OccurredAt:  openedAt.Add(time.Second),
		Candidates: []SpeakerIdentificationCandidate{
			speakerCandidate("speaker-candidate-1", "profile-a", 1),
		},
	}
	if receipt, submitErr := coordinator.SubmitFaceDetectionAt(detection, openedAt); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceDetectionAt() = %#v, %v", receipt, submitErr)
	}
	if receipt, submitErr := coordinator.SubmitFaceIdentificationAt(face, openedAt); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceIdentificationAt() = %#v, %v", receipt, submitErr)
	}
	if receipt, submitErr := coordinator.SubmitFaceLivenessAt(liveness, openedAt); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceLivenessAt() = %#v, %v", receipt, submitErr)
	}
	if receipt, submitErr := coordinator.SubmitSpeakerIdentificationAt(speaker, openedAt.Add(time.Second)); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitSpeakerIdentificationAt() = %#v, %v", receipt, submitErr)
	}

	policy := testPolicy()
	permissions, enrollments := authorizedIdentificationSnapshots(openedAt)
	for _, advance := range []struct {
		name   string
		wakeup IdentificationWakeup
		now    time.Time
	}{
		{name: "wrong token", wakeup: IdentificationWakeup{Token: "identity-window-other", Deadline: window.Deadline}, now: window.Deadline},
		{name: "wrong deadline", wakeup: IdentificationWakeup{Token: window.Token, Deadline: window.Deadline.Add(time.Nanosecond)}, now: window.Deadline},
		{name: "before deadline", wakeup: window.Wakeup(), now: window.Deadline.Add(-time.Nanosecond)},
	} {
		t.Run(advance.name, func(t *testing.T) {
			result, advanceErr := coordinator.AdvanceAt(advance.wakeup, policy, permissions, enrollments, advance.now)
			if advanceErr != nil || result.Resolved {
				t.Fatalf("AdvanceAt() = %#v, %v, want no resolution", result, advanceErr)
			}
		})
	}

	result, err := coordinator.AdvanceAt(window.Wakeup(), policy, permissions, enrollments, window.Deadline)
	if err != nil {
		t.Fatalf("AdvanceAt(deadline) error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != Recognized || result.Resolution.ProfileRef != "profile-a" || result.Resolution.Reason != ReasonModalitiesMatched {
		t.Fatalf("AdvanceAt(deadline) = %#v", result)
	}
	duplicate, err := coordinator.AdvanceAt(window.Wakeup(), policy, permissions, enrollments, window.Deadline)
	if err != nil || duplicate.Resolved {
		t.Fatalf("AdvanceAt(duplicate) = %#v, %v, want exactly-once no-op", duplicate, err)
	}
}

func TestEvidenceCoordinatorPreservesExplicitEmptyFaceMatchForMultiplePeopleGuard(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-multiple-people", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	if _, err := coordinator.SubmitFaceDetectionAt(FaceDetectionFragment{
		WindowToken: window.Token, FragmentID: "detection-multiple", OccurredAt: openedAt, FacesObserved: 2,
	}, openedAt); err != nil {
		t.Fatalf("SubmitFaceDetectionAt() error = %v", err)
	}
	if _, err := coordinator.SubmitFaceIdentificationAt(FaceIdentificationFragment{
		WindowToken: window.Token, FragmentID: "face-empty", OccurredAt: openedAt,
	}, openedAt); err != nil {
		t.Fatalf("SubmitFaceIdentificationAt(empty) error = %v", err)
	}
	if _, err := coordinator.SubmitSpeakerIdentificationAt(SpeakerIdentificationFragment{
		WindowToken: window.Token, FragmentID: "speaker-strong", OccurredAt: openedAt,
		Candidates: []SpeakerIdentificationCandidate{speakerCandidate("speaker-candidate", "profile-a", 1)},
	}, openedAt); err != nil {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v", err)
	}

	permissions := permissionSnapshot(openedAt,
		privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification,
		privacy.MicrophoneCapture, privacy.SpeakerIdentification,
	)
	enrollments := biometric.Snapshot{Revision: 1, Records: []biometric.Record{
		activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", openedAt),
	}}
	result, err := coordinator.AdvanceAt(window.Wakeup(), testPolicy(), permissions, enrollments, window.Deadline)
	if err != nil {
		t.Fatalf("AdvanceAt() error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != Anonymous || result.Resolution.Reason != ReasonMultiplePeople {
		t.Fatalf("AdvanceAt() = %#v, want ANONYMOUS/MULTIPLE_PEOPLE", result)
	}
}

func TestEvidenceCoordinatorDeduplicatesIndependentDetectionAndLivenessFragments(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-independent-dedup", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	detection := FaceDetectionFragment{
		WindowToken: window.Token, FragmentID: "detection-fragment", OccurredAt: openedAt, FacesObserved: 1,
	}
	if receipt, err := coordinator.SubmitFaceDetectionAt(detection, openedAt); err != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceDetectionAt(first) = %#v, %v", receipt, err)
	}
	if receipt, err := coordinator.SubmitFaceDetectionAt(detection, openedAt); err != nil || receipt.Status != FragmentDuplicate {
		t.Fatalf("SubmitFaceDetectionAt(duplicate) = %#v, %v", receipt, err)
	}
	changedDetection := detection
	changedDetection.FacesObserved = 2
	if _, err := coordinator.SubmitFaceDetectionAt(changedDetection, openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("SubmitFaceDetectionAt(collision) error = %v, want InvalidInput", err)
	}

	liveness := FaceLivenessFragment{
		WindowToken: window.Token, FragmentID: "liveness-fragment", OccurredAt: openedAt, State: LivenessPassed,
	}
	if receipt, err := coordinator.SubmitFaceLivenessAt(liveness, openedAt); err != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceLivenessAt(first) = %#v, %v", receipt, err)
	}
	if receipt, err := coordinator.SubmitFaceLivenessAt(liveness, openedAt); err != nil || receipt.Status != FragmentDuplicate {
		t.Fatalf("SubmitFaceLivenessAt(duplicate) = %#v, %v", receipt, err)
	}
	changedLiveness := liveness
	changedLiveness.State = LivenessFailed
	if _, err := coordinator.SubmitFaceLivenessAt(changedLiveness, openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("SubmitFaceLivenessAt(collision) error = %v, want InvalidInput", err)
	}
}

func TestEvidenceCoordinatorUsesHalfOpenIdentificationWindow(t *testing.T) {
	openedAt := testNow()
	tests := []struct {
		name        string
		windowToken string
		occurredAt  time.Time
		arrivedAt   time.Time
		wantCode    fault.Code
	}{
		{name: "opening instant accepted", occurredAt: openedAt, arrivedAt: openedAt},
		{name: "last instant accepted", occurredAt: openedAt.Add(time.Second - time.Nanosecond), arrivedAt: openedAt.Add(time.Second - time.Nanosecond)},
		{name: "wrong token stale", windowToken: "identity-window-other", occurredAt: openedAt, arrivedAt: openedAt, wantCode: fault.StaleInput},
		{name: "event before opening stale", occurredAt: openedAt.Add(-time.Nanosecond), arrivedAt: openedAt, wantCode: fault.StaleInput},
		{name: "arrival before opening invalid", occurredAt: openedAt, arrivedAt: openedAt.Add(-time.Nanosecond), wantCode: fault.InvalidInput},
		{name: "event after arrival invalid", occurredAt: openedAt.Add(time.Nanosecond), arrivedAt: openedAt, wantCode: fault.InvalidInput},
		{name: "event at deadline stale", occurredAt: openedAt.Add(time.Second), arrivedAt: openedAt, wantCode: fault.StaleInput},
		{name: "arrival at deadline stale", occurredAt: openedAt, arrivedAt: openedAt.Add(time.Second), wantCode: fault.StaleInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
			window, err := coordinator.OpenIdentificationWindow("identity-window-boundary", openedAt)
			if err != nil {
				t.Fatalf("OpenIdentificationWindow() error = %v", err)
			}
			windowToken := test.windowToken
			if windowToken == "" {
				windowToken = window.Token
			}
			fragment := SpeakerIdentificationFragment{
				WindowToken: windowToken,
				FragmentID:  "speaker-fragment-boundary",
				OccurredAt:  test.occurredAt,
				Candidates: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-candidate-boundary", "profile-a", 1),
				},
			}
			receipt, submitErr := coordinator.SubmitSpeakerIdentificationAt(fragment, test.arrivedAt)
			if test.wantCode == "" {
				if submitErr != nil || receipt.Status != FragmentAccepted {
					t.Fatalf("SubmitSpeakerIdentificationAt() = %#v, %v", receipt, submitErr)
				}
				return
			}
			if !fault.IsCode(submitErr, test.wantCode) {
				t.Fatalf("SubmitSpeakerIdentificationAt() error = %v, want %s", submitErr, test.wantCode)
			}
			permissions, enrollments := authorizedIdentificationSnapshots(openedAt)
			result, advanceErr := coordinator.AdvanceAt(window.Wakeup(), testPolicy(), permissions, enrollments, window.Deadline)
			if advanceErr != nil || !result.Resolved || result.Resolution.Reason != ReasonNoMatch {
				t.Fatalf("rejected fragment polluted window: AdvanceAt() = %#v, %v", result, advanceErr)
			}
		})
	}
}

func TestEvidenceCoordinatorDeduplicatesFragmentsAndRejectsCollisions(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-dedup", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	face := FaceIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  "fragment-1",
		OccurredAt:  openedAt,
		Candidates: []FaceIdentificationCandidate{
			faceCandidate("candidate-1", "profile-a", 1),
		},
	}
	if receipt, submitErr := coordinator.SubmitFaceIdentificationAt(face, openedAt); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitFaceIdentificationAt(first) = %#v, %v", receipt, submitErr)
	}
	if receipt, submitErr := coordinator.SubmitFaceIdentificationAt(face, openedAt); submitErr != nil || receipt.Status != FragmentDuplicate {
		t.Fatalf("SubmitFaceIdentificationAt(duplicate) = %#v, %v", receipt, submitErr)
	}

	collision := face
	collision.Candidates = append([]FaceIdentificationCandidate(nil), face.Candidates...)
	collision.Candidates[0].Score = 0.9
	if _, submitErr := coordinator.SubmitFaceIdentificationAt(collision, openedAt); !fault.IsCode(submitErr, fault.InvalidInput) {
		t.Fatalf("SubmitFaceIdentificationAt(collision) error = %v, want InvalidInput", submitErr)
	}
	secondFace := face
	secondFace.FragmentID = "fragment-2"
	secondFace.Candidates = append([]FaceIdentificationCandidate(nil), face.Candidates...)
	secondFace.Candidates[0].ID = "candidate-2"
	if _, submitErr := coordinator.SubmitFaceIdentificationAt(secondFace, openedAt); !fault.IsCode(submitErr, fault.PolicyBlocked) {
		t.Fatalf("SubmitFaceIdentificationAt(second fragment) error = %v, want PolicyBlocked", submitErr)
	}
	crossModalityCollision := SpeakerIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  face.FragmentID,
		OccurredAt:  openedAt,
		Candidates: []SpeakerIdentificationCandidate{
			speakerCandidate("candidate-3", "profile-a", 1),
		},
	}
	if _, submitErr := coordinator.SubmitSpeakerIdentificationAt(crossModalityCollision, openedAt); !fault.IsCode(submitErr, fault.InvalidInput) {
		t.Fatalf("SubmitSpeakerIdentificationAt(fragment collision) error = %v, want InvalidInput", submitErr)
	}
	candidateCollision := crossModalityCollision
	candidateCollision.FragmentID = "speaker-fragment-1"
	candidateCollision.Candidates[0].ID = face.Candidates[0].ID
	if receipt, submitErr := coordinator.SubmitSpeakerIdentificationAt(candidateCollision, openedAt); submitErr != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitSpeakerIdentificationAt(same provider-local candidate id) = %#v, %v", receipt, submitErr)
	}
}

func TestEvidenceCoordinatorRejectsCandidateIDDuplicatesWithinOneFragment(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-candidate-dedup", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	fragment := SpeakerIdentificationFragment{
		WindowToken: window.Token, FragmentID: "speaker-duplicates", OccurredAt: openedAt,
		Candidates: []SpeakerIdentificationCandidate{
			speakerCandidate("provider-local-1", "profile-a", 1),
			speakerCandidate("provider-local-1", "profile-b", 0.5),
		},
	}
	if _, err := coordinator.SubmitSpeakerIdentificationAt(fragment, openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v, want InvalidInput", err)
	}
}

func TestEvidenceCoordinatorClonesSubmittedCandidates(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-clone", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	fragment := SpeakerIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  "speaker-fragment-clone",
		OccurredAt:  openedAt,
		Candidates: []SpeakerIdentificationCandidate{
			speakerCandidate("speaker-candidate-clone", "profile-a", 1),
		},
	}
	if _, err := coordinator.SubmitSpeakerIdentificationAt(fragment, openedAt); err != nil {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v", err)
	}
	fragment.Candidates[0].ProfileRef = "profile-b"
	fragment.Candidates[0].ModelVersion = "tampered-model"

	permissions, enrollments := authorizedIdentificationSnapshots(openedAt)
	result, err := coordinator.AdvanceAt(window.Wakeup(), testPolicy(), permissions, enrollments, window.Deadline)
	if err != nil {
		t.Fatalf("AdvanceAt() error = %v", err)
	}
	if !result.Resolved || result.Resolution.ProfileRef != "profile-a" || result.Resolution.Reason != ReasonSpeakerIdentified {
		t.Fatalf("AdvanceAt() = %#v, input mutation reached coordinator state", result)
	}
}

func TestEvidenceCoordinatorSerializesConcurrentModalitySubmissions(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-concurrent", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}

	const submissions = 12
	start := make(chan struct{})
	type submissionResult struct {
		receipt FragmentReceipt
		err     error
	}
	results := make(chan submissionResult, submissions)
	var workers sync.WaitGroup
	workers.Add(submissions)
	for index := 0; index < submissions; index++ {
		index := index
		go func() {
			defer workers.Done()
			<-start
			fragment := SpeakerIdentificationFragment{
				WindowToken: window.Token,
				FragmentID:  "speaker-fragment-" + strings.Repeat("x", index+1),
				OccurredAt:  openedAt,
				Candidates: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-candidate-"+strings.Repeat("x", index+1), "profile-a", 1),
				},
			}
			receipt, submitErr := coordinator.SubmitSpeakerIdentificationAt(fragment, openedAt)
			results <- submissionResult{receipt: receipt, err: submitErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	accepted := 0
	blocked := 0
	for result := range results {
		switch {
		case result.err == nil && result.receipt.Status == FragmentAccepted:
			accepted++
		case fault.IsCode(result.err, fault.PolicyBlocked):
			blocked++
		default:
			t.Fatalf("SubmitSpeakerIdentificationAt() = %#v, %v", result.receipt, result.err)
		}
	}
	if accepted != 1 || blocked != submissions-1 {
		t.Fatalf("concurrent submissions accepted=%d blocked=%d, want 1/%d", accepted, blocked, submissions-1)
	}
}

func TestEvidenceCoordinatorConsumesEmptyWindowAsAnonymousNoMatch(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-empty", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	result, err := coordinator.AdvanceAt(window.Wakeup(), testPolicy(), privacy.Snapshot{}, biometric.Snapshot{}, window.Deadline)
	if err != nil {
		t.Fatalf("AdvanceAt() error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != Anonymous || result.Resolution.Reason != ReasonNoMatch || result.Resolution.PolicyVersion != testPolicy().Version {
		t.Fatalf("AdvanceAt() = %#v, want anonymous no match", result)
	}
	if duplicate, duplicateErr := coordinator.AdvanceAt(window.Wakeup(), Policy{}, privacy.Snapshot{}, biometric.Snapshot{}, window.Deadline); duplicateErr != nil || duplicate.Resolved {
		t.Fatalf("AdvanceAt(duplicate) = %#v, %v", duplicate, duplicateErr)
	}
}

func TestEvidenceCoordinatorDoesNotRetryConsumedResolutionFailure(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	window, err := coordinator.OpenIdentificationWindow("identity-window-resolution-error", openedAt)
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	fragment := SpeakerIdentificationFragment{
		WindowToken: window.Token,
		FragmentID:  "speaker-fragment-resolution-error",
		OccurredAt:  openedAt,
		Candidates: []SpeakerIdentificationCandidate{
			speakerCandidate("speaker-candidate-resolution-error", "profile-a", 1),
		},
	}
	if _, err := coordinator.SubmitSpeakerIdentificationAt(fragment, openedAt); err != nil {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v", err)
	}
	permissions, enrollments := authorizedIdentificationSnapshots(openedAt)
	if _, err := coordinator.AdvanceAt(window.Wakeup(), Policy{}, permissions, enrollments, window.Deadline); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("AdvanceAt(invalid policy) error = %v, want InvalidInput", err)
	}
	result, err := coordinator.AdvanceAt(window.Wakeup(), testPolicy(), permissions, enrollments, window.Deadline)
	if err != nil || result.Resolved {
		t.Fatalf("AdvanceAt(retry) = %#v, %v, want consumed no-op", result, err)
	}
}

func TestEvidenceCoordinatorBoundsConfigurationWindowsAndFragments(t *testing.T) {
	invalidConfigs := []IdentificationCoordinatorConfig{
		{},
		{WindowDuration: time.Second},
		{WindowDuration: time.Second, MaxOpenWindows: -1},
		{WindowDuration: time.Second, MaxOpenWindows: maxIdentificationWindows + 1},
	}
	for _, config := range invalidConfigs {
		if _, err := NewEvidenceCoordinator(config); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("NewEvidenceCoordinator(%#v) error = %v, want InvalidInput", config, err)
		}
	}

	openedAt := testNow()
	coordinator := newTestEvidenceCoordinator(t, time.Second, 1)
	if _, err := coordinator.OpenIdentificationWindow("identity-window-capacity-1", openedAt); err != nil {
		t.Fatalf("OpenIdentificationWindow(first) error = %v", err)
	}
	if _, err := coordinator.OpenIdentificationWindow("identity-window-capacity-2", openedAt); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("OpenIdentificationWindow(over capacity) error = %v, want Unavailable", err)
	}

	tooLong := strings.Repeat("x", maxIdentificationIDBytes+1)
	for _, test := range []struct {
		name     string
		fragment SpeakerIdentificationFragment
	}{
		{name: "fragment id too long", fragment: SpeakerIdentificationFragment{WindowToken: "identity-window-capacity-1", FragmentID: tooLong, OccurredAt: openedAt, Candidates: []SpeakerIdentificationCandidate{speakerCandidate("candidate", "profile-a", 1)}}},
		{name: "too many candidates", fragment: SpeakerIdentificationFragment{WindowToken: "identity-window-capacity-1", FragmentID: "speaker-fragment", OccurredAt: openedAt, Candidates: makeSpeakerCandidates(maxIdentificationCandidates+1, openedAt)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := coordinator.SubmitSpeakerIdentificationAt(test.fragment, openedAt); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("SubmitSpeakerIdentificationAt() error = %v, want InvalidInput", err)
			}
		})
	}
	if _, err := coordinator.OpenIdentificationWindow(tooLong, openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("OpenIdentificationWindow(long token) error = %v, want InvalidInput", err)
	}
}

func newTestEvidenceCoordinator(t *testing.T, duration time.Duration, capacity int) *EvidenceCoordinator {
	t.Helper()
	coordinator, err := NewEvidenceCoordinator(IdentificationCoordinatorConfig{
		WindowDuration: duration,
		MaxOpenWindows: capacity,
	})
	if err != nil {
		t.Fatalf("NewEvidenceCoordinator() error = %v", err)
	}
	return coordinator
}

func authorizedIdentificationSnapshots(at time.Time) (privacy.Snapshot, biometric.Snapshot) {
	permissions := permissionSnapshot(at,
		privacy.CameraCapture,
		privacy.MicrophoneCapture,
		privacy.FaceDetection,
		privacy.FaceIdentification,
		privacy.FaceLiveness,
		privacy.SpeakerIdentification,
	)
	enrollments := biometric.Snapshot{Revision: 1, Records: []biometric.Record{
		activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", at),
		activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", at),
	}}
	return permissions, enrollments
}

func makeSpeakerCandidates(count int, _ time.Time) []SpeakerIdentificationCandidate {
	candidates := make([]SpeakerIdentificationCandidate, 0, count)
	for index := 0; index < count; index++ {
		candidates = append(candidates, speakerCandidate(
			"candidate-"+strings.Repeat("x", index+1),
			"profile-a",
			1,
		))
	}
	return candidates
}
