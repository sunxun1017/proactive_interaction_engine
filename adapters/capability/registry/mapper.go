package registry

import (
	"sort"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/readiness"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type registrationDeclaration struct {
	providerID            string
	instanceID            string
	protocolVersion       string
	implementationVersion string
	capabilities          []readiness.CapabilityKind
	health                readiness.ProviderHealth
	healthReason          platformv1.ProviderHealthReason
}

func normalizeRegistration(request *platformv1.RegisterCapabilityProviderRequest) (registrationDeclaration, error) {
	if request == nil {
		return registrationDeclaration{}, status.Error(codes.InvalidArgument, "registration request is required")
	}
	if request.GetProviderId() == "" || request.GetInstanceId() == "" || request.GetProtocolVersion() == "" || request.GetImplementationVersion() == "" {
		return registrationDeclaration{}, status.Error(codes.InvalidArgument, "provider id, instance id, protocol version, and implementation version are required")
	}
	if request.GetProtocolVersion() != SupportedProtocolVersion {
		return registrationDeclaration{}, status.Error(codes.FailedPrecondition, "unsupported capability provider protocol")
	}
	capabilities, err := normalizeCapabilities(request.GetCapabilities())
	if err != nil {
		return registrationDeclaration{}, err
	}
	health, err := normalizeHealth(request.GetHealth(), request.GetHealthReason())
	if err != nil {
		return registrationDeclaration{}, err
	}
	return registrationDeclaration{
		providerID:            request.GetProviderId(),
		instanceID:            request.GetInstanceId(),
		protocolVersion:       request.GetProtocolVersion(),
		implementationVersion: request.GetImplementationVersion(),
		capabilities:          capabilities,
		health:                health,
		healthReason:          request.GetHealthReason(),
	}, nil
}

func normalizeHeartbeat(request *platformv1.HeartbeatCapabilityProviderRequest) (string, readiness.ProviderHealth, error) {
	if request == nil || request.GetLeaseId() == "" {
		return "", "", status.Error(codes.InvalidArgument, "heartbeat lease id is required")
	}
	health, err := normalizeHealth(request.GetHealth(), request.GetHealthReason())
	if err != nil {
		return "", "", err
	}
	return request.GetLeaseId(), health, nil
}

func normalizeCapabilities(input []platformv1.ServiceCapabilityKind) ([]readiness.CapabilityKind, error) {
	if len(input) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one capability is required")
	}
	wire := append([]platformv1.ServiceCapabilityKind(nil), input...)
	sort.Slice(wire, func(i, j int) bool { return wire[i] < wire[j] })
	capabilities := make([]readiness.CapabilityKind, 0, len(wire))
	for index, capability := range wire {
		if index > 0 && capability == wire[index-1] {
			return nil, status.Error(codes.InvalidArgument, "capability is duplicated")
		}
		mapped, ok := mapCapability(capability)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "capability is unspecified or unknown")
		}
		capabilities = append(capabilities, mapped)
	}
	return capabilities, nil
}

func mapCapability(input platformv1.ServiceCapabilityKind) (readiness.CapabilityKind, bool) {
	switch input {
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE:
		return readiness.PersonPresence, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION:
		return readiness.FaceDetection, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION:
		return readiness.FaceIdentification, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS:
		return readiness.FaceLiveness, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY:
		return readiness.VoiceActivity, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION:
		return readiness.SpeakerIdentification, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION:
		return readiness.SpeakerVerification, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEECH_TRANSCRIPTION:
		return readiness.SpeechTranscription, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_ATTENTION_ESTIMATION:
		return readiness.AttentionEstimation, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_BUSY_STATE:
		return readiness.BusyState, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_GESTURE_DETECTION:
		return readiness.GestureDetection, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_DEVICE_STATE:
		return readiness.DeviceState, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_DISPLAY_TEXT:
		return readiness.DisplayText, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_AVATAR_ATTEND:
		return readiness.AvatarAttend, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_AVATAR_EXPRESSION:
		return readiness.AvatarExpression, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEECH_SYNTHESIS:
		return readiness.SpeechSynthesis, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_GESTURE:
		return readiness.Gesture, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_LIGHT:
		return readiness.Light, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_LOCOMOTION:
		return readiness.Locomotion, true
	default:
		return "", false
	}
}

func normalizeHealth(state platformv1.ProviderHealthState, reason platformv1.ProviderHealthReason) (readiness.ProviderHealth, error) {
	if !validHealthReason(reason) {
		return "", status.Error(codes.InvalidArgument, "health reason is unspecified or unknown")
	}
	switch state {
	case platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_HEALTHY:
		if reason != platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE {
			return "", status.Error(codes.InvalidArgument, "healthy provider must use NONE reason")
		}
		return readiness.Healthy, nil
	case platformv1.ProviderHealthState_PROVIDER_HEALTH_STATE_UNHEALTHY:
		if reason == platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE {
			return "", status.Error(codes.InvalidArgument, "unhealthy provider must use a failure reason")
		}
		return readiness.Unhealthy, nil
	default:
		return "", status.Error(codes.InvalidArgument, "health state is unspecified or unknown")
	}
}

func validHealthReason(reason platformv1.ProviderHealthReason) bool {
	switch reason {
	case platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_NONE,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_STARTING,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEVICE_UNAVAILABLE,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_PERMISSION_DENIED,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_DEPENDENCY_UNAVAILABLE,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_MODEL_UNAVAILABLE,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_INTERNAL_ERROR,
		platformv1.ProviderHealthReason_PROVIDER_HEALTH_REASON_SHUTTING_DOWN:
		return true
	default:
		return false
	}
}
