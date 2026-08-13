package presenter

import (
	"errors"
	"fmt"
	"sort"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
)

// View is the complete safe-to-render desktop state.
type View struct {
	Revision          uint64
	Avatar            Avatar
	Permissions       []Permission
	Providers         []Provider
	CameraEnabled     bool
	MicrophoneEnabled bool
	BiometricsEnabled bool
}

type Avatar struct {
	Mode     string
	CSSClass string
	Text     string
	Speaking bool
}

type Permission struct {
	ID      string
	Label   string
	Wire    platformv1.DesktopPermission
	Enabled bool
}

type Provider struct {
	ID     string
	State  string
	Reason string
}

// Build validates and maps one complete typed snapshot.
func Build(state *platformv1.DesktopState) (View, error) {
	if state == nil || state.GetRevision() == 0 || state.GetAvatar() == nil {
		return View{}, errors.New("desktop state, positive revision, and avatar are required")
	}
	avatar, err := mapAvatar(state.GetAvatar())
	if err != nil {
		return View{}, err
	}
	permissions, err := mapPermissions(state.GetPermissions())
	if err != nil {
		return View{}, err
	}
	providers, err := mapProviders(state.GetProviders())
	if err != nil {
		return View{}, err
	}
	view := View{Revision: state.GetRevision(), Avatar: avatar, Permissions: permissions, Providers: providers}
	for _, permission := range permissions {
		switch permission.Wire {
		case platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE:
			view.CameraEnabled = permission.Enabled
		case platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE:
			view.MicrophoneEnabled = permission.Enabled
		case platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION,
			platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION,
			platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS,
			platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION,
			platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION:
			view.BiometricsEnabled = view.BiometricsEnabled || permission.Enabled
		}
	}
	return view, nil
}

func mapAvatar(input *platformv1.AvatarSnapshot) (Avatar, error) {
	view := Avatar{Text: input.GetText(), Speaking: input.GetSpeaking()}
	switch input.GetMode() {
	case platformv1.AvatarMode_AVATAR_MODE_IDLE:
		view.Mode, view.CSSClass = "空闲", "idle"
	case platformv1.AvatarMode_AVATAR_MODE_ATTENDING:
		view.Mode, view.CSSClass = "正在关注", "attending"
	case platformv1.AvatarMode_AVATAR_MODE_ACKNOWLEDGING:
		view.Mode, view.CSSClass = "正在回应", "acknowledging"
	case platformv1.AvatarMode_AVATAR_MODE_EXPRESSING:
		view.Mode, view.CSSClass = "正在表达", "expressing"
	case platformv1.AvatarMode_AVATAR_MODE_SPEAKING:
		view.Mode, view.CSSClass = "正在说话", "speaking"
	default:
		return Avatar{}, fmt.Errorf("unknown avatar mode %d", input.GetMode())
	}
	if input.GetSpeaking() && input.GetMode() != platformv1.AvatarMode_AVATAR_MODE_SPEAKING {
		return Avatar{}, errors.New("speaking flag requires SPEAKING mode")
	}
	return view, nil
}

func mapPermissions(input []*platformv1.PermissionGrant) ([]Permission, error) {
	definitions := permissionDefinitions()
	if len(input) != len(definitions) {
		return nil, errors.New("permission snapshot is incomplete")
	}
	byWire := make(map[platformv1.DesktopPermission]bool, len(input))
	for _, grant := range input {
		if grant == nil {
			return nil, errors.New("permission grant is required")
		}
		if _, known := definitions[grant.GetPermission()]; !known {
			return nil, fmt.Errorf("unknown permission %d", grant.GetPermission())
		}
		if _, exists := byWire[grant.GetPermission()]; exists {
			return nil, fmt.Errorf("duplicate permission %d", grant.GetPermission())
		}
		byWire[grant.GetPermission()] = grant.GetEnabled()
	}
	order := permissionOrder()
	output := make([]Permission, 0, len(order))
	for _, wire := range order {
		definition := definitions[wire]
		definition.Enabled = byWire[wire]
		output = append(output, definition)
	}
	return output, nil
}

func permissionOrder() []platformv1.DesktopPermission {
	return []platformv1.DesktopPermission{
		platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION,
	}
}

func permissionDefinitions() map[platformv1.DesktopPermission]Permission {
	return map[platformv1.DesktopPermission]Permission{
		platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE:         {ID: "camera-capture", Label: "摄像头采集", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE:     {ID: "microphone-capture", Label: "麦克风采集", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION:         {ID: "face-detection", Label: "人脸检测", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION:    {ID: "face-identification", Label: "人脸识别", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS:          {ID: "face-liveness", Label: "人脸活体", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION: {ID: "speaker-identification", Label: "声纹识别", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION:   {ID: "speaker-verification", Label: "声纹验证", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION},
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION:   {ID: "speech-transcription", Label: "语音转写", Wire: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION},
	}
}

func mapProviders(input []*platformv1.ProviderRuntime) ([]Provider, error) {
	output := make([]Provider, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, provider := range input {
		if provider == nil || provider.GetProviderId() == "" {
			return nil, errors.New("provider id is required")
		}
		if _, exists := seen[provider.GetProviderId()]; exists {
			return nil, fmt.Errorf("duplicate provider %q", provider.GetProviderId())
		}
		seen[provider.GetProviderId()] = struct{}{}
		state, err := providerState(provider.GetState())
		if err != nil {
			return nil, err
		}
		reason, err := providerReason(provider.GetReason())
		if err != nil {
			return nil, err
		}
		output = append(output, Provider{ID: provider.GetProviderId(), State: state, Reason: reason})
	}
	sort.Slice(output, func(i, j int) bool { return output[i].ID < output[j].ID })
	return output, nil
}

func providerState(input platformv1.ProviderRuntimeState) (string, error) {
	switch input {
	case platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DISABLED:
		return "已关闭", nil
	case platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_STARTING:
		return "启动中", nil
	case platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_RUNNING:
		return "运行中", nil
	case platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DEGRADED:
		return "已降级", nil
	case platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_STOPPING:
		return "停止中", nil
	default:
		return "", fmt.Errorf("unknown provider state %d", input)
	}
}

func providerReason(input platformv1.ProviderRuntimeReason) (string, error) {
	switch input {
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_NONE:
		return "正常", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DISABLED_BY_USER:
		return "用户已关闭", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_PERMISSION_DENIED:
		return "权限被拒绝", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DEVICE_UNAVAILABLE:
		return "设备不可用", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DEPENDENCY_UNAVAILABLE:
		return "依赖不可用", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_MODEL_UNAVAILABLE:
		return "模型不可用", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_INTERNAL_ERROR:
		return "内部错误", nil
	case platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_SHUTTING_DOWN:
		return "正在关闭", nil
	default:
		return "", fmt.Errorf("unknown provider reason %d", input)
	}
}
