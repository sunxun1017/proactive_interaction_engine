package identity

import (
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestSpeakerVerificationWorkerTypesDoNotExposeExpectedProfile(t *testing.T) {
	challengeType := reflect.TypeOf(SpeakerVerificationChallenge{})
	for _, forbidden := range []string{"ExpectedProfileRef", "ProfileRef"} {
		if _, exists := challengeType.FieldByName(forbidden); exists {
			t.Fatalf("SpeakerVerificationChallenge exposes %s", forbidden)
		}
	}
	candidateType := reflect.TypeOf(SpeakerVerificationSubmissionCandidate{})
	wantFields := []string{"ID", "Score", "ModelVersion"}
	if candidateType.NumField() != len(wantFields) {
		t.Fatalf("submission candidate fields = %d, want %d", candidateType.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		if got := candidateType.Field(index).Name; got != want {
			t.Fatalf("submission candidate field %d = %q, want %q", index, got, want)
		}
	}
}

func TestSpeakerVerificationCoordinatorBindsApplicationExpectedProfileAtDeadline(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
	challenge, err := coordinator.IssueChallenge("verification-challenge-1", "evidence-window-1", "profile-a", openedAt)
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	if challenge.ID != "verification-challenge-1" || challenge.EvidenceWindowID != "evidence-window-1" || challenge.OpenedAt != openedAt || challenge.Deadline != openedAt.Add(time.Second) {
		t.Fatalf("challenge = %#v", challenge)
	}
	if wakeup := challenge.Wakeup(); wakeup.ChallengeID != challenge.ID || wakeup.EvidenceWindowID != challenge.EvidenceWindowID || wakeup.Deadline != challenge.Deadline {
		t.Fatalf("Wakeup() = %#v", wakeup)
	}

	fragment := validSpeakerVerificationFragment(challenge.ID, challenge.EvidenceWindowID, "verification-fragment-1", openedAt, testPolicy().SpeakerVerificationThreshold)
	receipt, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt)
	if err != nil || receipt.FragmentID != fragment.FragmentID || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitSpeakerVerificationAt() = %#v, %v", receipt, err)
	}

	permissions := permissionSnapshot(openedAt, privacy.MicrophoneCapture, privacy.SpeakerVerification)
	enrollments := biometric.Snapshot{Revision: 1, Records: []biometric.Record{
		activeEnrollment("profile-a", readiness.SpeakerVerification, "speaker-verification-model.v1", openedAt),
	}}
	for _, early := range []struct {
		name   string
		wakeup SpeakerVerificationWakeup
		now    time.Time
	}{
		{name: "wrong challenge", wakeup: SpeakerVerificationWakeup{ChallengeID: "verification-other", EvidenceWindowID: challenge.EvidenceWindowID, Deadline: challenge.Deadline}, now: challenge.Deadline},
		{name: "wrong window", wakeup: SpeakerVerificationWakeup{ChallengeID: challenge.ID, EvidenceWindowID: "evidence-window-other", Deadline: challenge.Deadline}, now: challenge.Deadline},
		{name: "wrong deadline", wakeup: SpeakerVerificationWakeup{ChallengeID: challenge.ID, EvidenceWindowID: challenge.EvidenceWindowID, Deadline: challenge.Deadline.Add(time.Nanosecond)}, now: challenge.Deadline},
		{name: "before deadline", wakeup: challenge.Wakeup(), now: challenge.Deadline.Add(-time.Nanosecond)},
	} {
		t.Run(early.name, func(t *testing.T) {
			result, advanceErr := coordinator.AdvanceAt(early.wakeup, testPolicy(), permissions, enrollments, early.now)
			if advanceErr != nil || result.Resolved {
				t.Fatalf("AdvanceAt() = %#v, %v, want no-op", result, advanceErr)
			}
		})
	}

	result, err := coordinator.AdvanceAt(challenge.Wakeup(), testPolicy(), permissions, enrollments, challenge.Deadline)
	if err != nil {
		t.Fatalf("AdvanceAt(deadline) error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != Verified || result.Resolution.ProfileRef != "profile-a" || result.Resolution.Reason != ReasonSpeakerVerified {
		t.Fatalf("AdvanceAt(deadline) = %#v", result)
	}
	repeated, err := coordinator.AdvanceAt(challenge.Wakeup(), testPolicy(), permissions, enrollments, challenge.Deadline)
	if err != nil || repeated.Resolved {
		t.Fatalf("AdvanceAt(repeated) = %#v, %v, want consumed no-op", repeated, err)
	}
	if _, err := coordinator.SubmitSpeakerVerificationAt(fragment, challenge.Deadline); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("SubmitSpeakerVerificationAt(consumed) error = %v, want StaleInput", err)
	}
}

func TestSpeakerVerificationCoordinatorDefersAuthorizationUntilResolution(t *testing.T) {
	openedAt := testNow()
	tests := []struct {
		name        string
		permissions privacy.Snapshot
		enrollments biometric.Snapshot
		reason      Reason
	}{
		{name: "permission missing", permissions: permissionSnapshot(openedAt), reason: ReasonBiometricPermissionMissing},
		{name: "enrollment missing", permissions: permissionSnapshot(openedAt, privacy.MicrophoneCapture, privacy.SpeakerVerification), reason: ReasonEnrollmentUnavailable},
		{
			name:        "model mismatch",
			permissions: permissionSnapshot(openedAt, privacy.MicrophoneCapture, privacy.SpeakerVerification),
			enrollments: biometric.Snapshot{Revision: 1, Records: []biometric.Record{activeEnrollment("profile-a", readiness.SpeakerVerification, "different-model", openedAt)}},
			reason:      ReasonModelVersionMismatch,
		},
		{
			name:        "consent revoked",
			permissions: permissionSnapshot(openedAt, privacy.MicrophoneCapture, privacy.SpeakerVerification),
			enrollments: biometric.Snapshot{Revision: 1, Records: []biometric.Record{func() biometric.Record {
				record := activeEnrollment("profile-a", readiness.SpeakerVerification, "speaker-verification-model.v1", openedAt)
				record.Consented = false
				return record
			}()}},
			reason: ReasonEnrollmentUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
			challenge, err := coordinator.IssueChallenge("verification-challenge-auth", "evidence-window-auth", "profile-a", openedAt)
			if err != nil {
				t.Fatalf("IssueChallenge() error = %v", err)
			}
			fragment := validSpeakerVerificationFragment(challenge.ID, challenge.EvidenceWindowID, "verification-fragment-auth", openedAt, 1)
			receipt, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt)
			if err != nil || receipt.Status != FragmentAccepted {
				t.Fatalf("receipt = %#v, %v; authorization leaked into submission", receipt, err)
			}
			result, err := coordinator.AdvanceAt(challenge.Wakeup(), testPolicy(), test.permissions, test.enrollments, challenge.Deadline)
			if err != nil {
				t.Fatalf("AdvanceAt() error = %v", err)
			}
			if !result.Resolved {
				t.Fatal("AdvanceAt() did not consume due challenge")
			}
			assertAnonymousReason(t, result.Resolution, test.reason)
		})
	}
}

func TestSpeakerVerificationCoordinatorUsesHalfOpenChallengeWindow(t *testing.T) {
	openedAt := testNow()
	tests := []struct {
		name       string
		occurredAt time.Time
		arrivedAt  time.Time
		wantCode   fault.Code
	}{
		{name: "opening instant accepted", occurredAt: openedAt, arrivedAt: openedAt},
		{name: "last instant accepted", occurredAt: openedAt.Add(time.Second - time.Nanosecond), arrivedAt: openedAt.Add(time.Second - time.Nanosecond)},
		{name: "occurrence before opening stale", occurredAt: openedAt.Add(-time.Nanosecond), arrivedAt: openedAt, wantCode: fault.StaleInput},
		{name: "arrival before opening invalid", occurredAt: openedAt, arrivedAt: openedAt.Add(-time.Nanosecond), wantCode: fault.InvalidInput},
		{name: "occurrence after arrival invalid", occurredAt: openedAt.Add(time.Nanosecond), arrivedAt: openedAt, wantCode: fault.InvalidInput},
		{name: "occurrence at deadline stale", occurredAt: openedAt.Add(time.Second), arrivedAt: openedAt.Add(time.Second - time.Nanosecond), wantCode: fault.StaleInput},
		{name: "arrival at deadline stale", occurredAt: openedAt, arrivedAt: openedAt.Add(time.Second), wantCode: fault.StaleInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
			challenge, err := coordinator.IssueChallenge("verification-challenge-window", "evidence-window-boundary", "profile-a", openedAt)
			if err != nil {
				t.Fatalf("IssueChallenge() error = %v", err)
			}
			fragment := validSpeakerVerificationFragment(challenge.ID, challenge.EvidenceWindowID, "verification-fragment-window", test.occurredAt, 1)
			receipt, submitErr := coordinator.SubmitSpeakerVerificationAt(fragment, test.arrivedAt)
			if test.wantCode == "" {
				if submitErr != nil || receipt.Status != FragmentAccepted {
					t.Fatalf("SubmitSpeakerVerificationAt() = %#v, %v", receipt, submitErr)
				}
				return
			}
			if !fault.IsCode(submitErr, test.wantCode) {
				t.Fatalf("SubmitSpeakerVerificationAt() error = %v, want %s", submitErr, test.wantCode)
			}
		})
	}
}

func TestSpeakerVerificationCoordinatorDeduplicatesAndAllowsOnlyOneSubmission(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
	challenge, err := coordinator.IssueChallenge("verification-challenge-dedup", "evidence-window-dedup", "profile-a", openedAt)
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	fragment := validSpeakerVerificationFragment(challenge.ID, challenge.EvidenceWindowID, "verification-fragment-1", openedAt, 1)
	if receipt, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt); err != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("first submission = %#v, %v", receipt, err)
	}
	if receipt, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt); err != nil || receipt.Status != FragmentDuplicate {
		t.Fatalf("duplicate submission = %#v, %v", receipt, err)
	}
	changed := fragment
	changed.Candidate.Score = 0.5
	if _, err := coordinator.SubmitSpeakerVerificationAt(changed, openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("changed duplicate error = %v, want InvalidInput", err)
	}
	second := fragment
	second.FragmentID = "verification-fragment-2"
	if _, err := coordinator.SubmitSpeakerVerificationAt(second, openedAt); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("second submission error = %v, want PolicyBlocked", err)
	}
}

func TestSpeakerVerificationCoordinatorRejectsEvidenceWindowMismatch(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
	challenge, err := coordinator.IssueChallenge("verification-challenge-window-binding", "evidence-window-bound", "profile-a", openedAt)
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	fragment := validSpeakerVerificationFragment(challenge.ID, "evidence-window-other", "verification-fragment-window-binding", openedAt, 1)
	if _, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("SubmitSpeakerVerificationAt(window mismatch) error = %v, want StaleInput", err)
	}
	fragment.WindowToken = challenge.EvidenceWindowID
	if receipt, err := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt); err != nil || receipt.Status != FragmentAccepted {
		t.Fatalf("SubmitSpeakerVerificationAt(correct window) = %#v, %v", receipt, err)
	}
}

func TestSpeakerVerificationCoordinatorConsumesNoResponseAndResolutionErrors(t *testing.T) {
	openedAt := testNow()
	t.Run("no response", func(t *testing.T) {
		coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
		challenge, err := coordinator.IssueChallenge("verification-challenge-empty", "evidence-window-empty", "profile-a", openedAt)
		if err != nil {
			t.Fatalf("IssueChallenge() error = %v", err)
		}
		result, err := coordinator.AdvanceAt(challenge.Wakeup(), testPolicy(), privacy.Snapshot{}, biometric.Snapshot{}, challenge.Deadline)
		if err != nil {
			t.Fatalf("AdvanceAt() error = %v", err)
		}
		if !result.Resolved {
			t.Fatal("AdvanceAt() did not resolve empty challenge")
		}
		assertAnonymousReason(t, result.Resolution, ReasonNoMatch)
		if _, err := coordinator.IssueChallenge("verification-challenge-after-close", "evidence-window-after-close", "profile-a", challenge.Deadline); err != nil {
			t.Fatalf("IssueChallenge(after close) error = %v; consumed challenge did not release capacity", err)
		}
	})
	t.Run("invalid policy consumes", func(t *testing.T) {
		coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
		challenge, err := coordinator.IssueChallenge("verification-challenge-error", "evidence-window-error", "profile-a", openedAt)
		if err != nil {
			t.Fatalf("IssueChallenge() error = %v", err)
		}
		if _, err := coordinator.AdvanceAt(challenge.Wakeup(), Policy{}, privacy.Snapshot{}, biometric.Snapshot{}, challenge.Deadline); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("AdvanceAt(invalid policy) error = %v, want InvalidInput", err)
		}
		retry, err := coordinator.AdvanceAt(challenge.Wakeup(), testPolicy(), privacy.Snapshot{}, biometric.Snapshot{}, challenge.Deadline)
		if err != nil || retry.Resolved {
			t.Fatalf("AdvanceAt(retry) = %#v, %v, want consumed no-op", retry, err)
		}
	})
}

func TestSpeakerVerificationCoordinatorValidatesConfigurationCapacityAndInput(t *testing.T) {
	for _, config := range []SpeakerVerificationCoordinatorConfig{
		{},
		{ChallengeDuration: time.Second},
		{ChallengeDuration: time.Second, MaxOpenChallenges: -1},
		{ChallengeDuration: time.Second, MaxOpenChallenges: maxSpeakerVerificationChallenges + 1},
	} {
		if _, err := NewSpeakerVerificationCoordinator(config); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("NewSpeakerVerificationCoordinator(%#v) error = %v, want InvalidInput", config, err)
		}
	}

	openedAt := testNow()
	coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
	if _, err := coordinator.IssueChallenge("verification-capacity-1", "evidence-window-capacity-1", "profile-a", openedAt); err != nil {
		t.Fatalf("IssueChallenge(first) error = %v", err)
	}
	if _, err := coordinator.IssueChallenge("verification-capacity-2", "evidence-window-capacity-2", "profile-b", openedAt); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("IssueChallenge(capacity) error = %v, want Unavailable", err)
	}
	if _, err := coordinator.IssueChallenge("verification-capacity-1", "evidence-window-capacity-1", "profile-a", openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("IssueChallenge(duplicate) error = %v, want InvalidInput", err)
	}

	tests := []struct {
		name     string
		fragment SpeakerVerificationFragment
	}{
		{name: "unknown challenge", fragment: validSpeakerVerificationFragment("verification-unknown", "evidence-window-unknown", "fragment", openedAt, 1)},
		{name: "invalid score", fragment: validSpeakerVerificationFragment("verification-capacity-1", "evidence-window-capacity-1", "fragment", openedAt, math.NaN())},
		{name: "missing model", fragment: func() SpeakerVerificationFragment {
			fragment := validSpeakerVerificationFragment("verification-capacity-1", "evidence-window-capacity-1", "fragment", openedAt, 1)
			fragment.Candidate.ModelVersion = ""
			return fragment
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := coordinator.SubmitSpeakerVerificationAt(test.fragment, openedAt)
			want := fault.InvalidInput
			if test.name == "unknown challenge" {
				want = fault.StaleInput
			}
			if !fault.IsCode(err, want) {
				t.Fatalf("SubmitSpeakerVerificationAt() error = %v, want %s", err, want)
			}
		})
	}
	tooLong := strings.Repeat("x", maxSpeakerVerificationIDBytes+1)
	if _, err := coordinator.IssueChallenge(tooLong, "evidence-window-long", "profile-a", openedAt); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("IssueChallenge(long id) error = %v, want InvalidInput", err)
	}
}

func TestSpeakerVerificationCoordinatorSerializesConcurrentDuplicateSubmissions(t *testing.T) {
	openedAt := testNow()
	coordinator := newTestSpeakerVerificationCoordinator(t, time.Second, 1)
	challenge, err := coordinator.IssueChallenge("verification-concurrent", "evidence-window-concurrent", "profile-a", openedAt)
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	fragment := validSpeakerVerificationFragment(challenge.ID, challenge.EvidenceWindowID, "verification-fragment-concurrent", openedAt, 1)

	const submissions = 16
	start := make(chan struct{})
	results := make(chan FragmentReceipt, submissions)
	errors := make(chan error, submissions)
	var workers sync.WaitGroup
	workers.Add(submissions)
	for range submissions {
		go func() {
			defer workers.Done()
			<-start
			receipt, submitErr := coordinator.SubmitSpeakerVerificationAt(fragment, openedAt)
			results <- receipt
			errors <- submitErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)

	accepted, duplicates := 0, 0
	for receipt := range results {
		switch receipt.Status {
		case FragmentAccepted:
			accepted++
		case FragmentDuplicate:
			duplicates++
		default:
			t.Fatalf("unexpected receipt %#v", receipt)
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatalf("SubmitSpeakerVerificationAt() error = %v", err)
		}
	}
	if accepted != 1 || duplicates != submissions-1 {
		t.Fatalf("accepted=%d duplicates=%d, want 1/%d", accepted, duplicates, submissions-1)
	}
}

func newTestSpeakerVerificationCoordinator(t *testing.T, duration time.Duration, capacity int) *SpeakerVerificationCoordinator {
	t.Helper()
	coordinator, err := NewSpeakerVerificationCoordinator(SpeakerVerificationCoordinatorConfig{
		ChallengeDuration: duration,
		MaxOpenChallenges: capacity,
	})
	if err != nil {
		t.Fatalf("NewSpeakerVerificationCoordinator() error = %v", err)
	}
	return coordinator
}

func validSpeakerVerificationFragment(challengeID, windowToken, fragmentID string, occurredAt time.Time, score float64) SpeakerVerificationFragment {
	return SpeakerVerificationFragment{
		ChallengeID: challengeID,
		WindowToken: windowToken,
		FragmentID:  fragmentID,
		OccurredAt:  occurredAt,
		Candidate: SpeakerVerificationSubmissionCandidate{
			ID: "verification-candidate-1", Score: score, ModelVersion: "speaker-verification-model.v1",
		},
	}
}
