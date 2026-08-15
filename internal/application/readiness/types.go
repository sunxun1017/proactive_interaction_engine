package readiness

import (
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

// CapabilityKind identifies one platform capability without naming a device,
// model, or implementation.
type CapabilityKind string

const (
	PersonPresence        CapabilityKind = "PERSON_PRESENCE"
	FaceDetection         CapabilityKind = "FACE_DETECTION"
	FaceIdentification    CapabilityKind = "FACE_IDENTIFICATION"
	FaceLiveness          CapabilityKind = "FACE_LIVENESS"
	VoiceActivity         CapabilityKind = "VOICE_ACTIVITY"
	SpeakerIdentification CapabilityKind = "SPEAKER_IDENTIFICATION"
	SpeakerVerification   CapabilityKind = "SPEAKER_VERIFICATION"
	SpeechTranscription   CapabilityKind = "SPEECH_TRANSCRIPTION"
	AttentionEstimation   CapabilityKind = "ATTENTION_ESTIMATION"
	BusyState             CapabilityKind = "BUSY_STATE"
	GestureDetection      CapabilityKind = "GESTURE_DETECTION"
	DeviceState           CapabilityKind = "DEVICE_STATE"
	DisplayText           CapabilityKind = "DISPLAY_TEXT"
	AvatarAttend          CapabilityKind = "AVATAR_ATTEND"
	AvatarExpression      CapabilityKind = "AVATAR_EXPRESSION"
	SpeechSynthesis       CapabilityKind = "SPEECH_SYNTHESIS"
	Gesture               CapabilityKind = "GESTURE"
	Light                 CapabilityKind = "LIGHT"
	Locomotion            CapabilityKind = "LOCOMOTION"
)

// CapabilityRequirement binds a required capability to one selected provider.
type CapabilityRequirement struct {
	Kind       CapabilityKind
	ProviderID string
}

// Fallback is a declared product-safe degradation for an optional capability.
type Fallback string

const (
	AnonymousSubject Fallback = "ANONYMOUS_SUBJECT"
	NoVoiceReply     Fallback = "NO_VOICE_REPLY"
	NoTranscript     Fallback = "NO_TRANSCRIPT"
	VisualOnly       Fallback = "VISUAL_ONLY"
	AudioOnly        Fallback = "AUDIO_ONLY"
)

// OptionalCapability binds an optional capability and its deterministic
// fallback to one selected provider.
type OptionalCapability struct {
	Kind       CapabilityKind
	ProviderID string
	Fallback   Fallback
}

// ScenarioRequirements is the capability selection validated before a
// scenario is activated.
type ScenarioRequirements struct {
	ID       string
	Required []CapabilityRequirement
	Optional []OptionalCapability
}

// ProviderHealth is the provider's current application-level health state.
type ProviderHealth string

const (
	Healthy   ProviderHealth = "HEALTHY"
	Unhealthy ProviderHealth = "UNHEALTHY"
)

// ProviderPrivacyClass describes where a provider processes its inputs.
type ProviderPrivacyClass string

const (
	ProviderPrivacyDeviceLocal      ProviderPrivacyClass = "DEVICE_LOCAL"
	ProviderPrivacyRemoteProcessing ProviderPrivacyClass = "REMOTE_PROCESSING"
)

// ProviderCancellationSemantics describes the provider's weakest software
// cancellation guarantee. It does not describe a physical emergency stop.
type ProviderCancellationSemantics string

const (
	ProviderCancellationNotSupported ProviderCancellationSemantics = "NOT_SUPPORTED"
	ProviderCancellationCooperative  ProviderCancellationSemantics = "COOPERATIVE"
	ProviderCancellationBounded      ProviderCancellationSemantics = "BOUNDED"
)

// ProviderDeviceClass identifies a direct device dependency without exposing
// adapter-specific paths, handles, or vendor identifiers.
type ProviderDeviceClass string

const (
	ProviderDeviceCamera               ProviderDeviceClass = "CAMERA"
	ProviderDeviceMicrophone           ProviderDeviceClass = "MICROPHONE"
	ProviderDeviceDisplay              ProviderDeviceClass = "DISPLAY"
	ProviderDeviceAudioOutput          ProviderDeviceClass = "AUDIO_OUTPUT"
	ProviderDeviceEmbodimentController ProviderDeviceClass = "EMBODIMENT_CONTROLLER"
)

// ProviderOperationalProfile is a conservative envelope across all
// capabilities declared by one provider. Registry boundaries reject an
// undeclared or incomplete profile.
type ProviderOperationalProfile struct {
	PrivacyClass          ProviderPrivacyClass
	MaximumLatency        time.Duration
	CancellationSemantics ProviderCancellationSemantics
	DeviceRequirements    []ProviderDeviceClass
}

// ProviderSnapshot is an immutable view of one deployed provider instance.
type ProviderSnapshot struct {
	ProviderID            string
	InstanceID            string
	ProtocolVersion       string
	ImplementationVersion string
	Capabilities          []CapabilityKind
	Health                ProviderHealth
	LeaseExpiresAt        time.Time
	OperationalProfile    ProviderOperationalProfile
}

// BiometricPolicySnapshot is the aggregate authorization view used by Stage
// C1 readiness. Per-subject authorization and enrollment storage is external.
type BiometricPolicySnapshot struct {
	Enabled    bool
	Authorized []CapabilityKind
	Enrolled   []CapabilityKind
}

// ActivationStatus is the scenario readiness result.
type ActivationStatus string

const (
	Ready    ActivationStatus = "READY"
	Degraded ActivationStatus = "DEGRADED"
	Blocked  ActivationStatus = "BLOCKED"
)

// Issue explains one unavailable or policy-blocked selected capability.
type Issue struct {
	Capability CapabilityKind
	ProviderID string
	Code       fault.Code
	Fallback   Fallback
}

// Activation is the deterministic result of readiness evaluation.
type Activation struct {
	Status ActivationStatus
	Issues []Issue
}
