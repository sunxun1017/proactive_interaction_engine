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
	OccurredAt   time.Time
	Liveness     Liveness
}

// SpeakerIdentificationCandidate contains only identity metadata produced by
// an open-set speaker identification provider.
type SpeakerIdentificationCandidate struct {
	ID           string
	ProfileRef   string
	Score        float64
	ModelVersion string
	OccurredAt   time.Time
}

// SpeakerVerificationCandidate contains only identity metadata produced by a
// provider verifying one explicitly requested profile.
type SpeakerVerificationCandidate struct {
	ID           string
	ProfileRef   string
	Score        float64
	ModelVersion string
	OccurredAt   time.Time
}

// Evidence is one deterministic resolution input. Identification and
// verification evidence are mutually exclusive.
type Evidence struct {
	FacesObserved          uint32
	ExpectedProfileRef     string
	FaceIdentifications    []FaceIdentificationCandidate
	SpeakerIdentifications []SpeakerIdentificationCandidate
	SpeakerVerifications   []SpeakerVerificationCandidate
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
	ReasonEvidenceExpired             Reason = "EVIDENCE_EXPIRED"
	ReasonEvidenceTimeSkew            Reason = "EVIDENCE_TIME_SKEW"
	ReasonVerificationTargetMismatch  Reason = "VERIFICATION_TARGET_MISMATCH"
)

// Resolution is the deterministic identity result for one policy version.
type Resolution struct {
	Assurance     Assurance
	ProfileRef    string
	Reason        Reason
	PolicyVersion string
}
