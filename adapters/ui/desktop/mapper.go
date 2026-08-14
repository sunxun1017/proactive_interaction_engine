package desktop

import (
	"errors"
	"sort"
	"strings"
	"time"

	"proactive-interaction-engine/adapters/embodiment/webavatar"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/runtime/provider"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func avatarToWire(input webavatar.Update) (*platformv1.AvatarSnapshot, error) {
	mode, err := avatarModeToWire(input.Mode)
	if err != nil {
		return nil, err
	}
	occurredAt, err := requiredTimestamp(input.OccurredAt)
	if err != nil {
		return nil, err
	}
	if input.Speaking && input.Mode != webavatar.ModeSpeaking {
		return nil, errors.New("speaking avatar must be in SPEAKING mode")
	}
	return &platformv1.AvatarSnapshot{
		Mode:       mode,
		Text:       input.Text,
		Speaking:   input.Speaking,
		ActionId:   input.ActionID,
		OccurredAt: occurredAt,
	}, nil
}

func avatarModeToWire(input webavatar.Mode) (platformv1.AvatarMode, error) {
	switch input {
	case webavatar.ModeIdle:
		return platformv1.AvatarMode_AVATAR_MODE_IDLE, nil
	case webavatar.ModeAttending:
		return platformv1.AvatarMode_AVATAR_MODE_ATTENDING, nil
	case webavatar.ModeAcknowledging:
		return platformv1.AvatarMode_AVATAR_MODE_ACKNOWLEDGING, nil
	case webavatar.ModeExpressing:
		return platformv1.AvatarMode_AVATAR_MODE_EXPRESSING, nil
	case webavatar.ModeSpeaking:
		return platformv1.AvatarMode_AVATAR_MODE_SPEAKING, nil
	default:
		return platformv1.AvatarMode_AVATAR_MODE_UNSPECIFIED, errors.New("unknown avatar mode")
	}
}

func permissionsToWire(snapshot privacy.Snapshot) ([]*platformv1.PermissionGrant, error) {
	if snapshot.Revision == 0 {
		for _, grant := range snapshot.Grants {
			if grant.Enabled || !grant.UpdatedAt.IsZero() {
				return nil, errors.New("revision zero contains changed permission")
			}
		}
	}
	seen := make(map[privacy.Permission]struct{}, len(snapshot.Grants))
	output := make([]*platformv1.PermissionGrant, 0, len(snapshot.Grants))
	for _, grant := range snapshot.Grants {
		permission, err := permissionToWire(grant.Permission)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[grant.Permission]; exists {
			return nil, errors.New("duplicate permission grant")
		}
		seen[grant.Permission] = struct{}{}
		if grant.Enabled && grant.UpdatedAt.IsZero() {
			return nil, errors.New("enabled permission has no update time")
		}
		updatedAt, err := optionalTimestamp(grant.UpdatedAt)
		if err != nil {
			return nil, err
		}
		output = append(output, &platformv1.PermissionGrant{
			Permission: permission,
			Enabled:    grant.Enabled,
			UpdatedAt:  updatedAt,
		})
	}
	if len(seen) != len(privacy.AllPermissions()) {
		return nil, errors.New("permission snapshot is incomplete")
	}
	sort.Slice(output, func(left, right int) bool {
		return output[left].GetPermission() < output[right].GetPermission()
	})
	return output, nil
}

func permissionFromWire(input platformv1.DesktopPermission) (privacy.Permission, error) {
	switch input {
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE:
		return privacy.CameraCapture, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE:
		return privacy.MicrophoneCapture, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION:
		return privacy.FaceDetection, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION:
		return privacy.FaceIdentification, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS:
		return privacy.FaceLiveness, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION:
		return privacy.SpeakerIdentification, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION:
		return privacy.SpeakerVerification, nil
	case platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION:
		return privacy.SpeechTranscription, nil
	default:
		return "", errors.New("unknown desktop permission")
	}
}

func permissionToWire(input privacy.Permission) (platformv1.DesktopPermission, error) {
	switch input {
	case privacy.CameraCapture:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE, nil
	case privacy.MicrophoneCapture:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE, nil
	case privacy.FaceDetection:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION, nil
	case privacy.FaceIdentification:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION, nil
	case privacy.FaceLiveness:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS, nil
	case privacy.SpeakerIdentification:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION, nil
	case privacy.SpeakerVerification:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION, nil
	case privacy.SpeechTranscription:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION, nil
	default:
		return platformv1.DesktopPermission_DESKTOP_PERMISSION_UNSPECIFIED, errors.New("unknown privacy permission")
	}
}

func providersToWire(snapshot provider.Snapshot) ([]*platformv1.ProviderRuntime, error) {
	seen := make(map[string]struct{}, len(snapshot.Providers))
	output := make([]*platformv1.ProviderRuntime, 0, len(snapshot.Providers))
	for _, runtime := range snapshot.Providers {
		if strings.TrimSpace(runtime.ProviderID) == "" || runtime.ProviderID != strings.TrimSpace(runtime.ProviderID) {
			return nil, errors.New("provider id is required")
		}
		if _, exists := seen[runtime.ProviderID]; exists {
			return nil, errors.New("duplicate provider runtime")
		}
		seen[runtime.ProviderID] = struct{}{}
		state, err := providerStateToWire(runtime.State)
		if err != nil {
			return nil, err
		}
		reason, err := providerReasonToWire(runtime.Reason)
		if err != nil {
			return nil, err
		}
		if !validProviderStateReason(runtime.State, runtime.Reason) {
			return nil, errors.New("invalid provider state and reason")
		}
		updatedAt, err := requiredTimestamp(runtime.UpdatedAt)
		if err != nil {
			return nil, err
		}
		output = append(output, &platformv1.ProviderRuntime{
			ProviderId: runtime.ProviderID,
			State:      state,
			Reason:     reason,
			UpdatedAt:  updatedAt,
		})
	}
	sort.Slice(output, func(left, right int) bool {
		return output[left].GetProviderId() < output[right].GetProviderId()
	})
	return output, nil
}

func providerStateToWire(input provider.State) (platformv1.ProviderRuntimeState, error) {
	switch input {
	case provider.Disabled:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DISABLED, nil
	case provider.Starting:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_STARTING, nil
	case provider.Running:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_RUNNING, nil
	case provider.Degraded:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DEGRADED, nil
	case provider.Stopping:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_STOPPING, nil
	default:
		return platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_UNSPECIFIED, errors.New("unknown provider state")
	}
}

func providerReasonToWire(input provider.Reason) (platformv1.ProviderRuntimeReason, error) {
	switch input {
	case provider.ReasonNone:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_NONE, nil
	case provider.ReasonDisabledByUser:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DISABLED_BY_USER, nil
	case provider.ReasonPermissionDenied:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_PERMISSION_DENIED, nil
	case provider.ReasonDeviceUnavailable:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DEVICE_UNAVAILABLE, nil
	case provider.ReasonDependencyUnavailable:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DEPENDENCY_UNAVAILABLE, nil
	case provider.ReasonModelUnavailable:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_MODEL_UNAVAILABLE, nil
	case provider.ReasonInternalError:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_INTERNAL_ERROR, nil
	case provider.ReasonShuttingDown:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_SHUTTING_DOWN, nil
	default:
		return platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_UNSPECIFIED, errors.New("unknown provider reason")
	}
}

func validProviderStateReason(state provider.State, reason provider.Reason) bool {
	switch state {
	case provider.Disabled:
		return reason == provider.ReasonDisabledByUser
	case provider.Starting, provider.Running:
		return reason == provider.ReasonNone
	case provider.Degraded:
		switch reason {
		case provider.ReasonPermissionDenied,
			provider.ReasonDeviceUnavailable,
			provider.ReasonDependencyUnavailable,
			provider.ReasonModelUnavailable,
			provider.ReasonInternalError:
			return true
		default:
			return false
		}
	case provider.Stopping:
		return reason == provider.ReasonShuttingDown
	default:
		return false
	}
}

func requiredTimestamp(input time.Time) (*timestamppb.Timestamp, error) {
	if input.IsZero() {
		return nil, errors.New("timestamp is required")
	}
	value := timestamppb.New(input)
	if err := value.CheckValid(); err != nil {
		return nil, err
	}
	return value, nil
}

func optionalTimestamp(input time.Time) (*timestamppb.Timestamp, error) {
	if input.IsZero() {
		return nil, nil
	}
	return requiredTimestamp(input)
}
