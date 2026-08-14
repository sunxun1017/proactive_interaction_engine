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
		name   string
		value  any
		fields []string
	}{
		{
			name:  "face identification",
			value: FaceIdentificationCandidate{},
			fields: []string{
				"ID", "ProfileRef", "Score", "ModelVersion", "OccurredAt", "Liveness",
			},
		},
		{
			name:  "speaker identification",
			value: SpeakerIdentificationCandidate{},
			fields: []string{
				"ID", "ProfileRef", "Score", "ModelVersion", "OccurredAt",
			},
		},
		{
			name:  "speaker verification",
			value: SpeakerVerificationCandidate{},
			fields: []string{
				"ID", "ProfileRef", "Score", "ModelVersion", "OccurredAt",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			typeOf := reflect.TypeOf(test.value)
			if typeOf.NumField() != len(test.fields) {
				t.Fatalf("candidate fields = %d, want metadata-only %d", typeOf.NumField(), len(test.fields))
			}
			for index, name := range test.fields {
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

func TestResolveAtRecognizesExactThresholdAndMatchingModalities(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
		profile  string
		reason   Reason
	}{
		{
			name: "face only",
			evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", policy.FaceIdentificationThreshold, now, LivenessPassed),
			}},
			profile: "profile-a",
			reason:  ReasonFaceIdentified,
		},
		{
			name: "speaker only",
			evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
				speakerCandidate("speaker-1", "profile-a", policy.SpeakerIdentificationThreshold, now),
			}},
			profile: "profile-a",
			reason:  ReasonSpeakerIdentified,
		},
		{
			name: "matching face and speaker",
			evidence: Evidence{
				FacesObserved: 1,
				FaceIdentifications: []FaceIdentificationCandidate{
					faceCandidate("face-1", "profile-a", policy.FaceIdentificationThreshold, now, LivenessPassed),
				},
				SpeakerIdentifications: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-1", "profile-a", policy.SpeakerIdentificationThreshold, now),
				},
			},
			profile: "profile-a",
			reason:  ReasonModalitiesMatched,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			if resolution.Assurance != Recognized || resolution.ProfileRef != test.profile || resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v", resolution)
			}
			if resolution.PolicyVersion != policy.Version {
				t.Fatalf("policy version = %q, want %q", resolution.PolicyVersion, policy.Version)
			}
		})
	}
}

func TestResolveAtVerifiesOnlyTheExplicitSpeakerVerificationTarget(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	verification := Evidence{
		ExpectedProfileRef: "profile-a",
		SpeakerVerifications: []SpeakerVerificationCandidate{
			verificationCandidate("verification-1", "profile-a", policy.SpeakerVerificationThreshold, now),
		},
	}
	resolution, err := ResolveAt(policy, verification, now)
	if err != nil {
		t.Fatalf("ResolveAt(verification) error = %v", err)
	}
	if resolution.Assurance != Verified || resolution.ProfileRef != "profile-a" || resolution.Reason != ReasonSpeakerVerified {
		t.Fatalf("verification resolution = %#v", resolution)
	}

	identification := Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
		speakerCandidate("identification-1", "profile-a", 1, now),
	}}
	resolution, err = ResolveAt(policy, identification, now)
	if err != nil {
		t.Fatalf("ResolveAt(identification) error = %v", err)
	}
	if resolution.Assurance != Recognized {
		t.Fatalf("identification assurance = %q, want RECOGNIZED", resolution.Assurance)
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
				FacesObserved: 1,
				FaceIdentifications: []FaceIdentificationCandidate{
					faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
				},
				SpeakerIdentifications: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-1", "profile-b", 1, now),
				},
			},
			reason: ReasonModalityConflict,
		},
		{
			name: "multiple face matches",
			evidence: Evidence{FacesObserved: 2, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
				faceCandidate("face-2", "profile-b", 1, now, LivenessPassed),
			}},
			reason: ReasonMultipleFaceMatches,
		},
		{
			name: "multiple people with one matching face",
			evidence: Evidence{FacesObserved: 2, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
			}},
			reason: ReasonMultiplePeople,
		},
		{
			name: "multiple speaker matches",
			evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
				speakerCandidate("speaker-1", "profile-a", 1, now),
				speakerCandidate("speaker-2", "profile-b", 1, now),
			}},
			reason: ReasonMultipleSpeakerMatches,
		},
		{
			name: "required liveness missing",
			evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", 1, now, LivenessUnknown),
			}},
			reason: ReasonLivenessMissing,
		},
		{
			name: "required liveness failed",
			evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", 1, now, LivenessFailed),
			}},
			reason: ReasonLivenessFailed,
		},
		{
			name: "evidence expired",
			evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
				speakerCandidate("speaker-1", "profile-a", 1, now.Add(-policy.MaxEvidenceAge-time.Nanosecond)),
			}},
			reason: ReasonEvidenceExpired,
		},
		{
			name: "modalities outside skew",
			evidence: Evidence{
				FacesObserved: 1,
				FaceIdentifications: []FaceIdentificationCandidate{
					faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
				},
				SpeakerIdentifications: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-1", "profile-a", 1, now.Add(-policy.MaxEvidenceSkew-time.Nanosecond)),
				},
			},
			reason: ReasonEvidenceTimeSkew,
		},
		{
			name: "verification target mismatch",
			evidence: Evidence{
				ExpectedProfileRef: "profile-a",
				SpeakerVerifications: []SpeakerVerificationCandidate{
					verificationCandidate("verification-1", "profile-b", 1, now),
				},
			},
			reason: ReasonVerificationTargetMismatch,
		},
		{
			name: "all candidates below threshold",
			evidence: Evidence{
				FacesObserved: 1,
				FaceIdentifications: []FaceIdentificationCandidate{
					faceCandidate("face-1", "profile-a", policy.FaceIdentificationThreshold-0.01, now, LivenessPassed),
				},
				SpeakerIdentifications: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-1", "profile-a", policy.SpeakerIdentificationThreshold-0.01, now),
				},
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
			if resolution.Assurance != Anonymous || resolution.ProfileRef != "" || resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v, want ANONYMOUS/%s", resolution, test.reason)
			}
		})
	}
}

func TestResolveAtAcceptsExactEvidenceAgeAndSkewBoundaries(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name     string
		evidence Evidence
		reason   Reason
	}{
		{
			name: "exact maximum age",
			evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
				speakerCandidate("speaker-1", "profile-a", 1, now.Add(-policy.MaxEvidenceAge)),
			}},
			reason: ReasonSpeakerIdentified,
		},
		{
			name: "exact maximum modality skew",
			evidence: Evidence{
				FacesObserved: 1,
				FaceIdentifications: []FaceIdentificationCandidate{
					faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
				},
				SpeakerIdentifications: []SpeakerIdentificationCandidate{
					speakerCandidate("speaker-1", "profile-a", 1, now.Add(-policy.MaxEvidenceSkew)),
				},
			},
			reason: ReasonModalitiesMatched,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAt(policy, test.evidence, now)
			if err != nil {
				t.Fatalf("ResolveAt() error = %v", err)
			}
			if resolution.Assurance != Recognized || resolution.ProfileRef != "profile-a" || resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v", resolution)
			}
		})
	}
}

func TestResolveAtIsIndependentOfCandidateOrder(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	faceWinner := faceCandidate("face-winner", "profile-a", 1, now, LivenessPassed)
	faceNoise := faceCandidate("face-noise", "profile-b", policy.FaceIdentificationThreshold/2, now, LivenessPassed)
	speakerWinner := speakerCandidate("speaker-winner", "profile-a", 1, now)
	speakerNoise := speakerCandidate("speaker-noise", "profile-b", policy.SpeakerIdentificationThreshold/2, now)
	left := Evidence{
		FacesObserved:          1,
		FaceIdentifications:    []FaceIdentificationCandidate{faceWinner, faceNoise},
		SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerWinner, speakerNoise},
	}
	right := Evidence{
		FacesObserved:          1,
		FaceIdentifications:    []FaceIdentificationCandidate{faceNoise, faceWinner},
		SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerNoise, speakerWinner},
	}
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
		{name: "zero evaluation time", policy: validPolicy, evidence: validFaceEvidence(now), now: time.Time{}},
		{name: "missing candidate id", policy: validPolicy, evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{faceCandidate("", "profile-a", 1, now, LivenessPassed)}}, now: now},
		{name: "missing profile ref", policy: validPolicy, evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{faceCandidate("face-1", "", 1, now, LivenessPassed)}}, now: now},
		{name: "missing model version", policy: validPolicy, evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{{ID: "face-1", ProfileRef: "profile-a", Score: 1, OccurredAt: now, Liveness: LivenessPassed}}}, now: now},
		{name: "zero occurred at", policy: validPolicy, evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("speaker-1", "profile-a", 1, time.Time{})}}, now: now},
		{name: "NaN score", policy: validPolicy, evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("speaker-1", "profile-a", math.NaN(), now)}}, now: now},
		{name: "score above one", policy: validPolicy, evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("speaker-1", "profile-a", 1.1, now)}}, now: now},
		{name: "unknown liveness", policy: validPolicy, evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{faceCandidate("face-1", "profile-a", 1, now, Liveness("UNKNOWN_VALUE"))}}, now: now},
		{name: "face evidence missing observed count", policy: validPolicy, evidence: Evidence{FaceIdentifications: []FaceIdentificationCandidate{faceCandidate("face-1", "profile-a", 1, now, LivenessPassed)}}, now: now},
		{name: "observed count without face evidence", policy: validPolicy, evidence: Evidence{FacesObserved: 1, SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("speaker-1", "profile-a", 1, now)}}, now: now},
		{name: "future evidence time", policy: validPolicy, evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("speaker-1", "profile-a", 1, now.Add(time.Nanosecond))}}, now: now},
		{name: "duplicate candidate id across kinds", policy: validPolicy, evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{faceCandidate("candidate-1", "profile-a", 1, now, LivenessPassed)}, SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("candidate-1", "profile-a", 1, now)}}, now: now},
		{name: "identification and verification mixed", policy: validPolicy, evidence: Evidence{ExpectedProfileRef: "profile-a", SpeakerIdentifications: []SpeakerIdentificationCandidate{speakerCandidate("identification-1", "profile-a", 1, now)}, SpeakerVerifications: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-a", 1, now)}}, now: now},
		{name: "verification missing explicit target", policy: validPolicy, evidence: Evidence{SpeakerVerifications: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-a", 1, now)}}, now: now},
		{name: "empty evidence", policy: validPolicy, now: now},
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
	return Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{
		faceCandidate("face-1", "profile-a", 1, at, LivenessPassed),
	}}
}

func faceCandidate(id, profile string, score float64, at time.Time, liveness Liveness) FaceIdentificationCandidate {
	return FaceIdentificationCandidate{
		ID: id, ProfileRef: profile, Score: score, ModelVersion: "face-model.v1", OccurredAt: at, Liveness: liveness,
	}
}

func speakerCandidate(id, profile string, score float64, at time.Time) SpeakerIdentificationCandidate {
	return SpeakerIdentificationCandidate{
		ID: id, ProfileRef: profile, Score: score, ModelVersion: "speaker-model.v1", OccurredAt: at,
	}
}

func verificationCandidate(id, profile string, score float64, at time.Time) SpeakerVerificationCandidate {
	return SpeakerVerificationCandidate{
		ID: id, ProfileRef: profile, Score: score, ModelVersion: "speaker-verification-model.v1", OccurredAt: at,
	}
}
