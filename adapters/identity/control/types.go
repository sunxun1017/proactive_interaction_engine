package control

import "time"

// Config makes every retained watcher and materialized-template bound
// explicit. Values above the package hard limits are rejected.
type Config struct {
	MaxWatchers           int
	MaxTemplatesPerWork   int
	MaxTemplateBytes      int
	MaxTotalTemplateBytes int
}

// IdentificationTemplate is adapter-private materialized identification work.
// Encoded is copied before it is retained or sent.
type IdentificationTemplate struct {
	ProfileRef string
	Encoded    []byte
}

type FaceDetectionTask struct{}

type FaceIdentificationTask struct {
	Templates []IdentificationTemplate
}

type FaceLivenessTask struct{}

// VisionWork is one desired vision identity task set in a half-open evidence
// window. Message-like pointers preserve absent versus explicitly present
// tasks.
type VisionWork struct {
	EvidenceWindowID string
	OpenedAt         time.Time
	Deadline         time.Time
	Detection        *FaceDetectionTask
	Identification   *FaceIdentificationTask
	Liveness         *FaceLivenessTask
}

type SpeakerIdentificationWork struct {
	EvidenceWindowID string
	OpenedAt         time.Time
	Deadline         time.Time
	Templates        []IdentificationTemplate
}

// SpeakerVerificationWork deliberately has no profile or template reference.
// The application-owned challenge has already selected ExpectedEncoded.
type SpeakerVerificationWork struct {
	ChallengeID      string
	EvidenceWindowID string
	OpenedAt         time.Time
	Deadline         time.Time
	ExpectedEncoded  []byte
}

// AudioWork is a strict oneof: exactly one pointer must be present.
type AudioWork struct {
	Identification *SpeakerIdentificationWork
	Verification   *SpeakerVerificationWork
}
