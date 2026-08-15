package identity

import "time"

// Liveness is the face liveness result attached by an isolated provider.
type Liveness string

const (
	LivenessUnknown Liveness = "UNKNOWN"
	LivenessPassed  Liveness = "PASSED"
	LivenessFailed  Liveness = "FAILED"
)

// FaceIdentificationCandidate contains only identity metadata produced by a
// face identification provider.
type FaceIdentificationCandidate struct {
	ID           string
	ProfileRef   string
	Score        float64
	ModelVersion string
}

// SpeakerIdentificationCandidate contains only identity metadata produced by
// an open-set speaker identification provider.
type SpeakerIdentificationCandidate struct {
	ID           string
	ProfileRef   string
	Score        float64
	ModelVersion string
}

// SpeakerVerificationCandidate contains only identity metadata produced by a
// provider verifying one explicitly requested profile.
type SpeakerVerificationCandidate struct {
	ID           string
	ProfileRef   string
	Score        float64
	ModelVersion string
}

// FaceDetectionEvidence is one complete face-count result. A zero count is an
// explicit observation and differs from an absent detection fragment.
type FaceDetectionEvidence struct {
	OccurredAt    time.Time
	FacesObserved uint32
}

// FaceIdentificationEvidence is one complete open-set face match result. An
// empty candidate slice is a valid no-match result.
type FaceIdentificationEvidence struct {
	OccurredAt time.Time
	Candidates []FaceIdentificationCandidate
}

// FaceLivenessEvidence is one independent liveness result.
type FaceLivenessEvidence struct {
	OccurredAt time.Time
	State      Liveness
}

// SpeakerIdentificationEvidence is one complete open-set speaker match
// result. An empty candidate slice is a valid no-match result.
type SpeakerIdentificationEvidence struct {
	OccurredAt time.Time
	Candidates []SpeakerIdentificationCandidate
}

// SpeakerVerificationEvidence verifies candidates only against the explicit
// expected profile supplied by application-owned challenge state.
type SpeakerVerificationEvidence struct {
	OccurredAt         time.Time
	ExpectedProfileRef string
	Candidates         []SpeakerVerificationCandidate
}

// Evidence is one deterministic, presence-aware resolution input.
// Identification and verification fragments are mutually exclusive.
type Evidence struct {
	FaceDetection         *FaceDetectionEvidence
	FaceIdentification    *FaceIdentificationEvidence
	FaceLiveness          *FaceLivenessEvidence
	SpeakerIdentification *SpeakerIdentificationEvidence
	SpeakerVerification   *SpeakerVerificationEvidence
}

// Policy explicitly supplies every threshold and temporal bound used by the
// resolver. This package defines no defaults.
type Policy struct {
	Version                        string
	FaceIdentificationThreshold    float64
	SpeakerIdentificationThreshold float64
	SpeakerVerificationThreshold   float64
	MaxEvidenceAge                 time.Duration
	MaxEvidenceSkew                time.Duration
	RequireFaceLiveness            bool
}

// Assurance is the trust level of one resolution.
type Assurance string

const (
	Anonymous  Assurance = "ANONYMOUS"
	Recognized Assurance = "RECOGNIZED"
	Verified   Assurance = "VERIFIED"
)

// Reason is the stable machine-readable explanation for a resolution.
type Reason string

const (
	ReasonNoMatch                     Reason = "NO_MATCH"
	ReasonFaceIdentified              Reason = "FACE_IDENTIFIED"
	ReasonSpeakerIdentified           Reason = "SPEAKER_IDENTIFIED"
	ReasonModalitiesMatched           Reason = "MODALITIES_MATCHED"
	ReasonSpeakerVerified             Reason = "SPEAKER_VERIFIED"
	ReasonModalityConflict            Reason = "MODALITY_CONFLICT"
	ReasonMultipleFaceMatches         Reason = "MULTIPLE_FACE_MATCHES"
	ReasonMultiplePeople              Reason = "MULTIPLE_PEOPLE"
	ReasonMultipleSpeakerMatches      Reason = "MULTIPLE_SPEAKER_MATCHES"
	ReasonMultipleVerificationMatches Reason = "MULTIPLE_VERIFICATION_MATCHES"
	ReasonLivenessMissing             Reason = "LIVENESS_MISSING"
	ReasonLivenessFailed              Reason = "LIVENESS_FAILED"
	ReasonFaceDetectionMismatch       Reason = "FACE_DETECTION_MISMATCH"
	ReasonEvidenceExpired             Reason = "EVIDENCE_EXPIRED"
	ReasonEvidenceTimeSkew            Reason = "EVIDENCE_TIME_SKEW"
	ReasonVerificationTargetMismatch  Reason = "VERIFICATION_TARGET_MISMATCH"
	ReasonBiometricPermissionMissing  Reason = "BIOMETRIC_PERMISSION_MISSING"
	ReasonEnrollmentUnavailable       Reason = "ENROLLMENT_UNAVAILABLE"
	ReasonModelVersionMismatch        Reason = "MODEL_VERSION_MISMATCH"
)

// Resolution is the deterministic identity result for one policy version.
type Resolution struct {
	Assurance     Assurance
	ProfileRef    string
	Reason        Reason
	PolicyVersion string
}
