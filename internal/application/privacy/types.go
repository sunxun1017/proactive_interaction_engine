package privacy

import (
	"context"
	"time"
)

// Permission identifies one independently revocable privacy boundary.
type Permission string

const (
	CameraCapture         Permission = "CAMERA_CAPTURE"
	MicrophoneCapture     Permission = "MICROPHONE_CAPTURE"
	FaceDetection         Permission = "FACE_DETECTION"
	FaceIdentification    Permission = "FACE_IDENTIFICATION"
	FaceLiveness          Permission = "FACE_LIVENESS"
	SpeakerIdentification Permission = "SPEAKER_IDENTIFICATION"
	SpeakerVerification   Permission = "SPEAKER_VERIFICATION"
	SpeechTranscription   Permission = "SPEECH_TRANSCRIPTION"
)

var permissionOrder = []Permission{
	CameraCapture,
	MicrophoneCapture,
	FaceDetection,
	FaceIdentification,
	FaceLiveness,
	SpeakerIdentification,
	SpeakerVerification,
	SpeechTranscription,
}

// AllPermissions returns the stable UI and persistence order as a copy.
func AllPermissions() []Permission {
	return append([]Permission(nil), permissionOrder...)
}

// Grant is the desired local permission state, not provider health or proof of
// biometric enrollment.
type Grant struct {
	Permission Permission
	Enabled    bool
	UpdatedAt  time.Time
}

// Snapshot is an immutable, revisioned view of every permission.
type Snapshot struct {
	Revision uint64
	Grants   []Grant
}

// ChangePermission sets desired state. Repeating the same desired state is an
// idempotent no-op.
type ChangePermission struct {
	Permission Permission
	Enabled    bool
}

// Repository persists complete snapshots using optimistic revision checks.
type Repository interface {
	Load(context.Context) (Snapshot, error)
	Save(context.Context, uint64, Snapshot) error
}
