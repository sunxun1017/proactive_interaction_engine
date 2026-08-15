package identity

import (
	"math"
	"reflect"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestCandidateTypesExposeOnlyIdentityMetadata(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "face identification", value: FaceIdentificationCandidate{}},
		{name: "speaker identification", value: SpeakerIdentificationCandidate{}},
		{name: "speaker verification", value: SpeakerVerificationCandidate{}},
	}
	want := []string{"ID", "ProfileRef", "Score", "ModelVersion"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			typeOf := reflect.TypeOf(test.value)
			if typeOf.NumField() != len(want) {
				t.Fatalf("candidate fields = %d, want metadata-only %d", typeOf.NumField(), len(want))
			}
			for index, name := range want {
				if got := typeOf.Field(index).Name; got != name {
					t.Fatalf("field %d = %q, want %q", index, got, name)
				}
			}
		})
	}
	if reflect.TypeOf(FaceIdentificationCandidate{}) == reflect.TypeOf(SpeakerIdentificationCandidate{}) ||
		reflect.TypeOf(SpeakerIdentificationCandidate{}) == reflect.TypeOf(SpeakerVerificationCandidate{}) {
		t.Fatal("identification and verification candidate types must remain distinct")
	}
}

func TestResolveAtDistinguishesMissingAndExplicitEmptyFragments(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
	}{
		{name: "no fragments", evidence: Evidence{}},
		{name: "empty face identification", evidence: Evidence{FaceIdentification: &FaceIdentificationEvidence{OccurredAt: now}}},
		{name: "empty speaker identification", evidence: Evidence{SpeakerIdentification: &SpeakerIdentificationEvidence{OccurredAt: now}}},
		{name: "explicit zero faces", evidence: Evidence{FaceDetection: &FaceDetectionEvidence{OccurredAt: now, FacesObserved: 0}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, ReasonNoMatch)
		})
	}
}

func TestResolveAtRecognizesIndependentModalities(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
		profile  string
		reason   Reason
	}{
		{
			name: "face with independent detection and liveness",
			evidence: Evidence{
				FaceDetection:      faceDetection(now, 1),
				FaceIdentification: faceIdentification(now, faceCandidate("face-1", "profile-a", policy.FaceIdentificationThreshold)),
				FaceLiveness:       faceLiveness(now, LivenessPassed),
			},
			profile: "profile-a", reason: ReasonFaceIdentified,
		},
		{
			name: "speaker without face detection",
			evidence: Evidence{
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-b", 1)),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", policy.SpeakerIdentificationThreshold)),
			},
			profile: "profile-a", reason: ReasonSpeakerIdentified,
		},
		{
			name: "matching face and speaker",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 1),
				FaceIdentification:    faceIdentification(now, faceCandidate("provider-local-1", "profile-a", 1)),
				FaceLiveness:          faceLiveness(now, LivenessPassed),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("provider-local-1", "profile-a", 1)),
			},
			profile: "profile-a", reason: ReasonModalitiesMatched,
		},
		{
			name: "speaker remains available when face liveness is missing",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 1),
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-b", 1)),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1)),
			},
			profile: "profile-a", reason: ReasonSpeakerIdentified,
		},
		{
			name: "speaker remains available when face liveness fails",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 1),
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-b", 1)),
				FaceLiveness:          faceLiveness(now, LivenessFailed),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1)),
			},
			profile: "profile-a", reason: ReasonSpeakerIdentified,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			if resolution.Assurance != Recognized || resolution.ProfileRef != test.profile || resolution.Reason != test.reason || resolution.PolicyVersion != policy.Version {
				t.Fatalf("resolution = %#v", resolution)
			}
		})
	}
}

func TestResolveAtPrioritizesMultiplePeopleAndDetectionMismatch(t *testing.T) {
	now := testNow()
	strongSpeaker := speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1))
	tests := []struct {
		name     string
		evidence Evidence
		reason   Reason
	}{
		{
			name: "multiple people blocks empty face match and strong speaker",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 2),
				FaceIdentification:    faceIdentification(now),
				SpeakerIdentification: strongSpeaker,
			},
			reason: ReasonMultiplePeople,
		},
		{
			name: "zero faces contradicts face candidates",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 0),
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-a", 1)),
				SpeakerIdentification: strongSpeaker,
			},
			reason: ReasonFaceDetectionMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(testPolicy(), test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, test.reason)
		})
	}
}

func TestResolveAtUsesOnlyIndependentLivenessEvidence(t *testing.T) {
	now := testNow()
	base := Evidence{
		FaceDetection:      faceDetection(now, 1),
		FaceIdentification: faceIdentification(now, faceCandidate("face-1", "profile-a", 1)),
	}
	tests := []struct {
		name     string
		liveness *FaceLivenessEvidence
		reason   Reason
	}{
		{name: "missing", reason: ReasonLivenessMissing},
		{name: "unknown", liveness: faceLiveness(now, LivenessUnknown), reason: ReasonLivenessMissing},
		{name: "failed", liveness: faceLiveness(now, LivenessFailed), reason: ReasonLivenessFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := base
			evidence.FaceLiveness = test.liveness
			resolution, err := ResolveAt(testPolicy(), evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, test.reason)
		})
	}
}

func TestResolveAtFallsBackToAnonymousWithStableReason(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
		reason   Reason
	}{
		{
			name: "face and speaker conflict",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 1),
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-a", 1)),
				FaceLiveness:          faceLiveness(now, LivenessPassed),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-b", 1)),
			},
			reason: ReasonModalityConflict,
		},
		{
			name: "multiple face matches",
			evidence: Evidence{
				FaceDetection: faceDetection(now, 1),
				FaceIdentification: faceIdentification(now,
					faceCandidate("face-1", "profile-a", 1),
					faceCandidate("face-2", "profile-b", 1),
				),
				FaceLiveness: faceLiveness(now, LivenessPassed),
			},
			reason: ReasonMultipleFaceMatches,
		},
		{
			name: "multiple speaker matches",
			evidence: Evidence{SpeakerIdentification: speakerIdentification(now,
				speakerCandidate("speaker-1", "profile-a", 1),
				speakerCandidate("speaker-2", "profile-b", 1),
			)},
			reason: ReasonMultipleSpeakerMatches,
		},
		{
			name: "all candidates below threshold",
			evidence: Evidence{
				FaceDetection:         faceDetection(now, 1),
				FaceIdentification:    faceIdentification(now, faceCandidate("face-1", "profile-a", policy.FaceIdentificationThreshold-0.01)),
				FaceLiveness:          faceLiveness(now, LivenessPassed),
				SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", policy.SpeakerIdentificationThreshold-0.01)),
			},
			reason: ReasonNoMatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, test.reason)
		})
	}
}

func TestResolveAtVerifiesOnlyExplicitTarget(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name       string
		candidates []SpeakerVerificationCandidate
		assurance  Assurance
		reason     Reason
		profile    string
	}{
		{name: "exact threshold", candidates: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-a", policy.SpeakerVerificationThreshold)}, assurance: Verified, reason: ReasonSpeakerVerified, profile: "profile-a"},
		{name: "empty candidates", assurance: Anonymous, reason: ReasonNoMatch},
		{name: "target mismatch", candidates: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-b", 1)}, assurance: Anonymous, reason: ReasonVerificationTargetMismatch},
		{name: "multiple matches", candidates: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-a", 1), verificationCandidate("verification-2", "profile-b", 1)}, assurance: Anonymous, reason: ReasonMultipleVerificationMatches},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, Evidence{SpeakerVerification: &SpeakerVerificationEvidence{
				OccurredAt: now, ExpectedProfileRef: "profile-a", Candidates: test.candidates,
			}}, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			if resolution.Assurance != test.assurance || resolution.ProfileRef != test.profile || resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v", resolution)
			}
		})
	}
}

func TestResolveAtAppliesAgeAndSkewAcrossPresentFragments(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
		reason   Reason
	}{
		{name: "expired empty fragment", evidence: Evidence{SpeakerIdentification: speakerIdentification(now.Add(-policy.MaxEvidenceAge - time.Nanosecond))}, reason: ReasonEvidenceExpired},
		{name: "exact maximum age", evidence: Evidence{SpeakerIdentification: speakerIdentification(now.Add(-policy.MaxEvidenceAge), speakerCandidate("speaker-1", "profile-a", 1))}, reason: ReasonSpeakerIdentified},
		{name: "outside skew", evidence: Evidence{FaceDetection: faceDetection(now, 1), SpeakerIdentification: speakerIdentification(now.Add(-policy.MaxEvidenceSkew-time.Nanosecond), speakerCandidate("speaker-1", "profile-a", 1))}, reason: ReasonEvidenceTimeSkew},
		{name: "exact maximum skew", evidence: Evidence{FaceDetection: faceDetection(now, 1), SpeakerIdentification: speakerIdentification(now.Add(-policy.MaxEvidenceSkew), speakerCandidate("speaker-1", "profile-a", 1))}, reason: ReasonSpeakerIdentified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			if resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v, want %s", resolution, test.reason)
			}
		})
	}
}

func TestResolveAtIsIndependentOfCandidateOrder(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	winner := speakerCandidate("speaker-winner", "profile-a", 1)
	noise := speakerCandidate("speaker-noise", "profile-b", policy.SpeakerIdentificationThreshold/2)
	left := Evidence{SpeakerIdentification: speakerIdentification(now, winner, noise)}
	right := Evidence{SpeakerIdentification: speakerIdentification(now, noise, winner)}
	first, err := ResolveAt(policy, left, now)
	if err != nil {
		t.Fatalf("ResolveAt(left) error = %v", err)
	}
	second, err := ResolveAt(policy, right, now)
	if err != nil {
		t.Fatalf("ResolveAt(right) error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("resolution depends on input order: %#v != %#v", first, second)
	}
}

func TestResolveAtRejectsInvalidOrMixedInput(t *testing.T) {
	now := testNow()
	validPolicy := testPolicy()
	tests := []struct {
		name     string
		policy   Policy
		evidence Evidence
		now      time.Time
	}{
		{name: "missing policy version", policy: func() Policy { value := validPolicy; value.Version = ""; return value }(), evidence: validFaceEvidence(now), now: now},
		{name: "NaN face threshold", policy: func() Policy { value := validPolicy; value.FaceIdentificationThreshold = math.NaN(); return value }(), evidence: validFaceEvidence(now), now: now},
		{name: "zero max evidence age", policy: func() Policy { value := validPolicy; value.MaxEvidenceAge = 0; return value }(), evidence: validFaceEvidence(now), now: now},
		{name: "zero max evidence skew", policy: func() Policy { value := validPolicy; value.MaxEvidenceSkew = 0; return value }(), evidence: validFaceEvidence(now), now: now},
		{name: "zero evaluation time", policy: validPolicy, evidence: Evidence{}, now: time.Time{}},
		{name: "zero fragment time", policy: validPolicy, evidence: Evidence{SpeakerIdentification: &SpeakerIdentificationEvidence{}}, now: now},
		{name: "future fragment time", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now.Add(time.Nanosecond))}, now: now},
		{name: "missing candidate id", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("", "profile-a", 1))}, now: now},
		{name: "missing profile ref", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "", 1))}, now: now},
		{name: "missing model version", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, SpeakerIdentificationCandidate{ID: "speaker-1", ProfileRef: "profile-a", Score: 1})}, now: now},
		{name: "NaN score", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", math.NaN()))}, now: now},
		{name: "score above one", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1.1))}, now: now},
		{name: "invalid liveness", policy: validPolicy, evidence: Evidence{FaceLiveness: faceLiveness(now, Liveness("INVALID"))}, now: now},
		{name: "duplicate face candidate id within fragment", policy: validPolicy, evidence: Evidence{FaceIdentification: faceIdentification(now, faceCandidate("candidate-1", "profile-a", 1), faceCandidate("candidate-1", "profile-a", 0.5))}, now: now},
		{name: "duplicate speaker candidate id within fragment", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("candidate-1", "profile-a", 1), speakerCandidate("candidate-1", "profile-a", 0.5))}, now: now},
		{name: "identification and verification fragments mixed", policy: validPolicy, evidence: Evidence{SpeakerIdentification: speakerIdentification(now), SpeakerVerification: &SpeakerVerificationEvidence{OccurredAt: now, ExpectedProfileRef: "profile-a"}}, now: now},
		{name: "verification missing explicit target", policy: validPolicy, evidence: Evidence{SpeakerVerification: &SpeakerVerificationEvidence{OccurredAt: now}}, now: now},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveAt(test.policy, test.evidence, test.now)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("ResolveAt() error = %v, want InvalidInput", err)
			}
		})
	}
}

func testNow() time.Time {
	return time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
}

func testPolicy() Policy {
	return Policy{
		Version:                        "identity-policy.v1",
		FaceIdentificationThreshold:    0.8,
		SpeakerIdentificationThreshold: 0.75,
		SpeakerVerificationThreshold:   0.9,
		MaxEvidenceAge:                 5 * time.Second,
		MaxEvidenceSkew:                time.Second,
		RequireFaceLiveness:            true,
	}
}

func validFaceEvidence(at time.Time) Evidence {
	return Evidence{
		FaceDetection:      faceDetection(at, 1),
		FaceIdentification: faceIdentification(at, faceCandidate("face-1", "profile-a", 1)),
		FaceLiveness:       faceLiveness(at, LivenessPassed),
	}
}

func faceDetection(at time.Time, count uint32) *FaceDetectionEvidence {
	return &FaceDetectionEvidence{OccurredAt: at, FacesObserved: count}
}

func faceIdentification(at time.Time, candidates ...FaceIdentificationCandidate) *FaceIdentificationEvidence {
	return &FaceIdentificationEvidence{OccurredAt: at, Candidates: candidates}
}

func faceLiveness(at time.Time, state Liveness) *FaceLivenessEvidence {
	return &FaceLivenessEvidence{OccurredAt: at, State: state}
}

func speakerIdentification(at time.Time, candidates ...SpeakerIdentificationCandidate) *SpeakerIdentificationEvidence {
	return &SpeakerIdentificationEvidence{OccurredAt: at, Candidates: candidates}
}

func faceCandidate(id, profile string, score float64) FaceIdentificationCandidate {
	return FaceIdentificationCandidate{ID: id, ProfileRef: profile, Score: score, ModelVersion: "face-model.v1"}
}

func speakerCandidate(id, profile string, score float64) SpeakerIdentificationCandidate {
	return SpeakerIdentificationCandidate{ID: id, ProfileRef: profile, Score: score, ModelVersion: "speaker-model.v1"}
}

func verificationCandidate(id, profile string, score float64) SpeakerVerificationCandidate {
	return SpeakerVerificationCandidate{ID: id, ProfileRef: profile, Score: score, ModelVersion: "speaker-verification-model.v1"}
}
